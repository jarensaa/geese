//go:build integration

package goosetest

import (
	"os"
	"testing"

	"github.com/jarensaa/geese"
	"github.com/jarensaa/geese/geesetest"
	"github.com/jarensaa/geese/goose"
)

func TestMain(m *testing.M) { geesetest.Main(m) }

// Migrations 1 to 3 rolled into a baseline at 3 keep the schema main's
// migrations built, with main's capabilities, one of them retired.
func TestARollupKeepsTheSchema(t *testing.T) {
	RollupKeepsSchema(t, Rollup{
		Main:         os.DirFS("testdata/main"),
		Branch:       goose.Migrations{FS: os.DirFS("testdata"), Dir: "branch"},
		Plan:         geese.FlightPlan{MaxActiveVersions: 4, MinimumSupportedVersion: 3, LatestMigrationVersion: 3, MinimumServeVersion: 3, MaximumServeVersion: 3},
		Capabilities: []geese.Capability{{Name: "doneItems", MinimumSchema: 3}},
		Retired:      []string{"lists"},
	})
}
