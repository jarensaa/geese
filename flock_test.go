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
	live := &Flock{requests: map[uint64]context.CancelFunc{}}
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
	live := &Flock{requests: map[uint64]context.CancelFunc{}}
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
	live := &Flock{requests: map[uint64]context.CancelFunc{}}
	live.ready.Store(true)
	deadline := time.Now().Add(time.Minute)
	live.deadline.Store(&deadline)
	return live
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

// A roosting goose admits nothing new, gives its work in flight a grace to
// finish, then cancels it, and flies again once the flock has migrated.
func TestRoosting(t *testing.T) {
	live := readyFlock()
	work, release, err := live.Admit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if live.roost(2) {
		t.Fatal("roosted with work in flight")
	}
	if _, _, err := live.Admit(context.Background()); !errors.Is(err, ErrRoosting) {
		t.Fatalf("admitted work while roosting: %v", err)
	}
	if work.Err() != nil {
		t.Fatal("cancelled work in flight within its grace")
	}
	live.roostedAt = time.Now().Add(-roostGrace - time.Second)
	live.roost(2)
	if work.Err() == nil {
		t.Fatal("work in flight past its grace ran on")
	}
	release()
	if !live.roost(2) {
		t.Fatal("not roosted once nothing runs")
	}
	live.wake()
	if _, again, err := live.Admit(context.Background()); err != nil {
		t.Fatalf("not admitted once awake: %v", err)
	} else {
		again()
	}
}
