// Package goosetest checks goose migrations in a flock: that a rollup of
// migrations into a baseline changes no schema.
package goosetest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	pressly "github.com/pressly/goose/v3"

	"github.com/jarensaa/geese"
	"github.com/jarensaa/geese/geesetest"
	"github.com/jarensaa/geese/goose"
)

// Rollup is a branch that rewrites migrations main has: a rollup of them into
// a baseline, which may change no schema.
type Rollup struct {
	// Main is main's migrations directory, as deployed databases ran it.
	Main fs.FS
	// Branch is the branch's migrations, its plan, and what its image serves.
	Branch       goose.Migrations
	Plan         geese.FlightPlan
	Capabilities []geese.Capability
	Retired      []string
}

// RollupKeepsSchema proves a branch's rewritten migrations change no schema.
// Migrations on main have run against real databases, so a pull request may
// rewrite them only as a rollup into a baseline does. The branch must end on
// main's last version, build an identical PostgreSQL catalog (tables, columns,
// indexes, constraints, triggers, functions, policies, enums and sequences),
// take a database main migrated as current with what main's geese switched on
// still accepted, refuse a database older than its baseline, and refuse to be
// rolled back. A drift probe proves the comparison notices a difference.
//
// Run it in CI whenever a pull request deletes, edits or renames a migration
// main has, with main's migrations from the merge base.
func RollupKeepsSchema(t *testing.T, r Rollup) {
	t.Helper()
	mainVersions := migrationVersions(t, r.Main)
	latest := mainVersions[len(mainVersions)-1]
	if r.Plan.LatestMigrationVersion != latest {
		t.Fatalf("rewriting main's migrations must not change the schema: main ends at version %d and this branch at %d; add new migrations in a pull request of their own", latest, r.Plan.LatestMigrationVersion)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// The database main's migrations deploy, and the branch's.
	deployed, url := geesetest.Start(t, ctx)
	rolled, _ := geesetest.Start(t, ctx)
	mainMigrations := goose.Migrations{FS: r.Main, Dir: ".", Table: r.Branch.Table}
	if err := mainMigrations.MigrateTo(ctx, deployed, latest); err != nil {
		t.Fatal(err)
	}
	if err := r.Branch.MigrateTo(ctx, rolled, latest); err != nil {
		t.Fatal(err)
	}
	label := fmt.Sprintf("version %d", latest)
	exclude := excluded(r.Branch)
	AssertSameSchema(t, ctx, deployed, rolled, label, exclude...)
	// Prove the catalog probe itself notices drift before trusting its equality.
	if _, err := rolled.ExecContext(ctx, `CREATE TABLE geese_parity_probe (id integer)`); err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(catalog(t, ctx, deployed, exclude), catalog(t, ctx, rolled, exclude)) {
		t.Fatal("the catalog comparison missed an added table")
	}
	if _, err := rolled.ExecContext(ctx, `DROP TABLE geese_parity_probe`); err != nil {
		t.Fatal(err)
	}

	// A baseline has nothing to return to: rolling it back must fail and change nothing.
	sub, err := fs.Sub(r.Branch.FS, r.Branch.Dir)
	if err != nil {
		t.Fatal(err)
	}
	table := r.Branch.Table
	if table == "" {
		table = goose.DefaultTable
	}
	provider, err := pressly.NewProvider(pressly.DialectPostgres, rolled, sub, pressly.WithTableName(table), pressly.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(ctx); err == nil {
		t.Fatal("rolling back the baseline succeeded; its down section must refuse")
	}
	if version, err := provider.GetDBVersion(ctx); err != nil || version != latest {
		t.Fatalf("a refused rollback moved the ledger to %d: %v", version, err)
	}
	AssertSameSchema(t, ctx, deployed, rolled, label+" after a refused rollback", exclude...)

	// A database main migrated is current: nothing to apply.
	if err := goose.Migrate(ctx, url, r.Branch, r.Plan, false); err != nil {
		t.Fatalf("a database main migrated to %d should be current: %v", latest, err)
	}
	// Main's geese have switched on main's capabilities, those the baseline
	// retires included; they stay named active while this image joins.
	var mains []geese.Capability
	for _, name := range r.Retired {
		mains = append(mains, geese.Capability{Name: name, MinimumSchema: latest})
	}
	old, err := geese.Join(ctx, geese.Config{DatabaseURL: url, Name: "main", Window: r.Plan.Window(), Weather: geese.ClearSkies{}, Migrator: r.Branch,
		Capabilities: append(mains, r.Capabilities...)})
	if err != nil {
		t.Fatal(err)
	}
	defer old.Land()
	if err := old.TryActivate(ctx); err != nil {
		t.Fatal(err)
	}
	branch, err := geese.Join(ctx, geese.Config{DatabaseURL: url, Name: "branch", Window: r.Plan.Window(), Weather: geese.ClearSkies{}, Migrator: r.Branch,
		Capabilities: r.Capabilities, Retired: r.Retired})
	if err != nil {
		t.Fatal(err)
	}
	defer branch.Land()
	if branch.Version() != latest || !branch.Ready() {
		t.Fatalf("the branch's goose didn't join at %s", label)
	}
	if err := branch.TryMigrate(ctx); err != nil {
		t.Fatal(err)
	}
	if branch.Version() != latest {
		t.Fatalf("expected to stay at %s, got %d", label, branch.Version())
	}
	if err := branch.TryActivate(ctx); err != nil {
		t.Fatal(err)
	}
	active, err := branch.ActiveCapabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{}
	for _, capability := range r.Capabilities {
		if capability.MinimumSchema <= latest {
			want = append(want, capability.Name)
		}
	}
	slices.Sort(active)
	slices.Sort(want)
	if !slices.Equal(active, want) {
		t.Fatalf("capabilities at %s: got %v, want %v", label, active, want)
	}

	// A database older than the new baseline must go through an earlier release first.
	if mainVersions[0] < r.Plan.MinimumSupportedVersion {
		older, olderURL := geesetest.Start(t, ctx)
		olderVersion := r.Plan.MinimumSupportedVersion - 1
		if err := mainMigrations.MigrateTo(ctx, older, olderVersion); err != nil {
			t.Fatal(err)
		}
		if refused, err := geese.Join(ctx, geese.Config{DatabaseURL: olderURL, Window: r.Plan.Window(), Weather: geese.ClearSkies{}, Migrator: r.Branch}); !errors.Is(err, geese.ErrSchemaUnserved) {
			if refused != nil {
				refused.Land()
			}
			t.Fatalf("version %d should not be servable: %v", olderVersion, err)
		}
		if err := goose.Migrate(ctx, olderURL, r.Branch, r.Plan, true); err == nil || !strings.Contains(err.Error(), "older release") {
			t.Fatalf("version %d should be refused by the migrator: %v", olderVersion, err)
		}
	}
}

// excluded is what isn't the application's schema: goose's ledger, the flock's
// tables, and those of the coordinator geese came from.
func excluded(m goose.Migrations) []string {
	table := m.Table
	if table == "" {
		table = goose.DefaultTable
	}
	return []string{table, "geese_flock", "geese_flight", "schema_instances", "schema_upgrade_state"}
}

// migrationVersions lists the SQL migration versions in a directory, in order.
func migrationVersions(t *testing.T, dir fs.FS) []int64 {
	t.Helper()
	entries, err := fs.ReadDir(dir, ".")
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`^(\d+)_[a-z][a-z0-9_]*\.sql$`)
	var versions []int64
	for _, entry := range entries {
		match := pattern.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		version, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	if len(versions) == 0 {
		t.Fatal("no migrations in main's directory")
	}
	slices.Sort(versions)
	return versions
}

// AssertSameSchema fails the test when two databases' public schemas differ,
// saying how. Tables named in exclude, and their sequences, are left out.
func AssertSameSchema(t *testing.T, ctx context.Context, a, b *sql.DB, label string, exclude ...string) {
	t.Helper()
	left, right := catalog(t, ctx, a, exclude), catalog(t, ctx, b, exclude)
	if reflect.DeepEqual(left, right) {
		return
	}
	keys, seen := []string{}, map[string]bool{}
	for _, side := range []map[string]string{left, right} {
		for key := range side {
			if !seen[key] {
				keys = append(keys, key)
				seen[key] = true
			}
		}
	}
	sort.Strings(keys)
	var differences []string
	for _, key := range keys {
		if left[key] != right[key] {
			differences = append(differences, fmt.Sprintf("%s\n first:  %s\n second: %s", key, left[key], right[key]))
		}
	}
	t.Fatalf("the schemas differ at %s:\n%s", label, strings.Join(differences, "\n"))
}

func catalog(t *testing.T, ctx context.Context, db *sql.DB, exclude []string) map[string]string {
	t.Helper()
	const query = `SELECT kind,name,definition FROM (
  SELECT 'relation' AS kind, c.relname::text AS name,
    jsonb_build_object('kind',c.relkind,'persistence',c.relpersistence,'rowSecurity',c.relrowsecurity,'forceRowSecurity',c.relforcerowsecurity,'view',CASE WHEN c.relkind IN ('v','m') THEN pg_get_viewdef(c.oid) ELSE NULL END)::text AS definition
  FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname='public' AND c.relkind IN ('r','p','v','m','f') AND NOT (c.relname::text = ANY($1))
  UNION ALL
  SELECT 'column', table_name||'.'||column_name,
    jsonb_build_object('type',data_type,'udt',udt_name,'nullable',is_nullable,'default',column_default,'identity',is_identity)::text AS definition
  FROM information_schema.columns WHERE table_schema='public' AND NOT (table_name::text = ANY($1))
  UNION ALL
  SELECT 'index', indexname, indexdef FROM pg_indexes WHERE schemaname='public' AND NOT (tablename::text = ANY($1))
  UNION ALL
  SELECT 'constraint', c.conrelid::regclass::text||'.'||c.conname||':'||pg_get_constraintdef(c.oid), pg_get_constraintdef(c.oid)
  FROM pg_constraint c JOIN pg_class r ON r.oid=c.conrelid JOIN pg_namespace n ON n.oid=r.relnamespace
  WHERE n.nspname='public' AND NOT (r.relname::text = ANY($1))
  UNION ALL
  SELECT 'trigger', r.relname||'.'||t.tgname, pg_get_triggerdef(t.oid)
  FROM pg_trigger t JOIN pg_class r ON r.oid=t.tgrelid JOIN pg_namespace n ON n.oid=r.relnamespace
  WHERE n.nspname='public' AND NOT t.tgisinternal AND NOT (r.relname::text = ANY($1))
  UNION ALL
  SELECT 'function', p.oid::regprocedure::text, pg_get_functiondef(p.oid)
  FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public'
  UNION ALL
  SELECT 'policy', schemaname||'.'||tablename||'.'||policyname,
    jsonb_build_object('command',cmd,'roles',roles,'permissive',permissive,'using',qual,'check',with_check)::text
  FROM pg_policies WHERE schemaname='public' AND NOT (tablename::text = ANY($1))
  UNION ALL
  SELECT 'enum', t.typname||'.'||e.enumlabel, e.enumsortorder::text
  FROM pg_enum e JOIN pg_type t ON t.oid=e.enumtypid JOIN pg_namespace n ON n.oid=t.typnamespace
  WHERE n.nspname='public'
  UNION ALL
  SELECT 'sequence', sequencename,
    jsonb_build_object('type',data_type,'start',start_value,'min',min_value,'max',max_value,'increment',increment_by,'cycle',cycle)::text
  FROM pg_sequences WHERE schemaname='public'
    AND NOT EXISTS (SELECT 1 FROM unnest($1::text[]) excluded WHERE sequencename LIKE replace(excluded,'_','\_')||'\_%')
) catalog ORDER BY kind,name`
	rows, err := db.QueryContext(ctx, query, exclude)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := map[string]string{}
	for rows.Next() {
		var kind, name, definition string
		if err := rows.Scan(&kind, &name, &definition); err != nil {
			t.Fatal(err)
		}
		result[kind+":"+name] = definition
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
