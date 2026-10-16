//go:build integration

package geese

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jarensaa/geese/geesetest"
)

// The flock against PostgreSQL, with a stand-in for the application's
// migrations: a one-row ledger of the schema version, and a migration that
// only moves it on.

func TestMain(m *testing.M) { geesetest.Main(m) }

// ledger is the stand-in's migrator.
type ledger struct{}

func (ledger) Version(ctx context.Context, tx *sql.Tx) (int64, error) {
	var version int64
	err := tx.QueryRowContext(ctx, `SELECT version FROM ledger`).Scan(&version)
	return version, err
}

func (ledger) Change(int64) (Change, error) { return Change{}, nil }

func (ledger) MigrateTo(ctx context.Context, conn *sql.DB, target int64) error {
	_, err := conn.ExecContext(ctx, `UPDATE ledger SET version=$1`, target)
	return err
}

func standIn(t *testing.T, ctx context.Context) (*sql.DB, string) {
	t.Helper()
	conn, url := geesetest.Start(t, ctx)
	if _, err := conn.ExecContext(ctx, `CREATE TABLE ledger (version bigint NOT NULL); INSERT INTO ledger VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	return conn, url
}

// goose joins the stand-in's flock serving schemas 1 and 2, the latest.
func goose(t *testing.T, ctx context.Context, url, name string) *Flock {
	t.Helper()
	live, err := join(ctx, url, name, "dev")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(live.Land)
	return live
}

// join is a goose of a release joining, which may be refused.
func join(ctx context.Context, url, name, release string) (*Flock, error) {
	return Join(ctx, Config{DatabaseURL: url, Name: name, Release: release, Weather: ClearSkies{}, Migrator: ledger{},
		Window: Window{ServeFrom: 1, Latest: 2}})
}

func oldestPage(t *testing.T, ctx context.Context, live *Flock) string {
	t.Helper()
	oldest, err := live.OldestPage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return oldest
}

// The oldest page served follows the oldest major of any goose heard: held
// while any is older or says no release, raised once none is, never lowered,
// and an image older than it is grounded.
func TestOldestPageFollowsTheOldestRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	_, url := standIn(t, ctx)
	must := func(live *Flock, err error) *Flock {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return live
	}
	seven := must(join(ctx, url, "seven", "7.2.0"))
	defer seven.Land()
	dev := must(join(ctx, url, "dev", "0.0.0-dev.abc"))
	if err := seven.TryRaiseFloor(ctx); err != nil {
		t.Fatal(err)
	}
	if got := oldestPage(t, ctx, seven); got != "" {
		t.Fatalf("a goose without a release let the floor rise to %q", got)
	}
	dev.Land()
	if err := seven.TryRaiseFloor(ctx); err != nil {
		t.Fatal(err)
	}
	if got := oldestPage(t, ctx, seven); got != "7.0.0" {
		t.Fatalf("the floor is %q with only release 7 serving, want 7.0.0", got)
	}

	// A breaking release rolls out: the floor waits for the last goose of 7.
	eight := must(join(ctx, url, "eight", "8.0.0+abc123"))
	defer eight.Land()
	if err := eight.TryRaiseFloor(ctx); err != nil {
		t.Fatal(err)
	}
	if got := oldestPage(t, ctx, eight); got != "7.0.0" {
		t.Fatalf("the floor moved to %q while release 7 still serves", got)
	}
	seven.Land()
	if err := eight.TryRaiseFloor(ctx); err != nil {
		t.Fatal(err)
	}
	if got := oldestPage(t, ctx, eight); got != "8.0.0" {
		t.Fatalf("the floor is %q once only release 8 serves, want 8.0.0", got)
	}

	// Rolling back below it, or to a build that isn't a release, is refused.
	for _, release := range []string{"7.9.0", "dev"} {
		if refused, err := join(ctx, url, "rollback", release); err == nil {
			refused.Land()
			t.Fatalf("release %q joined below the floor", release)
		}
	}
	if got := oldestPage(t, ctx, eight); got != "8.0.0" {
		t.Fatalf("the floor went to %q", got)
	}
}

// A capability read abandoned by its request is the request's failure, not
// the goose's: it keeps its place.
func TestAbandonedCapabilityReadKeepsTheLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	_, url := standIn(t, ctx)
	live := goose(t, ctx, url, "goose")
	gone, abandon := context.WithCancel(ctx)
	abandon()
	if _, err := live.ActiveCapabilities(gone); err == nil {
		t.Fatal("a read with its request gone succeeded")
	}
	if !live.Ready() {
		t.Fatal("an abandoned read grounded the goose")
	}
	if _, release, err := live.Admit(ctx); err != nil {
		t.Fatalf("work was refused after an abandoned read: %v", err)
	} else {
		release()
	}
}

func ledgerVersion(t *testing.T, ctx context.Context, conn *sql.DB) int64 {
	t.Helper()
	var version int64
	if err := conn.QueryRowContext(ctx, `SELECT version FROM ledger`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

// A leader leads only a flock that all speaks its protocol: a goose of another
// protocol, which can't be known to read the flock's decisions as this one
// does, holds every decision until it has landed.
func TestALeaderWaitsForAGooseOfAnotherProtocol(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	conn, url := standIn(t, ctx)
	leader := goose(t, ctx, url, "leader")
	if _, err := conn.ExecContext(ctx, `INSERT INTO geese_flock (id,name,protocol,serves_from,serves_to,heard_until)
		VALUES (gen_random_uuid(),'newer',$1,1,3,clock_timestamp()+interval '1 minute')`, protocol+1); err != nil {
		t.Fatal(err)
	}
	if err := leader.TryMigrate(ctx); !errors.Is(err, errMixedFlock) {
		t.Fatalf("migrating with a goose of another protocol heard: %v", err)
	}
	if version := ledgerVersion(t, ctx, conn); version != 1 {
		t.Fatalf("migrated to %d with a goose of another protocol heard", version)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM geese_flock WHERE name='newer'`); err != nil {
		t.Fatal(err)
	}
	if err := leader.TryMigrate(ctx); err != nil {
		t.Fatal(err)
	}
	if version := ledgerVersion(t, ctx, conn); version != 2 {
		t.Fatalf("didn't migrate once the flock spoke one protocol: at %d", version)
	}
}

