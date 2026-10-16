package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo makes a git repository with a flight plan at planPath and these files,
// and runs the test in it.
func repo(t *testing.T, planPath, plan string, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	files[planPath] = plan
	for path, content := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	t.Chdir(filepath.Join(root, filepath.Dir(planPath)))
	return root
}

const plan = `{"maxActiveVersions":4,"minimumSupportedVersion":1,"latestMigrationVersion":2,"minimumServeVersion":1,"maximumServeVersion":2}`

// With no flags, the plan is flightplan.json at the root, and the rest is
// beside it.
func TestCheckBesideThePlan(t *testing.T) {
	root := repo(t, "flightplan.json", plan, map[string]string{
		"migrations/00001_baseline.sql": "-- +goose Up\nCREATE TABLE lists (id bigint);",
		"migrations/00002_lists.sql":    "-- +goose Up\nALTER TABLE lists ADD COLUMN name text;",
		"compat_v0001.go":               "package main\n\nconst _ = support.ServeV1\n",
	})
	if err := run([]string{"check"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"tokens"}); err == nil {
		t.Fatal("wrote the tokens file into a directory that isn't there")
	}
	if err := os.MkdirAll(filepath.Join(root, "support"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"tokens"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"check"}); err != nil {
		t.Fatalf("the tokens file isn't a migration: %v", err)
	}
}

// A plan elsewhere has its migrations and tokens beside it.
func TestCheckAndTokens(t *testing.T) {
	root := repo(t, "server/db/flightplan.json", plan, map[string]string{
		"server/db/migrations/00001_baseline.sql": "-- +goose Up\nCREATE TABLE lists (id bigint);",
		"server/db/migrations/00002_lists.sql":    "-- +goose Up\nALTER TABLE lists ADD COLUMN name text;",
		"server/app/compat_v0001.go":              "package app\n\nconst _ = support.ServeV1\n",
	})
	t.Chdir(root)
	if err := run([]string{"check", "--plan", "server/db/flightplan.json"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"check", "--staged", "--plan", "server/db/flightplan.json"}); err != nil {
		t.Fatal(err)
	}
	// The tokens file is written, then current.
	if err := run([]string{"tokens", "--check", "--plan", "server/db/flightplan.json"}); err == nil {
		t.Fatal("a missing tokens file passed")
	}
	if err := os.MkdirAll(filepath.Join(root, "server/db/support"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"tokens", "--plan", "server/db/flightplan.json"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"tokens", "--check", "--plan", "server/db/flightplan.json"}); err != nil {
		t.Fatal(err)
	}
	written, _ := os.ReadFile(filepath.Join(root, "server/db/support/generated.go"))
	if !strings.Contains(string(written), "const ServeV2 = true") {
		t.Fatalf("tokens: %s", written)
	}

	// A migration in the work tree, not yet staged: the work tree has it, the index doesn't.
	if err := os.WriteFile(filepath.Join(root, "server/db/migrations/00003_items.sql"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"check", "--plan", "server/db/flightplan.json"}); err == nil {
		t.Fatal("a migration past the plan's last passed")
	}
	if err := run([]string{"check", "--staged", "--plan", "server/db/flightplan.json"}); err != nil {
		t.Fatalf("what's staged is fine: %v", err)
	}
}
