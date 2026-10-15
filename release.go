package geese

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
)

// The oldest page served. A page, such as a web app a service worker keeps,
// may be older than the geese serving it; a breaking release is a major one,
// so a page from an older major may call what no goose serves any more. The
// flock says the oldest major release whose pages it serves, which pages read
// (with the capabilities) and reload below; it rises once every goose heard
// is of that major or later, and never goes down.

// release is SemVer, with build metadata allowed: a pre-release isn't a release.
var release = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(\+[0-9A-Za-z.-]+)?$`)

// ReleaseMajor is a release's major version, or none for a build that isn't
// a release: "dev", or a pre-release such as "0.0.0-dev.<commit>".
func ReleaseMajor(version string) sql.NullInt64 {
	match := release.FindStringSubmatch(version)
	if match == nil {
		return sql.NullInt64{}
	}
	major, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: major, Valid: true}
}

// checkRelease grounds an image older than the oldest page the flock serves:
// pages it can't speak to would be told it's fine. A build that isn't a
// release can't say, so it can't fly once a floor is set.
func (f *Flock) checkRelease(floor sql.NullInt64) error {
	if !floor.Valid {
		return nil
	}
	if !f.release.Valid {
		return fmt.Errorf("geese: this build isn't a release, and the flock serves pages from release %d.0.0 on", floor.Int64)
	}
	if f.release.Int64 < floor.Int64 {
		return fmt.Errorf("geese: release %d is older than the oldest page the flock serves, %d.0.0", f.release.Int64, floor.Int64)
	}
	return nil
}

func formatFloor(floor sql.NullInt64) string {
	if !floor.Valid {
		return ""
	}
	return fmt.Sprintf("%d.0.0", floor.Int64)
}

// OldestPage is the oldest release whose page the flock serves, as "8.0.0",
// or "" before one is set. Like ActiveCapabilities, it reads the flock's
// shared decision.
func (f *Flock) OldestPage(ctx context.Context) (string, error) {
	if !f.Ready() {
		return "", ErrGrounded
	}
	var floor sql.NullInt64
	if err := f.db.QueryRowContext(ctx, `SELECT oldest_page_major FROM geese_flight WHERE id=true`).Scan(&floor); err != nil {
		return "", err
	}
	return formatFloor(floor), nil
}

// TryRaiseFloor raises the oldest page the flock serves to the oldest major
// release of any goose heard, when this goose leads, the weather is calm and
// the flock isn't migrating. A goose that says no release holds it where it is.
func (f *Flock) TryRaiseFloor(ctx context.Context) error {
	if !f.Ready() || !f.release.Valid {
		return nil
	}
	var floor sql.NullInt64
	if err := f.db.QueryRowContext(ctx, `SELECT oldest_page_major FROM geese_flight WHERE id=true`).Scan(&floor); err != nil {
		return err
	}
	// Nothing this goose could raise it to.
	if floor.Valid && floor.Int64 >= f.release.Int64 {
		return nil
	}
	release, leading, err := f.lead(ctx)
	if err != nil || !leading {
		return err
	}
	defer release()

	tx, _, err := f.coordinated(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var heading sql.NullInt64
	if err = tx.QueryRowContext(ctx, `SELECT heading_for,oldest_page_major FROM geese_flight WHERE id=true FOR UPDATE`).Scan(&heading, &floor); err != nil {
		return err
	}
	if heading.Valid {
		return nil
	}
	if err = f.unanimous(ctx, tx); err != nil {
		return err
	}
	var undeclared int
	var oldest sql.NullInt64
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE release_major IS NULL), min(release_major) FROM geese_flock
		WHERE heard_until > clock_timestamp()`).Scan(&undeclared, &oldest); err != nil {
		return err
	}
	if undeclared != 0 || !oldest.Valid || (floor.Valid && oldest.Int64 <= floor.Int64) {
		return nil
	}
	if _, err = tx.ExecContext(ctx, `UPDATE geese_flight SET oldest_page_major=$1 WHERE id=true`, oldest.Int64); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	slog.Info("geese: oldest page served raised", "release", formatFloor(oldest))
	return nil
}
