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
		Window: Window{ServeFrom: 1, ServeTo: 2, Latest: 2}})
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

// A migration waits for admitted work to finish, and holds new work until the
// schema it runs on is in place.
func TestTheFlockGathersBeforeItMigrates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	conn, url := standIn(t, ctx)
	leader, other := goose(t, ctx, url, "leader"), goose(t, ctx, url, "other")
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go other.Fly(runCtx)
	go leader.Fly(runCtx)

	// A request on the other goose is in flight when the migration begins.
	work, release, err := other.Admit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	upgraded := make(chan error, 1)
	go func() { upgraded <- leader.TryMigrate(ctx) }()
	// Every goose hears the heading at its next honk.
	time.Sleep(3 * time.Second)
	var version int64
	if err := conn.QueryRowContext(ctx, `SELECT version FROM ledger`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("migrated to %d while a request was in flight", version)
	}
	if work.Err() != nil {
		t.Fatal("the migration cancelled a request within its grace")
	}
	// New work is held, not refused, meanwhile.
	held := make(chan int64, 1)
	go func() {
		_, done, err := other.Admit(ctx)
		if err != nil {
			held <- -1
			return
		}
		held <- other.Version()
		done()
	}()
	release()
	select {
	case err := <-upgraded:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the migration didn't run once the request finished")
	}
	select {
	case got := <-held:
		if got != 2 {
			t.Fatalf("held work resumed on schema %d, want 2", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("held work didn't resume after the migration")
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
			Window: Window{ServeFrom: 1, ServeTo: 2, Latest: 2}, Capabilities: []Capability{{Name: "lists", MinimumSchema: 1}}})
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
		Window: Window{ServeFrom: 1, ServeTo: 2, Latest: 2}})
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
// this one says, such as that it has gathered, may no longer be true.
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
