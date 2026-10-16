package geese

import (
	"strings"
	"testing"
)

func TestFlightPlans(t *testing.T) {
	plan, err := ParseFlightPlan([]byte(`{"maxActiveVersions":4,"minimumSupportedVersion":21,"latestMigrationVersion":22,"minimumServeVersion":21}`))
	if err != nil {
		t.Fatal(err)
	}
	if window := plan.Window(); window != (Window{ServeFrom: 21, Latest: 22}) {
		t.Fatalf("window: %+v", window)
	}
	for version, migratable := range map[int64]bool{0: true, 20: false, 21: true, 22: true, 23: false} {
		if err := plan.CheckMigratable(version); (err == nil) != migratable {
			t.Errorf("version %d: %v", version, err)
		}
	}
	for want, broken := range map[string]FlightPlan{
		"maxActiveVersions":  {MaxActiveVersions: 5, MinimumSupportedVersion: 1, LatestMigrationVersion: 1, MinimumServeVersion: 1},
		"at most latest":     {MaxActiveVersions: 4, MinimumSupportedVersion: 1, LatestMigrationVersion: 3, MinimumServeVersion: 4},
		"at most 4 versions": {MaxActiveVersions: 4, MinimumSupportedVersion: 1, LatestMigrationVersion: 5, MinimumServeVersion: 1},
		"minimumServe":       {MaxActiveVersions: 4, MinimumSupportedVersion: 3, LatestMigrationVersion: 4, MinimumServeVersion: 2},
	} {
		if err := broken.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: %v, want %q", broken, err, want)
		}
	}
	if _, err := ParseFlightPlan([]byte(`{`)); err == nil {
		t.Error("a broken file parsed")
	}
	if _, err := ParseFlightPlan([]byte(`{"maxActiveVersions":4,"minimumSupportedVersion":1,"latestMigrationVersion":1,"minimumServeVersion":1,"unsued":["lists.archived"]}`)); err == nil || !strings.Contains(err.Error(), "unsued") {
		t.Errorf("a misspelt key parsed: %v", err)
	}
}
