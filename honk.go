package geese

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// The flock keeps two tables beside the application's own, outside its
// migrations (PROTOCOL.md):
//
//   - geese_flock: one row per goose, renewed with every honk, and counted only
//     while heard_until hasn't passed.
//   - geese_flight: one row, the flock's shared decisions: where it's heading,
//     what it has switched on, and the oldest page it serves.
//
// What geese write there, and what they may assume of each other, is the
// protocol. A goose writes the protocol it speaks with every honk, and leads
// only a flock that all speaks it (lead.go).
const protocol = 1

// Advisory locks, database-wide: one flock to a database. The coordination
// lock serializes the flock's decisions with every honk; the leader's lock
// says who leads. The keys spell "geesecoo" and "geeselea".
const (
	coordinationLock int64 = 0x6765657365636f6f
	leaderLock       int64 = 0x67656573656c6561
)

// bootstrap makes the flock's tables when they're missing. They're the
// flock's, not the application's, so they're kept outside its migrations; a
// later protocol adds to them the same way, never taking away what an older
// goose reads.
func (f *Flock) bootstrap(ctx context.Context) error {
	tx, version, err := f.coordinated(ctx)
	if err != nil {
		return fmt.Errorf("geese: read the schema version (initialize a fresh database before serving): %w", err)
	}
	defer tx.Rollback()
	if err := f.checkServeVersion(version); err != nil {
		return err
	}
	// gathered_for is the migration a goose has gathered for: nothing it
	// admitted runs until that migration is applied.
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS geese_flock (
		id            uuid        PRIMARY KEY,
		name          text        NOT NULL,
		protocol      integer     NOT NULL,
		serves_from   bigint      NOT NULL,
		serves_to     bigint      NOT NULL,
		capabilities  text[]      NOT NULL DEFAULT '{}',
		unused        text[]      NOT NULL DEFAULT '{}',
		release_major bigint,
		gathered_for  bigint,
		heard_until   timestamptz NOT NULL,
		joined_at     timestamptz NOT NULL DEFAULT clock_timestamp()
	)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS geese_flight (
		id                  boolean     PRIMARY KEY DEFAULT true CHECK (id),
		heading_for         bigint,
		departed_at         timestamptz,
		pausing             boolean     NOT NULL DEFAULT true,
		active_capabilities text[]      NOT NULL DEFAULT '{}',
		oldest_page_major   bigint,
		failed_for          bigint,
		failures            integer     NOT NULL DEFAULT 0,
		retry_at            timestamptz,
		failure             text
	)`); err != nil {
		return err
	}
	if err = adoptLegacy(ctx, tx); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO geese_flight (id) VALUES (true) ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	return tx.Commit()
}

// adoptLegacy carries over what the coordinator geese came from decided, the
// first time the flock flies in a database it served: the capabilities it
// switched on and the oldest page it served, so nothing that was on goes off
// while an application moves to geese. A database it's migrating isn't joined
// until it's done. Its pods never read the flock's tables: the leader waits
// for them to land (unanimous).
func adoptLegacy(ctx context.Context, tx *sql.Tx) error {
	var legacy sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('schema_upgrade_state')::text`).Scan(&legacy); err != nil || !legacy.Valid {
		return err
	}
	var target sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT target_version FROM schema_upgrade_state WHERE id=true`).Scan(&target); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if target.Valid {
		return fmt.Errorf("geese: the coordinator geese came from is migrating this database to %d; join once it's done", target.Int64)
	}
	// Its oldest page came later than its capabilities: a database may not have it.
	floor := "NULL::bigint"
	var hasFloor bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_name='schema_upgrade_state' AND column_name='oldest_page_major' AND table_schema=current_schema())`).Scan(&hasFloor); err != nil {
		return err
	}
	if hasFloor {
		floor = "oldest_page_major"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO geese_flight (id,active_capabilities,oldest_page_major)
		SELECT true,active_capabilities,`+floor+` FROM schema_upgrade_state WHERE id=true ON CONFLICT DO NOTHING`)
	return err
}

// coordinated begins a transaction holding the coordination lock, and reads
// the schema version applied.
func (f *Flock) coordinated(ctx context.Context) (*sql.Tx, int64, error) {
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	var version int64
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, coordinationLock); err == nil {
		version, err = f.migrator.Version(ctx, tx)
	}
	if err != nil {
		tx.Rollback()
		return nil, 0, err
	}
	return tx, version, nil
}

// Honk keeps this goose's place in the flock: it checks the goose can still
// serve the schema, where the flock is heading, what it has switched on and
// the oldest page it serves, gathers when the flock is about to migrate, and
// says so in its row. A goose that can't is grounded until a honk is heard.
// Fly honks on its own; Honk is exported for tests and reconciliation.
func (f *Flock) Honk(ctx context.Context) error {
	honked := time.Now()
	epoch := f.epoch()
	tx, version, err := f.coordinated(ctx)
	if err != nil {
		f.ground()
		return err
	}
	defer tx.Rollback()
	var heading, floor sql.NullInt64
	var encoded string
	var pausing bool
	if err = tx.QueryRowContext(ctx, `SELECT heading_for,pausing,to_json(active_capabilities)::text,oldest_page_major FROM geese_flight WHERE id=true`).Scan(&heading, &pausing, &encoded, &floor); err != nil {
		f.ground()
		return err
	}
	active, err := decodeCapabilities(encoded)
	if err != nil {
		f.ground()
		return err
	}
	// A goose that can't serve where the flock is heading stays on the ground,
	// so it never runs against a schema it doesn't know.
	if err = f.checkServeVersion(version); err == nil && heading.Valid {
		err = f.checkServeVersion(heading.Int64)
	}
	if err == nil {
		err = f.checkActiveCapabilities(active, version)
	}
	if err == nil {
		err = f.checkRelease(floor)
	}
	if err != nil {
		f.ground()
		return err
	}
	// A heading ahead of the applied schema is a migration about to run:
	// gather for it, unless the flock needn't pause for it.
	migrating := heading.Valid && version < heading.Int64 && pausing
	var gatheredFor sql.NullInt64
	if migrating && f.gatherFor(heading.Int64) {
		gatheredFor = heading
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO geese_flock (id,name,protocol,serves_from,serves_to,capabilities,unused,release_major,gathered_for,heard_until)
		VALUES ($1,$2,$3,$4,$5,$6::text[],$7::text[],$8,$9,clock_timestamp()+$10::interval)
		ON CONFLICT (id) DO UPDATE SET capabilities=EXCLUDED.capabilities,gathered_for=EXCLUDED.gathered_for,heard_until=EXCLUDED.heard_until`,
		f.id, f.name, protocol, f.window.ServeFrom, f.window.ServeTo, f.supportedNames(), f.unused, f.release, gatheredFor, leaseFor.String()); err != nil {
		f.ground()
		return err
	}
	if err = tx.Commit(); err != nil {
		f.ground()
		return err
	}
	// The version is stored before held work resumes, so it runs on the new schema's code.
	f.version.Store(version)
	f.headingSet.Store(heading.Valid)
	if !f.keepUntil(epoch, honked.Add(ownDeadline)) {
		return errGroundedMidHonk
	}
	if !migrating {
		f.endGathering()
	}
	return nil
}