// The coordinator geese came from never writes the flock's tables, so a
// leader waits while any of its pods is still heard, and so does an offline
// migration (Flying).
func TestALeaderWaitsForTheCoordinatorGeeseCameFrom(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	conn, url := standIn(t, ctx)
	if _, err := conn.ExecContext(ctx, `CREATE TABLE schema_instances (boot_id uuid PRIMARY KEY, lease_until timestamptz NOT NULL);
		INSERT INTO schema_instances VALUES (gen_random_uuid(), clock_timestamp()+interval '1 minute')`); err != nil {
		t.Fatal(err)
	}
	leader := goose(t, ctx, url, "leader")
	if flying, err := Flying(ctx, conn); err != nil || flying != 2 {
		t.Fatalf("flying: %d %v, want the goose and the older pod", flying, err)
	}
	if err := leader.TryMigrate(ctx); !errors.Is(err, errMixedFlock) {
		t.Fatalf("migrating with an older pod heard: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE schema_instances SET lease_until=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if err := leader.TryMigrate(ctx); err != nil {
		t.Fatal(err)
	}
	if version := ledgerVersion(t, ctx, conn); version != 2 {
		t.Fatalf("didn't migrate once the older pods were gone: at %d", version)
	}
}

// A goose that can't serve where the flock is heading stays on the ground,
// and one that lands leaves at once.
func TestAGooseThatCantServeTheHeadingIsGrounded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	conn, url := standIn(t, ctx)
	live := goose(t, ctx, url, "goose")
	if _, err := conn.ExecContext(ctx, `UPDATE geese_flight SET heading_for=3`); err != nil {
		t.Fatal(err)
	}
	if err := live.Honk(ctx); err == nil || live.Ready() {
		t.Fatalf("a goose that serves up to 2 flew towards 3: %v", err)
	}
	if _, _, err := live.Admit(ctx); !errors.Is(err, ErrGrounded) {
		t.Fatalf("admitted while grounded: %v", err)
	}
	landed, err := join(ctx, url, "landing", "dev")
	if err == nil {
		landed.Land()
		t.Fatal("joined a flock heading where it can't go")
	}
	if _, err := conn.ExecContext(ctx, `UPDATE geese_flight SET heading_for=NULL`); err != nil {
		t.Fatal(err)
	}
	other := goose(t, ctx, url, "other")
	other.Land()
	var heard int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM geese_flock WHERE name='other'`).Scan(&heard); err != nil || heard != 0 {
		t.Fatalf("a goose that landed is still in the flock: %d %v", heard, err)
	}
}

// Moving from the coordinator geese came from: what it switched on stays on,
// the oldest page it served stays served, and a database it's migrating
// isn't joined until it's done.
func TestTheFlockCarriesOnFromTheCoordinatorGeeseCameFrom(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	conn, url := standIn(t, ctx)
	if _, err := conn.ExecContext(ctx, `CREATE TABLE schema_upgrade_state (
		id boolean PRIMARY KEY DEFAULT true CHECK (id), target_version bigint, started_at timestamptz,
		active_capabilities text[] NOT NULL DEFAULT '{}', oldest_page_major bigint);
		INSERT INTO schema_upgrade_state (id,target_version,active_capabilities,oldest_page_major) VALUES (true,2,'{lists}',7)`); err != nil {
		t.Fatal(err)
	}
	join := func(release string) (*Flock, error) {
		return Join(ctx, Config{DatabaseURL: url, Name: "new", Release: release, Weather: ClearSkies{}, Migrator: ledger{},
			Window: Window{ServeFrom: 1, Latest: 2}, Capabilities: []Capability{{Name: "lists", MinimumSchema: 1}}})
	}
	if refused, err := join("7.1.0"); err == nil || !strings.Contains(err.Error(), "migrating") {
		if refused != nil {
			refused.Land()
		}
		t.Fatalf("joined while the older coordinator migrates: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE schema_upgrade_state SET target_version=NULL`); err != nil {
		t.Fatal(err)
	}
	flock, err := join("7.1.0")
	if err != nil {
		t.Fatal(err)
	}
	defer flock.Land()
	active, err := flock.ActiveCapabilities(ctx)
	if err != nil || !slices.Equal(active, []string{"lists"}) {
		t.Fatalf("what was on: %v %v", active, err)
	}
	if page := oldestPage(t, ctx, flock); page != "7.0.0" {
		t.Fatalf("the oldest page served: %q", page)
	}
	if said, err := flock.Capabilities(ctx); err != nil || !slices.Equal(said.Active, []string{"lists"}) || said.OldestPage != "7.0.0" {
		t.Fatalf("what the flock says to clients: %+v %v", said, err)
	}
	if on, err := flock.Active(ctx, "lists"); err != nil || !on {
		t.Fatalf("lists is on: %v %v", on, err)
	}
	if on, err := flock.Active(ctx, "comments"); err != nil || on {
		t.Fatalf("comments isn't: %v %v", on, err)
	}
	if err := flock.Require(ctx, "lists"); err != nil {
		t.Fatalf("requiring lists: %v", err)
	}
	if err := flock.Require(ctx, "comments"); !errors.Is(err, ErrInactive) {
		t.Fatalf("requiring comments: %v", err)
	}
	// Carried over once: what the flock decides from then on is its own.
	if _, err := conn.ExecContext(ctx, `UPDATE schema_upgrade_state SET active_capabilities='{}'`); err != nil {
		t.Fatal(err)
	}
	again, err := join("7.1.0")
	if err != nil {
		t.Fatal(err)
	}
	defer again.Land()
	if active, _ := again.ActiveCapabilities(ctx); !slices.Equal(active, []string{"lists"}) {
		t.Fatalf("carried over again: %v", active)
	}
}

