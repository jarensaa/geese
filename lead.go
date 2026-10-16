package geese

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// One goose leads at a time: whoever holds the leader's lock, and only in calm
// weather. The leader alone migrates the schema, switches capabilities on and
// raises the oldest page served. When it lands, or its lock's connection drops,
// another takes over.

// lead makes this goose the leader, once the weather is calm. It reports false
// when another goose leads or the weather isn't calm; release lets go.
func (f *Flock) lead(ctx context.Context) (release func(), leading bool, err error) {
	conn, err := f.db.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	var acquired bool
	if err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, leaderLock).Scan(&acquired); err != nil || !acquired {
		conn.Close()
		return nil, false, err
	}
	release = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, leaderLock)
		conn.Close()
	}
	if calm, err := f.weather.Calm(ctx); err != nil || !calm {
		release()
		return nil, false, err
	}
	return release, true, nil
}

// errMixedFlock is why a leader waits: a goose it can hear speaks another
// protocol, so it can't know what that goose would make of a decision.
var errMixedFlock = errors.New("geese: the flock speaks more than one protocol; waiting until it speaks only this one")

// unanimous refuses, in the leader's transaction, while any goose heard
// speaks another protocol than this one, or the coordinator geese came from is
// still flying here (its geese write schema_instances, and never this flock's
// tables). A new protocol joins and follows a flock of the one before; it
// leads only once the older geese have landed.
func (f *Flock) unanimous(ctx context.Context, tx *sql.Tx) error {
	var others int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM geese_flock WHERE heard_until > clock_timestamp() AND protocol <> $1`, protocol).Scan(&others); err != nil {
		return err
	}
	if others == 0 {
		var legacy sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT to_regclass('schema_instances')::text`).Scan(&legacy); err != nil {
			return err
		}
		if legacy.Valid {
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM schema_instances WHERE lease_until > clock_timestamp()`).Scan(&others); err != nil {
				return err
			}
		}
	}
	if others != 0 {
		return errMixedFlock
	}
	return nil
}

// TryMigrate migrates the schema at most one version, while the flock serves:
// when this goose leads, the weather is calm, every goose heard serves the
// next version and declares unused what it drops, and the migrator says it
// keeps the code already serving working. The heading it sets first fences
// out a goose that joins meanwhile and couldn't serve the migration. A
// migration that asked for downtime runs instead once every goose heard has
// roosted: stopped serving, with nothing running. A migration that fails
// changes nothing, and the next attempt waits (retryAfter).
func (f *Flock) TryMigrate(ctx context.Context) error {
	if f.Version() >= f.window.Latest && !f.headingSet.Load() {
		return nil
	}
	release, leading, err := f.lead(ctx)
	if err != nil || !leading {
		return err
	}
	defer release()

	// The last check of versions and geese, and the heading, are serialized with every honk.
	tx, current, err := f.coordinated(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var heading, failedFor sql.NullInt64
	var waiting bool
	if err = tx.QueryRowContext(ctx, `SELECT heading_for,failed_for,coalesce(retry_at > clock_timestamp(),false) FROM geese_flight WHERE id=true FOR UPDATE`).Scan(&heading, &failedFor, &waiting); err != nil {
		return err
	}
	// Arrived, by a leader that didn't live to say so: say so.
	if heading.Valid && current == heading.Int64 {
		if err = tx.Commit(); err != nil {
			return err
		}
		return f.finishMigration(ctx, current)
	}
	if current >= f.window.Latest {
		return nil
	}
	if err = f.unanimous(ctx, tx); err != nil {
		return err
	}
	target := current + 1
	if heading.Valid && heading.Int64 != target {
		return fmt.Errorf("geese: the flock is heading for %d, and the schema is at %d", heading.Int64, current)
	}
	// The last attempt at this version failed: the next one waits its turn.
	if failedFor.Valid && failedFor.Int64 == target && waiting {
		return nil
	}
	var unable int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM geese_flock WHERE heard_until > clock_timestamp() AND (serves_from > $1 OR serves_to < $1)`, target).Scan(&unable); err != nil {
		return err
	}
	if unable > 0 {
		return nil
	}
	change, err := f.migrator.Change(target)
	if err == nil && change.Disrupts != "" {
		err = fmt.Errorf("geese: migration %d would disrupt the flock, which serves throughout: %s", target, change.Disrupts)
	}
	if err != nil {
		return f.fail(ctx, tx, current, target, err)
	}
	// While the flock serves, a goose that may still use what the migration
	// drops holds it back, until it lands. With the flock down, nothing runs
	// the code before the migration, and each goose serves the one after.
	if !change.Downtime {
		var undeclared int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM geese_flock WHERE heard_until > clock_timestamp() AND NOT (unused @> $1::text[])`,
			append([]string{}, change.Drops...)).Scan(&undeclared); err != nil {
			return err
		}
		if undeclared > 0 {
			return nil
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE geese_flight SET heading_for=$1,dropping=$2::text[],roosting=$3 WHERE id=true`,
		target, append([]string{}, change.Drops...), change.Downtime); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if change.Downtime {
		slog.Info("geese: migrating, with the flock down", "target", target)
		if err = f.awaitRoosted(ctx, target); err == nil {
			err = f.migrator.MigrateTo(ctx, f.db, target)
		}
	} else {
		slog.Info("geese: migrating", "target", target)
		err = f.migrate(ctx, target, change)
	}
	if err == nil {
		return f.finishMigration(ctx, target)
	}
	// What failed is recorded, even when the leader's step was cancelled.
	failCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ownDeadline)
	defer cancel()
	failTx, version, txErr := f.coordinated(failCtx)
	if txErr != nil {
		return errors.Join(err, txErr)
	}
	defer failTx.Rollback()
	if version >= target {
		return nil
	}
	return f.fail(failCtx, failTx, current, target, fmt.Errorf("geese: migrate to %d: %w", target, err))
}