// errGroundedMidHonk is why a honk didn't put its goose back in the air: the
// goose was grounded while it honked, so what the honk said, such as that the
// goose had gathered, may no longer be true. The next honk says it again.
var errGroundedMidHonk = errors.New("geese: grounded while honking")

// epoch is how many times this goose's place has changed: a honk keeps it in
// the air only if nothing else did since the honk began.
func (f *Flock) epoch() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.leaseEpoch
}

// keepUntil keeps this goose in the air until the deadline, measured from
// before its honk on this process's own monotonic clock, so a stalled honk
// grounds it before the flock counts it gone, whatever the database's clock
// says. It doesn't, and reports false, if the goose's place has changed since
// epoch.
func (f *Flock) keepUntil(epoch uint64, deadline time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.leaseEpoch != epoch {
		return false
	}
	f.leaseEpoch++
	kept := f.leaseEpoch
	if f.leaseTimer != nil {
		f.leaseTimer.Stop()
	}
	f.deadline.Store(&deadline)
	f.ready.Store(time.Now().Before(deadline))
	f.leaseTimer = time.AfterFunc(time.Until(deadline), func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.leaseEpoch == kept {
			f.groundLocked()
		}
	})
	return true
}

// Fly honks and leads until ctx ends: it keeps this goose's place in the
// flock, and, when it leads, migrates the schema one version at a time, switches capabilities on
// and raises the oldest page served. A migration that fails lets the flock go
// on at the schema applied, and is tried again later. A goose whose honk
// fails is grounded, and serves again once a honk is heard.
//
// When ctx ends, the goose stops honking and leading, but keeps serving what
// its last honk allows, a few seconds at most, so a process that's stopping
// can finish its work in flight; it's grounded once that runs out, or when it
// lands. A process ends Fly when it starts to stop, and lands when its work is
// done.
func (f *Flock) Fly(ctx context.Context) {
	go f.leadWhenCalm(ctx)
	for {
		interval := renewInterval
		if f.headingSet.Load() || f.gathering() {
			interval = gatheringInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		// A honk under way when ctx ends finishes: a goose cut off mid-honk
		// would be grounded, cancelling the work its process is finishing.
		honkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ownDeadline)
		if err := f.Honk(honkCtx); err != nil {
			slog.Error("geese: a honk went unheard", "error", err)
		}
		cancel()
	}
}

// leadWhenCalm tries, every few seconds, to lead the flock one step on.
func (f *Flock) leadWhenCalm(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !f.Ready() {
			continue
		}
		// A step is bounded by this goose staying in the air: grounding cancels
		// it. A wall clock would cancel a slow migration every time it ran.
		stepCtx, cancel := context.WithCancel(ctx)
		f.mu.Lock()
		if !f.Ready() {
			f.mu.Unlock()
			cancel()
			continue
		}
		f.leaderCancel = cancel
		f.mu.Unlock()
		if err := f.TryMigrate(stepCtx); err != nil {
			slog.Warn("geese: migration deferred", "error", err)
		} else if err := f.TryActivate(stepCtx); err != nil {
			slog.Warn("geese: capability activation deferred", "error", err)
		} else if err := f.TryRaiseFloor(stepCtx); err != nil {
			slog.Warn("geese: raising the oldest page served deferred", "error", err)
		}
		f.mu.Lock()
		f.leaderCancel = nil
		f.mu.Unlock()
		cancel()
	}
}
