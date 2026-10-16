// Command geese checks an application's flight plan against its repository,
// and writes the compile-time tokens its compatibility code names.
//
//	geese check [--staged] [flags]   the migration window, as the plan says
//	geese tokens [--check] [flags]   the tokens file, or whether it's current
//
// Paths are from the repository's root, wherever the command runs in it. The
// flight plan is flightplan.json unless --plan says otherwise, and the rest is
// beside it: its migrations in migrations/, and the tokens file in
// support/generated.go. A pre-commit hook runs `geese check --staged`, CI
// `geese check` and `geese tokens --check`.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/jarensaa/geese"
	"github.com/jarensaa/geese/internal/check"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "geese:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: geese check|tokens [flags]")
	}
	flags := flag.NewFlagSet("geese "+args[0], flag.ContinueOnError)
	planPath := flags.String("plan", "flightplan.json", "the flight plan")
	// Unset, a path is beside the plan.
	beside := func(set *string, name string) {
		if *set == "" {
			*set = path.Join(path.Dir(*planPath), name)
		}
	}
	switch args[0] {
	case "check":
		staged := flags.Bool("staged", false, "check what's staged for commit, not the working tree")
		layout := check.Layout{}
		flags.StringVar(&layout.Migrations, "migrations", "", "the SQL migrations (default: migrations/ beside the plan)")
		flags.StringVar(&layout.Code, "code", "", "where Go migrations would be, which aren't allowed (default: migrationcode/ beside the plan)")
		flags.StringVar(&layout.Source, "source", ".", "where compatibility files may be")
		flags.StringVar(&layout.Tokens, "tokens", "support", "the package of the compile-time tokens, as compatibility files name it")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		beside(&layout.Migrations, "migrations")
		beside(&layout.Code, "migrationcode")
		return checkRepository(*planPath, layout, *staged)
	case "tokens":
		out := flags.String("out", "", "the tokens file (default: support/generated.go beside the plan)")
		pkg := flags.String("package", "support", "the tokens file's package")
		verify := flags.Bool("check", false, "fail when the tokens file isn't current, instead of writing it")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		beside(out, "support/generated.go")
		return tokens(*planPath, *out, *pkg, *verify)
	}
	return fmt.Errorf("unknown command %q: geese check|tokens", args[0])
}

// repository is a git work tree, read as staged or as it is.
type repository struct {
	root   string
	staged bool
}

func openRepository(staged bool) (repository, error) {
	root, err := git("", "rev-parse", "--show-toplevel")
	if err != nil {
		return repository{}, fmt.Errorf("geese works in a git repository: %w", err)
	}
	return repository{root: strings.TrimSpace(root), staged: staged}, nil
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// files lists the files under the given paths: staged ones, or those in the
// work tree that git doesn't ignore.
func (r repository) files(paths ...string) ([]string, error) {
	args := []string{"ls-files", "--cached", "-z"}
	if !r.staged {
		args = append(args, "--others", "--exclude-standard")
	}
	out, err := git(r.root, append(append(args, "--"), paths...)...)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, path := range strings.Split(out, "\x00") {
		if path == "" {
			continue
		}
		// Deleted from the work tree but still in the index: gone, unless checking what's staged.
		if !r.staged {
			if _, err := os.Stat(filepath.Join(r.root, path)); err != nil {
				continue
			}
		}
		files = append(files, path)
	}
	return files, nil
}

func (r repository) read(path string) ([]byte, error) {
	if r.staged {
		out, err := git(r.root, "show", ":"+path)
		return []byte(out), err
	}
	return os.ReadFile(filepath.Join(r.root, path))
}

func checkRepository(planPath string, layout check.Layout, staged bool) error {
	repo, err := openRepository(staged)
	if err != nil {
		return err
	}
	data, err := repo.read(planPath)
	if err != nil {
		return err
	}
	plan, err := geese.ParseFlightPlan(data)
	if err != nil {
		return err
	}
	migrations, err := repo.files(layout.Migrations, layout.Code)
	if err != nil {
		return err
	}
	problems := check.Migrations(migrations, plan, layout)
	sql := map[string]string{}
	for _, path := range migrations {
		if filepath.Dir(path) != layout.Migrations || !strings.HasSuffix(path, ".sql") {
			continue
		}
		migration, err := repo.read(path)
		if err != nil {
			return err
		}
		sql[path] = string(migration)
	}
	problems = append(problems, check.Disruptive(sql, plan)...)
	sourcePaths, err := repo.files(layout.Source)
	if err != nil {
		return err
	}
	sources := map[string]string{}
	for _, path := range sourcePaths {
		if !strings.HasSuffix(path, ".go") || !strings.Contains(filepath.Base(path), "compat_v") {
			continue
		}
		source, err := repo.read(path)
		if err != nil {
			return err
		}
		sources[path] = string(source)
	}
	problems = append(problems, check.Compatibility(sources, plan, layout)...)
	if len(problems) > 0 {
		for _, problem := range problems {
			fmt.Fprintln(os.Stderr, "flight plan:", problem)
		}
		return fmt.Errorf("%d problems with the flight plan", len(problems))
	}
	active := 0
	for _, path := range migrations {
		if filepath.Dir(path) == layout.Migrations {
			active++
		}
	}
	fmt.Printf("Flight plan OK: %d of %d active migrations, serving %d–%d.\n", active, plan.MaxActiveVersions, plan.MinimumServeVersion, plan.MaximumServeVersion)
	return nil
}

func tokens(planPath, out, pkg string, verify bool) error {
	repo, err := openRepository(false)
	if err != nil {
		return err
	}
	plan, err := geese.ReadFlightPlan(filepath.Join(repo.root, planPath))
	if err != nil {
		return err
	}
	want := check.Tokens(plan, pkg, planPath)
	target := filepath.Join(repo.root, out)
	if !verify {
		return os.WriteFile(target, []byte(want), 0o644)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != want {
		return fmt.Errorf("%s isn't current; run geese tokens", out)
	}
	return nil
}
