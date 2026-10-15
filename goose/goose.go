// Package goose makes pressly/goose's SQL migrations part of a flock: the
// flock decides when to migrate, and goose migrates.
//
//	migrations := goose.Migrations{FS: embedded, Dir: "migrations"}
//	flock, err := geese.Join(ctx, geese.Config{..., Window: plan.Window(), Migrator: migrations})
//
// It uses goose's Provider, never its process-wide settings, so it can't be
// disturbed by, or disturb, other goose use in the process.
package goose

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	pressly "github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/jarensaa/geese"
)

// DefaultTable is goose's own ledger of the migrations it applied.
const DefaultTable = "goose_db_version"

// Migrations are SQL migrations in a directory of a file system, such as one
// embedded in the image. They're a geese.Migrator.
type Migrations struct {
	FS  fs.FS
	Dir string
	// Table is goose's ledger, DefaultTable when empty.
	Table string
}

func (m Migrations) table() string {
	if m.Table == "" {
		return DefaultTable
	}
	return m.Table
}

func (m Migrations) provider(db *sql.DB) (*pressly.Provider, error) {
	dir, err := fs.Sub(m.FS, m.Dir)
	if err != nil {
		return nil, err
	}
	// Migrations run under goose's session lock: a leader whose own lock went
	// with its connection may still be migrating when the next one starts, and
	// the second then finds the work done.
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, err
	}
	return pressly.NewProvider(pressly.DialectPostgres, db, dir, pressly.WithTableName(m.table()), pressly.WithDisableGlobalRegistry(true),
		pressly.WithSessionLocker(locker))
}

// Version reads the latest migration goose's ledger says is applied, in the
// flock's transaction, so it's read under the flock's lock. A database without
// the ledger is at 0.
func (m Migrations) Version(ctx context.Context, tx *sql.Tx) (int64, error) {
	var ledger sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, m.table()).Scan(&ledger); err != nil {
		return 0, err
	}
	if !ledger.Valid {
		return 0, nil
	}
	table := pgx.Identifier{m.table()}.Sanitize()
	var version int64
	err := tx.QueryRowContext(ctx, `SELECT version_id FROM `+table+` g WHERE is_applied AND NOT EXISTS
		(SELECT 1 FROM `+table+` newer WHERE newer.version_id=g.version_id AND newer.id>g.id)
		ORDER BY version_id DESC LIMIT 1`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return version, err
}

// MigrateTo applies the migrations up to and including target.
func (m Migrations) MigrateTo(ctx context.Context, db *sql.DB, target int64) error {
	provider, err := m.provider(db)
	if err != nil {
		return err
	}
	_, err = provider.UpTo(ctx, target)
	return err
}

// version reads the ledger outside the flock, for Migrate.
func (m Migrations) version(ctx context.Context, db *sql.DB) (int64, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	return m.Version(ctx, tx)
}

// Offline says a database already in use may be migrated outside the flock:
// only by an operator who has stopped everything that uses it. Without it,
// Migrate initializes only an empty database.
type Offline bool

// Migrate migrates a database outside the flock, to the plan's last
// migration: to initialize an empty one before the first goose joins, or, with
// Offline, to recover one nothing uses. It's never the way a rollout migrates:
// the flock does that, live. It refuses a database older than the plan's
// baseline or newer than its last migration, and, while any goose is heard in
// the database's flock, refuses offline too.
func Migrate(ctx context.Context, databaseURL string, migrations Migrations, plan geese.FlightPlan, offline Offline) error {
	if err := plan.Validate(); err != nil {
		return err
	}
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return fmt.Errorf("goose: parse the database URL: %w", err)
	}
	db := stdlib.OpenDB(*config)
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("goose: connect to migrate: %w", err)
	}
	version, err := migrations.version(ctx, db)
	if err != nil {
		return fmt.Errorf("goose: read the schema version: %w", err)
	}
	if err := plan.CheckMigratable(version); err != nil {
		return err
	}
	if version > 0 && version < plan.LatestMigrationVersion {
		if !offline {
			return fmt.Errorf("goose: refusing to migrate a database in use, at schema %d, outside its flock; the flock migrates it live (only after stopping everything that uses it, migrate it offline)", version)
		}
		flying, err := geese.Flying(ctx, db)
		if err != nil {
			return err
		}
		if flying > 0 {
			return fmt.Errorf("goose: refusing to migrate offline while %d geese are flying", flying)
		}
	}
	provider, err := migrations.provider(db)
	if err != nil {
		return err
	}
	if _, err := provider.UpTo(ctx, plan.LatestMigrationVersion); err != nil {
		return fmt.Errorf("goose: migrate: %w", err)
	}
	if version, err = migrations.version(ctx, db); err != nil {
		return err
	}
	if version != plan.LatestMigrationVersion {
		return fmt.Errorf("goose: the database migrated to %d, expected %d", version, plan.LatestMigrationVersion)
	}
	return nil
}
