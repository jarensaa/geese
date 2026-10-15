package geese

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
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
		conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, leaderLock)
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

// TryMigrate migrates the schema at most one version, when this goose leads,
// the weather is calm, every goose heard serves the next version, and all of
// them have gathered. It sets the flock's heading first, which each goose
// hears on its next honk and gathers for; a flock that doesn't gather in time
// goes on as it was, and the leader tries again later.
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
	var heading sql.NullInt64
	if err = tx.QueryRowContext(ctx, `SELECT heading_for FROM geese_flight WHERE id=true FOR UPDATE`).Scan(&heading); err != nil {
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
	var unable int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM geese_flock WHERE heard_until > clock_timestamp() AND (serves_from > $1 OR serves_to < $1)`, target).Scan(&unable); err != nil {
		return err
	}
	if unable > 0 {
		return nil
	}
	if _, err = tx.ExecContext(ctx, `UPDATE geese_flight SET heading_for=$1,departed_at=clock_timestamp() WHERE id=true`, target); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if err = f.awaitGathered(ctx, target); err != nil {
		if goOnErr := f.goOn(context.WithoutCancel(ctx), target); goOnErr != nil {
			return errors.Join(err, goOnErr)
		}
		return err
	}
	if err = f.migrator.MigrateTo(ctx, f.db, target); err != nil {
		return fmt.Errorf("geese: migrate to %d: %w", target, err)
	}
	return f.finishMigration(ctx, target)
}

// awaitGathered waits until every goose heard has gathered for target.
func (f *Flock) awaitGathered(ctx context.Context, target int64) error {
	deadline := time.Now().Add(gatherTimeout)
	for {
		var straggling int
		if err := f.db.QueryRowContext(ctx, `SELECT count(*) FROM geese_flock WHERE heard_until > clock_timestamp()
			AND gathered_for IS DISTINCT FROM $1`, target).Scan(&straggling); err != nil {
			return err
		}
		if straggling == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("geese: %d geese didn't gather for schema %d", straggling, target)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// goOn lets the flock go on as it was when a migration couldn't start,
// unless it was applied meanwhile.
func (f *Flock) goOn(ctx context.Context, target int64) error {
	tx, version, err := f.coordinated(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if version >= target {
		return nil
	}
	if _, err = tx.ExecContext(ctx, `UPDATE geese_flight SET heading_for=NULL,departed_at=NULL WHERE id=true AND heading_for=$1`, target); err != nil {
		return err
	}
	return tx.Commit()
}

// finishMigration says the flock has arrived: held work resumes on each goose's next honk.
func (f *Flock) finishMigration(ctx context.Context, target int64) error {
	tx, version, err := f.coordinated(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if version != target {
		return fmt.Errorf("geese: the schema is at %d, expected %d", version, target)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE geese_flight SET heading_for=NULL,departed_at=NULL WHERE id=true AND heading_for=$1`, target); err != nil {
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
