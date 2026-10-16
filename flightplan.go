package geese

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
)

// FlightPlan is an application's migration window, kept in its repository
// (flightplan.json by convention) and read by the image and by the geese
// command alike:
//
//   - MaxActiveVersions: how many migrations may be active, a baseline
//     included, before they're rolled into a new baseline: at most four.
//   - MinimumSupportedVersion: the first active migration, the baseline. A
//     database older than it must be migrated by an older release first.
//   - LatestMigrationVersion: the last migration.
//   - MinimumServeVersion: the oldest schema version the image serves, up to
//     the latest, at most four. It may serve the ones before the latest, for
//     a rollout from an image that doesn't have the latest migration yet. A
//     rollback is the flock's business: an image serves a newer schema while
//     every migration since its latest ran live.
//   - Unused: the tables ("invites") and columns ("lists.archived") the
//     image's code no longer touches (Config.Unused). A migration that drops
//     them runs once every goose heard declares them.
type FlightPlan struct {
	MaxActiveVersions       int      `json:"maxActiveVersions"`
	MinimumSupportedVersion int64    `json:"minimumSupportedVersion"`
	LatestMigrationVersion  int64    `json:"latestMigrationVersion"`
	MinimumServeVersion     int64    `json:"minimumServeVersion"`
	Unused                  []string `json:"unused,omitempty"`
}

// unusedName is a table or a column, as table.column, either maybe in a schema.
var unusedName = regexp.MustCompile(`^[^\s.]+(\.[^\s.]+){0,2}$`)

// MaxActive is the most active migrations, and the widest serving window, a
// flight plan may have.
const MaxActive = 4

// ParseFlightPlan reads a flight plan and checks it holds together. A key it
// doesn't know is an error, so a misspelt one isn't silently ignored.
func ParseFlightPlan(data []byte) (FlightPlan, error) {
	var plan FlightPlan
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return plan, fmt.Errorf("geese: parse the flight plan: %w", err)
	}
	return plan, plan.Validate()
}

// ReadFlightPlan reads a flight plan from a file.
func ReadFlightPlan(path string) (FlightPlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return FlightPlan{}, err
	}
	return ParseFlightPlan(data)
}

// Validate checks the plan's numbers hold together, and its unused names are names.
func (p FlightPlan) Validate() error {
	switch {
	case p.MaxActiveVersions < 1 || p.MaxActiveVersions > MaxActive:
		return fmt.Errorf("geese: maxActiveVersions is 1 to %d", MaxActive)
	case p.MinimumSupportedVersion < 1:
		return fmt.Errorf("geese: minimumSupportedVersion is 1 or more")
	case p.LatestMigrationVersion < p.MinimumSupportedVersion:
		return fmt.Errorf("geese: latestMigrationVersion is at least minimumSupportedVersion")
	case p.MinimumServeVersion < p.MinimumSupportedVersion:
		return fmt.Errorf("geese: minimumServeVersion is at least minimumSupportedVersion")
	case p.MinimumServeVersion > p.LatestMigrationVersion:
		return fmt.Errorf("geese: minimumServeVersion is at most latestMigrationVersion")
	case p.LatestMigrationVersion-p.MinimumServeVersion+1 > MaxActive:
		return fmt.Errorf("geese: the serving window is at most %d versions", MaxActive)
	}
	for _, name := range p.Unused {
		if !unusedName.MatchString(name) {
			return fmt.Errorf("geese: unused %q isn't a table or table.column", name)
		}
	}
	return nil
}

// Window is what an image of this plan serves and migrates to.
func (p FlightPlan) Window() Window {
	return Window{ServeFrom: p.MinimumServeVersion, Latest: p.LatestMigrationVersion}
}

// CheckMigratable refuses a database this plan's migrations can't take on:
// one older than its baseline, which an older release must migrate first, or
// one newer than its last migration. 0 is an empty database.
func (p FlightPlan) CheckMigratable(version int64) error {
	if version != 0 && version < p.MinimumSupportedVersion {
		return fmt.Errorf("geese: database schema version %d is below this release's baseline %d; migrate it with an older release first", version, p.MinimumSupportedVersion)
	}
	if version > p.LatestMigrationVersion {
		return fmt.Errorf("geese: database schema version %d is newer than this release's last migration %d", version, p.LatestMigrationVersion)
	}
	return nil
}

// Flying counts the geese heard in a database's flock now, and those of the
// coordinator geese came from. Migrating outside the flock (an offline
// migration) waits until there are none.
func Flying(ctx context.Context, db *sql.DB) (int, error) {
	count := 0
	for _, table := range []struct{ name, heard string }{{"geese_flock", "heard_until"}, {"schema_instances", "lease_until"}} {
		var exists sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, table.name).Scan(&exists); err != nil {
			return 0, err
		}
		if !exists.Valid {
			continue
		}
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table.name+` WHERE `+table.heard+` > clock_timestamp()`).Scan(&n); err != nil {
			return 0, err
		}
		count += n
	}
	return count, nil
}
