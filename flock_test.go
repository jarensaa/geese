package geese

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestGroundingCancelsWorkInFlightAndAdmitsNoMore(t *testing.T) {
	live := &Flock{requests: map[uint64]admission{}}
	live.ready.Store(true)
	deadline := time.Now().Add(time.Minute)
	live.deadline.Store(&deadline)
	live.version.Store(2)
	ctx, release, err := live.Admit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	live.ground()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("in-flight request was not canceled")
	}
	if _, _, err := live.Admit(context.Background()); !errors.Is(err, ErrGrounded) {
		t.Fatalf("admitted work after grounding: %v", err)
	}
}

func TestOwnDeadlineGroundsAGooseWhoseHonkIsntHeard(t *testing.T) {
	live := &Flock{requests: map[uint64]admission{}}
	live.keepUntil(live.epoch(), time.Now().Add(50*time.Millisecond))
	ctx, release, err := live.Admit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("lease deadline did not cancel the in-flight request")
	}
	if live.Ready() {
		t.Fatal("a goose stayed ready after its deadline")
	}
}

func TestServesOnlyItsDeclaredSchemas(t *testing.T) {
	// A release may declare it serves the schema after the one it migrates to.
	live := &Flock{window: Window{ServeFrom: 5, ServeTo: 7, Latest: 6}}
	for version, served := range map[int64]bool{4: false, 5: true, 7: true, 8: false} {
		if err := live.checkServeVersion(version); (err == nil) != served {
			t.Errorf("schema %d: %v", version, err)
		}
	}
}

func TestReleaseMajor(t *testing.T) {
	for version, want := range map[string]int64{"8.1.0": 8, "v12.0.3": 12, "8.1.0+0a1b2c3d4e5f": 8, "0.4.0": 0} {
		if got := ReleaseMajor(version); !got.Valid || got.Int64 != want {
			t.Errorf("%q: major %v, want %d", version, got, want)
		}
	}
	// Builds that aren't releases declare none.
	for _, version := range []string{"dev", "", "0.0.0-dev.0a1b2c3d4e5f", "8.1.0-rc.1", "8.1", "08.1.0"} {
		if got := ReleaseMajor(version); got.Valid {
			t.Errorf("%q declares major %d", version, got.Int64)
		}
	}
}

func TestServesOnlyFromTheOldestPage(t *testing.T) {
	floor := sql.NullInt64{Int64: 8, Valid: true}
	for release, served := range map[string]bool{"7.9.9": false, "8.0.0": true, "9.1.0": true, "dev": false} {
		live := &Flock{release: ReleaseMajor(release)}
		if err := live.checkRelease(floor); (err == nil) != served {
			t.Errorf("release %q under floor 8: %v", release, err)
		}
		if err := live.checkRelease(sql.NullInt64{}); err != nil {
			t.Errorf("release %q refused before any floor: %v", release, err)
		}
	}
}

func TestUnknownActiveCapabilityIsUnsupported(t *testing.T) {
	live := &Flock{capabilities: []Capability{{Name: "sharedLists", MinimumSchema: 3}}}
	if !live.supportsCapabilities([]string{"sharedLists"}) {
		t.Fatal("this image should support sharedLists")
	}
	if live.supportsCapabilities([]string{"futureCapability"}) {
		t.Fatal("this image must reject unknown active capabilities")
	}
	if err := live.checkActiveCapabilities([]string{"sharedLists"}, 2); err == nil {
		t.Fatal("sharedLists cannot be served on schema 2")
	}
}

func TestRetiredCapabilityIsSupportedButNotReported(t *testing.T) {
	live := &Flock{capabilities: []Capability{{Name: "listComments", MinimumSchema: 4}}, retired: map[string]bool{"sharedLists": true}}
	if !live.supportsCapabilities([]string{"sharedLists", "listComments"}) {
		t.Fatal("a database naming a retired capability active must still be served")
	}
	if got := live.supportedNames(); !slices.Equal(got, []string{"listComments", "sharedLists"}) {
		t.Fatalf("advertised %v; older geese must see the retired capability supported", got)
	}
}

