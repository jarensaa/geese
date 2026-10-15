package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo makes a git repository with a flight plan and these files, and runs
// the test in it.
func repo(t *testing.T, plan string, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	files["server/db/flightplan.json"] = plan
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
	t.Chdir(filepath.Join(root, "server"))
	return root
}

const plan = `{"maxActiveVersions":4,"minimumSupportedVersion":1,"latestMigrationVersion":2,"minimumServeVersion":1,"maximumServeVersion":2}`

func TestCheckAndTokens(t *testing.T) {
	root := repo(t, plan, map[string]string{
		"server/db/migrations/00001_baseline.sql": "",
		"server/db/migrations/00002_lists.sql":    "",
		"server/app/compat_v0001.go":              "package app\n\nconst _ = support.ServeV1\n",
	})
	if err := run([]string{"check"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"check", "--staged"}); err != nil {
		t.Fatal(err)
	}
	// The tokens file is written, then current.
	if err := run([]string{"tokens", "--check"}); err == nil {
		t.Fatal("a missing tokens file passed")
	}
	if err := os.MkdirAll(filepath.Join(root, "server/db/support"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"tokens"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"tokens", "--check"}); err != nil {
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
	if err := run([]string{"check"}); err == nil {
		t.Fatal("a migration past the plan's last passed")
	}
	if err := run([]string{"check", "--staged"}); err != nil {
		t.Fatalf("what's staged is fine: %v", err)
	}
}