// migrate runs the migration to target while the flock serves, never holding
// up its queries: each lock is waited for lockTimeout at most, so nothing
// queues long behind one the migration hasn't got. A migration that didn't
// get a lock, being transactional, changed nothing, and tries again. One that
// builds an index concurrently holds up no one while it waits, and isn't held
// to the timeout.
func (f *Flock) migrate(ctx context.Context, target int64, change Change) error {
	if change.Concurrently {
		return f.migrator.MigrateTo(ctx, f.db, target)
	}
	config := f.dbConfig.Copy()
	config.RuntimeParams["lock_timeout"] = fmt.Sprintf("%dms", lockTimeout.Milliseconds())
	db := stdlib.OpenDB(*config)
	defer db.Close()
	var err error
	for range lockAttempts {
		if err = f.migrator.MigrateTo(ctx, db, target); err == nil || !lockNotAvailable(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(lockRetry/2 + rand.N(lockRetry)):
		}
	}
	return err
}

// awaitRoosted waits until every goose heard has roosted for target.
func (f *Flock) awaitRoosted(ctx context.Context, target int64) error {
	deadline := time.Now().Add(roostTimeout)
	for {
		var awake int
		if err := f.db.QueryRowContext(ctx, `SELECT count(*) FROM geese_flock WHERE heard_until > clock_timestamp()
			AND roosted_for IS DISTINCT FROM $1`, target).Scan(&awake); err != nil {
			return err
		}
		if awake == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("geese: %d geese didn't stop serving for migration %d", awake, target)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// lockNotAvailable says whether err is PostgreSQL giving up waiting for a lock.
func lockNotAvailable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}

// fail records, in tx, an attempt to migrate to target that didn't, clearing
// any heading, so the next attempt waits its turn, and says when that is.
func (f *Flock) fail(ctx context.Context, tx *sql.Tx, current, target int64, why error) error {
	var failedFor sql.NullInt64
	var failures int
	if err := tx.QueryRowContext(ctx, `SELECT failed_for,failures FROM geese_flight WHERE id=true FOR UPDATE`).Scan(&failedFor, &failures); err != nil {
		return errors.Join(why, err)
	}
	if !failedFor.Valid || failedFor.Int64 != target {
		failures = 0
	}
	failures++
	retry := retryIn(failures)
	if _, err := tx.ExecContext(ctx, `UPDATE geese_flight SET heading_for=NULL,dropping='{}',roosting=false,failed_for=$1,failures=$2,
		retry_at=clock_timestamp()+$3::interval,failure=$4 WHERE id=true`, target, failures, retry.String(), why.Error()); err != nil {
		return errors.Join(why, err)
	}
	if err := tx.Commit(); err != nil {
		return errors.Join(why, err)
	}
	return fmt.Errorf("%w (the flock goes on at %d, and tries again in %s)", why, current, retry)
}

// finishMigration says the flock has arrived, and writes the leg in its
// journey: whether it ran live, and what it dropped.
func (f *Flock) finishMigration(ctx context.Context, target int64) error {
	tx, version, err := f.coordinated(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if version != target {
		return fmt.Errorf("geese: the schema is at %d, expected %d", version, target)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO geese_journey (version,downtime,dropped)
		SELECT $1,roosting,dropping FROM geese_flight WHERE id=true AND heading_for=$1 ON CONFLICT (version) DO NOTHING`, target); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE geese_flight SET heading_for=NULL,dropping='{}',roosting=false,failed_for=NULL,failures=0,retry_at=NULL,failure=NULL
		WHERE id=true AND heading_for=$1`, target); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	f.version.Store(target)
	f.headingSet.Store(false)
	slog.Info("geese: migrated", "version", target)
	return nil
}

// TryActivate switches on each capability of this image whose schema is in
// place, whose prerequisite is ready, and which every goose heard serves, when
// this goose leads, the weather is calm and the flock isn't migrating.
func (f *Flock) TryActivate(ctx context.Context) error {
	if !f.Ready() {
		return nil
	}
	currentActive, err := f.ActiveCapabilities(ctx)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(f.capabilities, func(capability Capability) bool { return !slices.Contains(currentActive, capability.Name) }) {
		return nil
	}
	release, leading, err := f.lead(ctx)
	if err != nil || !leading {
		return err
	}
	defer release()

	tx, version, err := f.coordinated(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var heading sql.NullInt64
	var encoded string
	if err = tx.QueryRowContext(ctx, `SELECT heading_for,to_json(active_capabilities)::text FROM geese_flight WHERE id=true FOR UPDATE`).Scan(&heading, &encoded); err != nil {
		return err
	}
	active, err := decodeCapabilities(encoded)
	if err != nil {
		return err
	}
	if heading.Valid {
		return nil
	}
	if err = f.unanimous(ctx, tx); err != nil {
		return err
	}
	activated := []string{}
	for _, capability := range f.capabilities {
		if version < capability.MinimumSchema || slices.Contains(active, capability.Name) {
			continue
		}
		if capability.Ready != nil {
			ready, readyErr := capability.Ready(ctx, tx)
			if readyErr != nil {
				return fmt.Errorf("geese: check capability %s's prerequisite: %w", capability.Name, readyErr)
			}
			if !ready {
				continue
			}
		}
		var unable int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM geese_flock WHERE heard_until > clock_timestamp()
			AND NOT ($1 = ANY(capabilities))`, capability.Name).Scan(&unable); err != nil {
			return err
		}
		if unable != 0 {
			continue
		}
		if _, err = tx.ExecContext(ctx, `UPDATE geese_flight SET active_capabilities=array_append(active_capabilities,$1) WHERE id=true`, capability.Name); err != nil {
			return err
		}
		active = append(active, capability.Name)
		activated = append(activated, capability.Name)
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	for _, name := range activated {
		slog.Info("geese: capability activated", "capability", name)
	}
	return nil
}