func readyFlock() *Flock {
	live := &Flock{requests: map[uint64]admission{}}
	live.ready.Store(true)
	deadline := time.Now().Add(time.Minute)
	live.deadline.Store(&deadline)
	return live
}

func TestGatheringHoldsNewWorkUntilTheMigrationIsApplied(t *testing.T) {
	live := readyFlock()
	if !live.gatherFor(11) {
		t.Fatal("a goose with nothing running gathers at once")
	}
	admitted := make(chan error, 1)
	go func() {
		_, release, err := live.Admit(context.Background())
		if err == nil {
			release()
		}
		admitted <- err
	}()
	select {
	case <-admitted:
		t.Fatal("work was admitted while gathering for a migration")
	case <-time.After(50 * time.Millisecond):
	}
	live.endGathering()
	select {
	case err := <-admitted:
		if err != nil {
			t.Fatalf("held work was refused once the migration applied: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("held work did not resume")
	}
}

func TestGatheringInterruptsStreamsAndWaitsForRequests(t *testing.T) {
	live := readyFlock()
	requestCtx, releaseRequest, err := live.Admit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	streamCtx, releaseStream, err := live.AdmitStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if live.gatherFor(11) {
		t.Fatal("gathered while work was still running")
	}
	select {
	case <-streamCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("gathering didn't interrupt the stream")
	}
	if requestCtx.Err() != nil {
		t.Fatal("gathering cancelled a request within its grace")
	}
	releaseStream()
	releaseRequest()
	if !live.gatherFor(11) {
		t.Fatal("not gathered once everything finished")
	}
}

func TestGatheringCancelsRequestsPastTheGrace(t *testing.T) {
	live := readyFlock()
	ctx, release, err := live.Admit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	live.gatherFor(11)
	live.mu.Lock()
	live.gatherStarted = time.Now().Add(-gatherGrace - time.Second)
	live.mu.Unlock()
	live.gatherFor(11)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("a request past the gathering's grace wasn't cancelled")
	}
}

func TestGroundingRefusesHeldWork(t *testing.T) {
	live := readyFlock()
	live.gatherFor(11)
	refused := make(chan error, 1)
	go func() {
		_, _, err := live.Admit(context.Background())
		refused <- err
	}()
	time.Sleep(20 * time.Millisecond)
	live.ground()
	select {
	case err := <-refused:
		if err == nil {
			t.Fatal("held work was admitted after the goose was grounded")
		}
	case <-time.After(time.Second):
		t.Fatal("held work didn't wake when the goose was grounded")
	}
}

func TestHeldWorkGivesUpWhileTheFlockMigrates(t *testing.T) {
	live := readyFlock()
	live.gatherFor(11)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := live.Admit(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("held work whose caller went away: %v", err)
	}
}

// A goose that stops flying lets its work in flight finish: it's grounded
// when it lands, or when its last honk runs out, not when it stops honking.
func TestStoppingFlyingLetsWorkInFlightFinish(t *testing.T) {
	live := readyFlock()
	work, release, err := live.Admit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	stopping, stop := context.WithCancel(context.Background())
	stop()
	live.Fly(stopping)
	if work.Err() != nil || !live.Ready() {
		t.Fatal("stopping flying cancelled the work in flight")
	}
	live.ground()
	if work.Err() == nil {
		t.Fatal("grounding left the work in flight running")
	}
}

func TestRetriesWaitTwiceAsLongUpToALimit(t *testing.T) {
	for failures, want := range map[int]time.Duration{1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute, 5: 8 * time.Minute, 6: 10 * time.Minute, 40: 10 * time.Minute} {
		if got := retryIn(failures); got != want {
			t.Errorf("after %d failures: %s, expected %s", failures, got, want)
		}
	}
}
