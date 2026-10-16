package check

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jarensaa/geese"
)

var layout = Layout{Migrations: "server/db/migrations", Code: "server/db/migrationcode", Source: "server", Tokens: "support"}

var plan = geese.FlightPlan{MaxActiveVersions: 4, MinimumSupportedVersion: 1, LatestMigrationVersion: 4, MinimumServeVersion: 4, MaximumServeVersion: 4}

func migrations(versions ...int) []string {
	paths := []string{}
	for _, version := range versions {
		paths = append(paths, fmt.Sprintf("server/db/migrations/%05d_change.sql", version))
	}
	return paths
}

func with(change func(*geese.FlightPlan)) geese.FlightPlan {
	p := plan
	change(&p)
	return p
}

func expect(t *testing.T, problems []string, want string) {
	t.Helper()
	if want == "" && len(problems) > 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if want != "" && !strings.Contains(strings.Join(problems, "\n"), want) {
		t.Fatalf("problems %v don't say %q", problems, want)
	}
}

func TestTheMigrationWindow(t *testing.T) {
	expect(t, Migrations(migrations(1, 2, 3, 4), plan, layout), "")
	expect(t, Migrations(migrations(1, 2, 3, 4, 5), with(func(p *geese.FlightPlan) {
		p.LatestMigrationVersion, p.MaximumServeVersion, p.MinimumServeVersion = 5, 5, 5
	}), layout), "exceed the 4-version cap")
	baseline := []string{"server/db/migrations/00004_baseline.sql", "server/db/migrations/00005_change.sql"}
	expect(t, Migrations(baseline, with(func(p *geese.FlightPlan) {
		p.MinimumSupportedVersion, p.LatestMigrationVersion, p.MinimumServeVersion, p.MaximumServeVersion = 4, 5, 5, 5
	}), layout), "")
	expect(t, Migrations(migrations(4, 5), with(func(p *geese.FlightPlan) {
		p.MinimumSupportedVersion, p.LatestMigrationVersion, p.MinimumServeVersion, p.MaximumServeVersion = 4, 5, 5, 5
	}), layout), "complete SQL baseline")
	expect(t, Migrations(append(migrations(1, 2, 3, 4), "server/db/migrationcode/00005_backfill.go"), plan, layout), "no executor")
	duplicate := append(migrations(1, 3), "server/db/migrations/00003_other.sql")
	problems := Migrations(duplicate, with(func(p *geese.FlightPlan) {
		p.LatestMigrationVersion, p.MinimumServeVersion, p.MaximumServeVersion = 3, 3, 3
	}), layout)
	expect(t, problems, "both")
	expect(t, problems, "consecutive")
	expect(t, Migrations(migrations(1, 2, 3), plan, layout), "latestMigrationVersion 4")
	expect(t, Migrations([]string{"server/db/migrations/notes.txt"}, plan, layout), "numbered SQL migration")
	// Other directories are none of its business.
	expect(t, Migrations(append(migrations(1, 2, 3, 4), "server/db/queries/lists.sql"), plan, layout), "")
}

func TestTheFlockMigratesOnlyInATransaction(t *testing.T) {
	const outside = "-- +goose NO TRANSACTION\n-- +goose Up\nCREATE INDEX CONCURRENTLY lists_name ON lists (name);\n"
	expect(t, Transactional(map[string]string{"server/db/migrations/00002_lists.sql": "-- +goose Up\nCREATE TABLE lists (id bigint);\n"}, plan), "")
	expect(t, Transactional(map[string]string{"server/db/migrations/00002_index.sql": outside}, plan), "00002_index.sql: runs outside a transaction")
	// The baseline is applied to an empty database, outside the flock.
	expect(t, Transactional(map[string]string{"server/db/migrations/00001_baseline.sql": outside}, plan), "")
}

func TestPauses(t *testing.T) {
	declares := with(func(p *geese.FlightPlan) { p.Unused = []string{"lists.archived"} })
	got := Pauses(map[string]string{
		"server/db/migrations/00001_baseline.sql": "-- +goose Up\nUPDATE lists SET name = 'x';",
		"server/db/migrations/00002_colour.sql":   "-- +goose Up\nALTER TABLE lists ADD COLUMN colour text;",
		"server/db/migrations/00003_archive.sql":  "-- +goose Up\nALTER TABLE lists DROP COLUMN archived;",
		"server/db/migrations/00004_invites.sql":  "-- +goose Up\nDROP TABLE invites;",
		"server/db/migrations/00005_rename.sql":   "-- +goose Up\nALTER TABLE lists RENAME name TO title;",
	}, declares)
	want := []string{
		"00002_colour.sql needs no pause",
		"00003_archive.sql needs no pause, once every image running declares lists.archived unused",
		"00004_invites.sql pauses the flock: the flight plan doesn't declare invites unused",
		`00005_rename.sql pauses the flock: geese can't tell code already serving keeps working through "ALTER TABLE lists RENAME name TO title"`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestCompatibilityFilesNameTheirToken(t *testing.T) {
	window := with(func(p *geese.FlightPlan) { p.MinimumServeVersion = 3 })
	expect(t, Compatibility(map[string]string{"server/app/compat_v0003_lists.go": "const _ = support.ServeV3"}, window, layout), "")
	expect(t, Compatibility(map[string]string{"server/app/compat_v0003.go": "package app"}, window, layout), "missing the compile-time support.ServeV3")
	expect(t, Compatibility(map[string]string{"server/app/compat_v0002.go": "const _ = support.ServeV2"}, window, layout), "outside the serving window")
}

func TestTokens(t *testing.T) {
	got := Tokens(with(func(p *geese.FlightPlan) { p.MinimumSupportedVersion, p.MinimumServeVersion = 3, 3 }), "support", "server/db/flightplan.json")
	want := "// Code generated by geese tokens from server/db/flightplan.json; DO NOT EDIT.\npackage support\n\nconst ServeV3 = true\nconst ServeV4 = true\nconst MigrateV3 = true\nconst MigrateV4 = true\n"
	if got != want {
		t.Fatalf("tokens:\n%s\nwant:\n%s", got, want)
	}
}
