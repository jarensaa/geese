//go:build integration

package goose

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jarensaa/geese"
	"github.com/jarensaa/geese/geesetest"
)

func TestMain(m *testing.M) { geesetest.Main(m) }

var migrations = Migrations{FS: os.DirFS("testdata"), Dir: "migrations"}

var plan = geese.FlightPlan{MaxActiveVersions: 4, MinimumSupportedVersion: 1, LatestMigrationVersion: 2, MinimumServeVersion: 1, MaximumServeVersion: 2}

// Outside the flock, Migrate initializes an empty database, and takes a
// database in use only offline, with no goose flying.
func TestMigrateOutsideTheFlock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, empty := geesetest.Start(t, ctx)
	if err := Migrate(ctx, empty, migrations, plan, false); err != nil {
		t.Fatalf("initializing an empty database: %v", err)
	}
	if err := Migrate(ctx, empty, migrations, plan, false); err != nil {
		t.Fatalf("a current database: %v", err)
	}

	conn, url := geesetest.Start(t, ctx)
	if err := migrations.MigrateTo(ctx, conn, 1); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, url, migrations, plan, false); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("migrated a database in use outside its flock: %v", err)
	}
	flock, err := geese.Join(ctx, geese.Config{DatabaseURL: url, Name: "flying", Window: plan.Window(), Weather: geese.ClearSkies{}, Migrator: migrations})
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, url, migrations, plan, true); err == nil || !strings.Contains(err.Error(), "flying") {
		t.Fatalf("migrated offline with a goose flying: %v", err)
	}
	flock.Land()
	if err := Migrate(ctx, url, migrations, plan, true); err != nil {
		t.Fatalf("migrating offline once nothing flies: %v", err)
	}

	// A database newer than the plan knows is refused.
	if err := Migrate(ctx, url, migrations, geese.FlightPlan{MaxActiveVersions: 4, MinimumSupportedVersion: 1, LatestMigrationVersion: 1, MinimumServeVersion: 1, MaximumServeVersion: 2}, true); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("migrated a database newer than the plan: %v", err)
	}
}

// In the flock, goose's ledger is the schema version, and the leader migrates
// one version at a time.
func TestTheFlockMigratesWithGoose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, url := geesetest.Start(t, ctx)
	if err := migrations.MigrateTo(ctx, conn, 1); err != nil {
		t.Fatal(err)
	}
	flock, err := geese.Join(ctx, geese.Config{DatabaseURL: url, Name: "leader", Window: plan.Window(), Weather: geese.ClearSkies{}, Migrator: migrations})
	if err != nil {
		t.Fatal(err)
	}
	defer flock.Land()
	if flock.Version() != 1 {
		t.Fatalf("joined at %d", flock.Version())
	}
	if err := flock.TryMigrate(ctx); err != nil {
		t.Fatal(err)
	}
	if flock.Version() != 2 {
		t.Fatalf("migrated to %d", flock.Version())
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO lists VALUES (1,'groceries'); INSERT INTO items VALUES (1,1,'eggs')`); err != nil {
		t.Fatalf("the schema at 2: %v", err)
	}
}

// Two leaders may migrate at once, as when the first one's lock went with its
// connection while it migrated: each migration is applied once, and both see
// it done.
func TestMigratingTwiceAtOnceAppliesOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, _ := geesetest.Start(t, ctx)
	migrated := make(chan error, 4)
	for range cap(migrated) {
		go func() { migrated <- migrations.MigrateTo(ctx, conn, 2) }()
	}
	for range cap(migrated) {
		if err := <-migrated; err != nil {
			t.Fatalf("migrating alongside another leader: %v", err)
		}
	}
	var applied int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM goose_db_version WHERE version_id > 0`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != 2 {
		t.Fatalf("applied %d migrations, expected 2", applied)
	}
}
