// Package check holds the flight plan's rules over a repository's files, for
// the geese command.
package check

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jarensaa/geese"
	"github.com/jarensaa/geese/goose"
)

// Layout is where a repository keeps what the plan governs, as paths from its root.
type Layout struct {
	// Migrations is the directory of SQL migrations.
	Migrations string
	// Code is where Go data migrations would go; none is allowed yet, as
	// geese has no executor for them, and they mustn't count as applied.
	Code string
	// Source is where compatibility files (compat_vNNNN*.go) may be.
	Source string
	// Tokens is the package of the compile-time tokens compatibility files
	// name, as they import it.
	Tokens string
}

var versionedName = regexp.MustCompile(`^(\d+)_([a-z][a-z0-9_]*)\.(?:sql|go)$`)

// Migrations checks the active migrations against the plan: numbered,
// consecutive, at most MaxActiveVersions of them, starting at the plan's
// baseline (a complete SQL baseline unless it's the first migration ever), and
// ending at its last migration.
func Migrations(paths []string, plan geese.FlightPlan, layout Layout) []string {
	var problems []string
	if err := plan.Validate(); err != nil {
		problems = append(problems, err.Error())
	}
	versions := map[int64]string{}
	for _, path := range paths {
		dir, name := split(path)
		if dir == layout.Code {
			problems = append(problems, fmt.Sprintf("%s: Go migrations have no executor; use SQL", path))
			continue
		}
		if dir != layout.Migrations {
			continue
		}
		match := versionedName.FindStringSubmatch(name)
		if match == nil || !strings.HasSuffix(name, ".sql") {
			problems = append(problems, fmt.Sprintf("%s: expected a numbered SQL migration, such as 00001_baseline.sql", path))
			continue
		}
		version, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil || version < 1 {
			problems = append(problems, fmt.Sprintf("%s: invalid migration version", path))
			continue
		}
		if other, ok := versions[version]; ok {
			problems = append(problems, fmt.Sprintf("version %d is used by both %s and %s", version, other, path))
			continue
		}
		versions[version] = path
	}
	ordered := make([]int64, 0, len(versions))
	for version := range versions {
		ordered = append(ordered, version)
	}
	slices.Sort(ordered)
	if len(ordered) == 0 {
		return append(problems, "no active migrations found in "+layout.Migrations)
	}
	if len(ordered) > plan.MaxActiveVersions {
		problems = append(problems, fmt.Sprintf("%d active migrations exceed the %d-version cap; roll them into a new baseline first", len(ordered), plan.MaxActiveVersions))
	}
	first := ordered[0]
	if first != plan.MinimumSupportedVersion {
		problems = append(problems, fmt.Sprintf("the first active version %d must equal minimumSupportedVersion %d", first, plan.MinimumSupportedVersion))
	}
	if _, name := split(versions[first]); first > 1 && !regexp.MustCompile(fmt.Sprintf(`^0*%d_baseline\.sql$`, first)).MatchString(name) {
		problems = append(problems, fmt.Sprintf("version %d must be a complete SQL baseline named %05d_baseline.sql", first, first))
	}
	for i := 1; i < len(ordered); i++ {
		if ordered[i] != ordered[i-1]+1 {
			problems = append(problems, fmt.Sprintf("migration versions must be consecutive: %d is followed by %d", ordered[i-1], ordered[i]))
		}
	}
	if plan.LatestMigrationVersion != ordered[len(ordered)-1] {
		problems = append(problems, fmt.Sprintf("latestMigrationVersion %d must equal the latest migration %d", plan.LatestMigrationVersion, ordered[len(ordered)-1]))
	}
	return problems
}

var compatName = regexp.MustCompile(`(?:^|/)compat_v0*(\d+)(?:_[a-z][a-z0-9_]*)?\.go$`)

// Disruptive fails each migration the flock would refuse to run: one that
// would disrupt it (goose.Classify), and one that drops what the flight plan
// doesn't declare unused. The baseline is applied to an empty database,
// outside the flock, so it's none of the flock's business.
func Disruptive(migrations map[string]string, plan geese.FlightPlan) []string {
	var problems []string
	for path, migration := range migrations {
		_, name := split(path)
		match := versionedName.FindStringSubmatch(name)
		if match == nil {
			continue
		}
		if version, err := strconv.ParseInt(match[1], 10, 64); err != nil || version <= plan.MinimumSupportedVersion {
			continue
		}
		change := goose.Classify([]byte(migration))
		if change.Disrupts != "" {
			problems = append(problems, fmt.Sprintf("%s would disrupt the flock: %s", path, change.Disrupts))
			continue
		}
		for _, dropped := range change.Drops {
			if !slices.Contains(plan.Unused, dropped) {
				problems = append(problems, fmt.Sprintf("%s drops %s, which the flight plan doesn't declare unused", path, dropped))
			}
		}
	}
	slices.Sort(problems)
	return problems
}

// Compatibility checks each compatibility file: code kept for an older
// schema version the image still serves, named compat_vNNNN.go (or
// compat_vNNNN_<what>.go). It must be within the serving window and name the
// version's compile-time token, so raising the window's floor removes the
// token and fails the build until the file is deleted.
func Compatibility(sources map[string]string, plan geese.FlightPlan, layout Layout) []string {
	var problems []string
	paths := make([]string, 0, len(sources))
	for path := range sources {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		match := compatName.FindStringSubmatch(path)
		if match == nil {
			continue
		}
		version, _ := strconv.ParseInt(match[1], 10, 64)
		if version < plan.MinimumServeVersion || version > plan.MaximumServeVersion {
			problems = append(problems, fmt.Sprintf("%s: compatibility version %d is outside the serving window %d–%d", path, version, plan.MinimumServeVersion, plan.MaximumServeVersion))
		}
		token := fmt.Sprintf("%s.ServeV%d", layout.Tokens, version)
		if !strings.Contains(sources[path], token) {
			problems = append(problems, fmt.Sprintf("%s: missing the compile-time %s guard", path, token))
		}
	}
	return problems
}

// Tokens is the Go file of compile-time tokens for a plan: ServeVN for each
// version the image serves, MigrateVN for each active migration.
func Tokens(plan geese.FlightPlan, pkg, planPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "// Code generated by geese tokens from %s; DO NOT EDIT.\npackage %s\n\n", planPath, pkg)
	for version := plan.MinimumServeVersion; version <= plan.MaximumServeVersion; version++ {
		fmt.Fprintf(&b, "const ServeV%d = true\n", version)
	}
	for version := plan.MinimumSupportedVersion; version <= plan.LatestMigrationVersion; version++ {
		fmt.Fprintf(&b, "const MigrateV%d = true\n", version)
	}
	return b.String()
}

func split(path string) (dir, name string) {
	i := strings.LastIndex(path, "/")
	if i < 0 {
		return "", path
	}
	return path[:i], path[i+1:]
}