// stalling is the stand-in's migrator, whose read of the version waits while
// a test is waiting for it, until the test lets it go: so the test can act
// while a honk is under way.
type stalling struct {
	ledger
	reached, proceed chan struct{}
}

func newStalling() *stalling {
	return &stalling{reached: make(chan struct{}), proceed: make(chan struct{})}
}

func (s *stalling) Version(ctx context.Context, tx *sql.Tx) (int64, error) {
	select {
	case s.reached <- struct{}{}:
		<-s.proceed
	default:
	}
	return s.ledger.Version(ctx, tx)
}

func joinStalling(t *testing.T, ctx context.Context, url string, migrator *stalling) *Flock {
	t.Helper()
	live, err := Join(ctx, Config{DatabaseURL: url, Name: "stalling", Release: "dev", Weather: ClearSkies{}, Migrator: migrator,
		Window: Window{ServeFrom: 1, Latest: 2}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(live.Land)
	return live
}

// A goose told to stop while it honks finishes the honk, and its work in
// flight goes on.
func TestStoppingMidHonkLetsWorkInFlightFinish(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, url := standIn(t, ctx)
	honk := newStalling()
	live := joinStalling(t, ctx, url, honk)
	work, release, err := live.Admit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	flying, stop := context.WithCancel(ctx)
	landed := make(chan struct{})
	go func() { live.Fly(flying); close(landed) }()
	<-honk.reached
	stop()
	close(honk.proceed)
	<-landed
	if work.Err() != nil || !live.Ready() {
		t.Fatal("stopping while a honk was under way cancelled the work in flight")
	}
}

// A goose grounded while it honks stays grounded until the next honk: what
// this one says may no longer be true.
func TestGroundedMidHonkStaysGrounded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, url := standIn(t, ctx)
	honk := newStalling()
	live := joinStalling(t, ctx, url, honk)

	honked := make(chan error, 1)
	go func() { honked <- live.Honk(ctx) }()
	<-honk.reached
	live.ground()
	close(honk.proceed)
	if err := <-honked; err == nil || live.Ready() {
		t.Fatalf("a goose grounded mid-honk flew on: %v", err)
	}
	if err := live.Honk(ctx); err != nil || !live.Ready() {
		t.Fatalf("the next honk didn't put it back in the air: %v", err)
	}
}

// honking keeps a goose in the flock, as Fly does, without leading it: the
// test leads.
func honking(t *testing.T, ctx context.Context, live *Flock) {
	t.Helper()
	ctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			_ = live.Honk(ctx)
			select {
			case <-ctx.Done():
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
	t.Cleanup(func() { stop(); <-done })
}

// failing is the stand-in's migrator, whose migration fails until it's fixed,
// leaving the schema as it was, as a transactional migration does.
type failing struct {
	ledger
	fixed bool
}

func (m *failing) MigrateTo(ctx context.Context, conn *sql.DB, target int64) error {
	if !m.fixed {
		return errors.New("constraint violated")
	}
	return m.ledger.MigrateTo(ctx, conn, target)
}

// A migration that fails lets the flock go on at the schema applied, and the
// leader waits longer between each attempt until one succeeds.
func TestAFailedMigrationLetsTheFlockGoOn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, url := standIn(t, ctx)
	migrator := &failing{}
	leader, err := Join(ctx, Config{DatabaseURL: url, Name: "leader", Release: "dev", Weather: ClearSkies{}, Migrator: migrator,
		Window: Window{ServeFrom: 1, Latest: 2}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(leader.Land)
	honking(t, ctx, leader)

	if err := leader.TryMigrate(ctx); err == nil || !strings.Contains(err.Error(), "constraint violated") {
		t.Fatalf("a failed migration: %v", err)
	}
	time.Sleep(time.Second)
	if _, release, err := leader.Admit(ctx); err != nil {
		t.Fatalf("the flock didn't go on after a failed migration: %v", err)
	} else {
		release()
	}
	var failures int
	var waiting bool
	if err := conn.QueryRowContext(ctx, `SELECT failures, retry_at > clock_timestamp() + interval '20 seconds' FROM geese_flight WHERE failed_for=2 AND heading_for IS NULL`).Scan(&failures, &waiting); err != nil {
		t.Fatalf("the failure isn't recorded: %v", err)
	}
	if failures != 1 || !waiting {
		t.Fatalf("failures=%d, waiting=%v", failures, waiting)
	}

	// Until the retry is due, no leader tries again.
	if err := leader.TryMigrate(ctx); err != nil {
		t.Fatal(err)
	}
	var heading sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT heading_for FROM geese_flight`).Scan(&heading); err != nil || heading.Valid {
		t.Fatalf("tried again before the retry was due: heading=%v err=%v", heading, err)
	}

	// The next failure waits twice as long.
	if _, err := conn.ExecContext(ctx, `UPDATE geese_flight SET retry_at=clock_timestamp()`); err != nil {
		t.Fatal(err)
	}
	if err := leader.TryMigrate(ctx); err == nil {
		t.Fatal("the migration still fails")
	}
	if err := conn.QueryRowContext(ctx, `SELECT failures, retry_at > clock_timestamp() + interval '50 seconds' FROM geese_flight`).Scan(&failures, &waiting); err != nil || failures != 2 || !waiting {
		t.Fatalf("failures=%d, waiting=%v, err=%v", failures, waiting, err)
	}

	// Fixed, it migrates once the retry is due, and the failure is forgotten.
	migrator.fixed = true
	if _, err := conn.ExecContext(ctx, `UPDATE geese_flight SET retry_at=clock_timestamp()`); err != nil {
		t.Fatal(err)
	}
	if err := leader.TryMigrate(ctx); err != nil || leader.Version() != 2 {
		t.Fatalf("migrated to %d: %v", leader.Version(), err)
	}
	var failed sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT failed_for, failures FROM geese_flight`).Scan(&failed, &failures); err != nil || failed.Valid || failures != 0 {
		t.Fatalf("the failure is remembered: failed_for=%v failures=%d err=%v", failed, failures, err)
	}
}

// classified is the stand-in's migrator, saying its migration changes what
// the test says.
type classified struct {
	ledger
	change Change
}

func (m classified) Change(int64) (Change, error) { return m.change, nil }

func joinClassified(t *testing.T, ctx context.Context, url, name string, change Change, unused ...string) *Flock {
	t.Helper()
	live, err := Join(ctx, Config{DatabaseURL: url, Name: name, Release: "dev", Weather: ClearSkies{}, Migrator: classified{change: change},
		Window: Window{ServeFrom: 1, Latest: 2}, Unused: unused})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(live.Land)
	return live
}

// The flock migrates while it serves: work in flight runs on, and new work is
// admitted throughout.
func TestTheFlockMigratesWhileItServes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, url := standIn(t, ctx)
	leader := joinClassified(t, ctx, url, "leader", Change{})
	other := joinClassified(t, ctx, url, "other", Change{})
	work, release, err := other.Admit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	migrated := make(chan error, 1)
	go func() { migrated <- leader.TryMigrate(ctx) }()
	select {
	case err := <-migrated:
		if err != nil || leader.Version() != 2 {
			t.Fatalf("migrated to %d: %v", leader.Version(), err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the migration waited for the flock")
	}
	if _, more, err := other.Admit(ctx); err != nil {
		t.Fatalf("work wasn't admitted once the flock migrated: %v", err)
	} else {
		more()
	}
	if err := other.Honk(ctx); err != nil || other.Version() != 2 || work.Err() != nil {
		t.Fatalf("version %d, work %v, honk %v", other.Version(), work.Err(), err)
	}
}

// A migration that drops a column waits until every goose heard declares the
// column unused, and a goose that doesn't can't join while it runs.
func TestADropWaitsForEveryGooseToDeclareItUnused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, url := standIn(t, ctx)
	drop := Change{Drops: []string{"lists.archived"}}
	leader := joinClassified(t, ctx, url, "leader", drop, "lists.archived", "invites")
	older := joinClassified(t, ctx, url, "older", drop)

	if err := leader.TryMigrate(ctx); err != nil || leader.Version() != 1 {
		t.Fatalf("dropped what a goose may read: version %d, %v", leader.Version(), err)
	}
	older.Land()

	// While the drop runs, its heading fences out a goose that may read the column.
	if _, err := conn.ExecContext(ctx, `UPDATE geese_flight SET heading_for=2, dropping='{lists.archived}'`); err != nil {
		t.Fatal(err)
	}
	if late, err := Join(ctx, Config{DatabaseURL: url, Name: "late", Release: "dev", Weather: ClearSkies{}, Migrator: classified{change: drop},
		Window: Window{ServeFrom: 1, Latest: 2}}); err == nil {
		late.Land()
		t.Fatal("a goose that may read the column joined while it's dropped")
	}
	if err := leader.TryMigrate(ctx); err != nil || leader.Version() != 2 {
		t.Fatalf("every goose declares it unused: version %d, %v", leader.Version(), err)
	}
}

// A migration that would disrupt the flock isn't run: the attempt is recorded
// as failed, and the schema stays as it was.
func TestAMigrationThatWouldDisruptIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, url := standIn(t, ctx)
	leader := joinClassified(t, ctx, url, "leader", Change{Disrupts: "a rename"})
	if err := leader.TryMigrate(ctx); err == nil || !strings.Contains(err.Error(), "would disrupt the flock") {
		t.Fatalf("ran a migration that would disrupt the flock: %v", err)
	}
	var version int64
	var failure string
	if err := conn.QueryRowContext(ctx, `SELECT (SELECT version FROM ledger), failure FROM geese_flight WHERE failed_for=2 AND retry_at > clock_timestamp()`).Scan(&version, &failure); err != nil || version != 1 || !strings.Contains(failure, "a rename") {
		t.Fatalf("version %d, failure %q: %v", version, failure, err)
	}
}

// altering is the stand-in's migrator, whose migration alters a table of the
// application's.
type altering struct{ ledger }

func (m altering) MigrateTo(ctx context.Context, conn *sql.DB, target int64) error {
	if _, err := conn.ExecContext(ctx, `ALTER TABLE lists ADD COLUMN colour text`); err != nil {
		return err
	}
	return m.ledger.MigrateTo(ctx, conn, target)
}

// A migration never holds up the flock's queries: it waits for a lock only a
// moment, so nothing queues behind it for long, and tries again. One that
// never gets its lock fails, and is tried again later.
func TestAMigrationNeverHoldsUpQueries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, url := standIn(t, ctx)
	if _, err := conn.ExecContext(ctx, `CREATE TABLE lists (id bigint)`); err != nil {
		t.Fatal(err)
	}
	leader, err := Join(ctx, Config{DatabaseURL: url, Name: "leader", Release: "dev", Weather: ClearSkies{}, Migrator: altering{},
		Window: Window{ServeFrom: 1, Latest: 2}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(leader.Land)
	honking(t, ctx, leader)

	// The flock's queries on the table, timed, all along.
	querying, stopQuerying := context.WithCancel(ctx)
	slowest := make(chan time.Duration, 1)
	go func() {
		var worst time.Duration
		for querying.Err() == nil {
			started := time.Now()
			if _, err := conn.ExecContext(querying, `SELECT count(*) FROM lists`); err == nil {
				worst = max(worst, time.Since(started))
			}
			time.Sleep(5 * time.Millisecond)
		}
		slowest <- worst
	}()

	// A long read holds the table throughout the first attempt.
	reading, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reading.ExecContext(ctx, `SELECT * FROM lists`); err != nil {
		t.Fatal(err)
	}
	if err := leader.TryMigrate(ctx); err == nil || !strings.Contains(err.Error(), "lock timeout") {
		t.Fatalf("migrated through a lock: %v", err)
	}

	// The next attempt gets its lock once the read ends, while it tries.
	if _, err := conn.ExecContext(ctx, `UPDATE geese_flight SET retry_at=clock_timestamp()`); err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(time.Second, func() { reading.Rollback() })
	if err := leader.TryMigrate(ctx); err != nil || leader.Version() != 2 {
		t.Fatalf("migrated to %d: %v", leader.Version(), err)
	}
	stopQuerying()
	worst := <-slowest
	t.Logf("the slowest query took %s", worst)
	if worst > 250*time.Millisecond {
		t.Fatalf("a query waited %s behind the migration", worst)
	}
}

// Tests run on a database without what the flight plan declares unused.
func TestDropUnused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, _ := geesetest.Start(t, ctx)
	if _, err := conn.ExecContext(ctx, `CREATE SCHEMA app; CREATE TABLE app.tokens (id bigint);
		CREATE TABLE lists (id bigint, archived boolean); CREATE TABLE invites (id bigint)`); err != nil {
		t.Fatal(err)
	}
	if err := geesetest.DropUnused(ctx, conn, []string{"lists.archived", "invites", "app.tokens"}); err != nil {
		t.Fatal(err)
	}
	var left string
	if err := conn.QueryRowContext(ctx, `SELECT string_agg(table_schema||'.'||table_name||'.'||column_name, ',' ORDER BY 1)
		FROM information_schema.columns WHERE table_schema IN ('public','app')`).Scan(&left); err != nil || left != "public.lists.id" {
		t.Fatalf("left %q: %v", left, err)
	}
	if err := geesetest.DropUnused(ctx, conn, []string{"nothing"}); err == nil {
		t.Fatal("dropped a name that's nothing")
	}
}

// recording is the stand-in's migrator, whose migration asks for downtime and
// says when it ran.
type recording struct {
	ledger
	ran chan time.Time
}

func (recording) Change(int64) (Change, error) { return Change{Downtime: true}, nil }

func (m recording) MigrateTo(ctx context.Context, conn *sql.DB, target int64) error {
	m.ran <- time.Now()
	return m.ledger.MigrateTo(ctx, conn, target)
}

// A migration that asks for downtime runs once every goose has stopped
// serving: new work is refused meanwhile, work in flight finishes first, and
// the flock serves again once it's done.
func TestAMigrationWithDowntime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, url := standIn(t, ctx)
	migrator := recording{ran: make(chan time.Time, 1)}
	join := func(name string) *Flock {
		live, err := Join(ctx, Config{DatabaseURL: url, Name: name, Release: "dev", Weather: ClearSkies{}, Migrator: migrator,
			Window: Window{ServeFrom: 1, Latest: 2}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(live.Land)
		honking(t, ctx, live)
		return live
	}
	leader, other := join("leader"), join("other")
	_, release, err := other.Admit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan time.Time, 1)
	time.AfterFunc(time.Second, func() { released <- time.Now(); release() })

	migrated := make(chan error, 1)
	go func() { migrated <- leader.TryMigrate(ctx) }()
	time.Sleep(500 * time.Millisecond)
	if _, _, err := other.Admit(ctx); !errors.Is(err, ErrRoosting) {
		t.Fatalf("admitted work while the flock is down: %v", err)
	}
	if err := <-migrated; err != nil {
		t.Fatal(err)
	}
	if ran, done := <-migrator.ran, <-released; ran.Before(done) {
		t.Fatal("migrated while work was still running")
	}
	time.Sleep(500 * time.Millisecond)
	if _, again, err := other.Admit(ctx); err != nil || other.Version() != 2 {
		t.Fatalf("not serving at 2 after the migration: version %d, %v", other.Version(), err)
	} else {
		again()
	}
}

// An older image serves a schema newer than its own, as a rollback runs it,
// while every migration since its own ran live, and it declares unused what
// they dropped: a migration that ran live kept the code before it working.
func TestRollingBackAcrossLiveMigrations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, url := standIn(t, ctx)
	newer := joinClassified(t, ctx, url, "newer", Change{Drops: []string{"lists.archived"}}, "lists.archived")
	if err := newer.TryMigrate(ctx); err != nil || newer.Version() != 2 {
		t.Fatalf("migrated to %d: %v", newer.Version(), err)
	}
	newer.Land()

	older := func(unused ...string) (*Flock, error) {
		return Join(ctx, Config{DatabaseURL: url, Name: "older", Release: "dev", Weather: ClearSkies{}, Migrator: ledger{},
			Window: Window{ServeFrom: 1, Latest: 1}, Unused: unused})
	}
	if old, err := older(); !errors.Is(err, ErrSchemaUnserved) {
		if old != nil {
			old.Land()
		}
		t.Fatalf("an image that may read a column dropped since joined: %v", err)
	}
	old, err := older("lists.archived")
	if err != nil {
		t.Fatalf("an image rolled back to across a live migration: %v", err)
	}
	if old.Version() != 1 {
		t.Fatalf("it runs its code for %d, not its newest", old.Version())
	}
	if _, release, err := old.Admit(ctx); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
	old.Land()

	// A migration with downtime since closes the way back, as does one the
	// flock has no record of.
	for _, change := range []string{`UPDATE geese_journey SET downtime=true`, `DELETE FROM geese_journey`} {
		if _, err := conn.ExecContext(ctx, change); err != nil {
			t.Fatal(err)
		}
		if old, err := older("lists.archived"); !errors.Is(err, ErrSchemaUnserved) {
			if old != nil {
				old.Land()
			}
			t.Fatalf("after %q, an image older than the schema joined: %v", change, err)
		}
	}
}
