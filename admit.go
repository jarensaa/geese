package geese

import (
	"context"
	"log/slog"
	"time"
)

// Admit registers a unit of work, such as a request, with this goose. Call
// release when it's done. A goose that loses its place cancels the returned
// context before the flock counts it gone. While the flock gathers for a
// migration, Admit waits until the new schema is in place, so the work runs on
// the new schema's code; it gives up with ErrMigrating after a minute.
func (f *Flock) Admit(parent context.Context) (ctx context.Context, release func(), err error) {
	return f.admit(parent, false)
}

// AdmitStream admits long-lived work, such as a socket or a stream of
// server-sent events, which a migration interrupts instead of waiting for. Its
// client reconnects, and is admitted again once the flock has landed on the
// new schema.
func (f *Flock) AdmitStream(parent context.Context) (ctx context.Context, release func(), err error) {
	return f.admit(parent, true)
}

func (f *Flock) admit(parent context.Context, stream bool) (context.Context, func(), error) {
	var hold <-chan time.Time
	for {
		f.mu.Lock()
		if !f.Ready() {
			f.groundLocked()
			f.mu.Unlock()
			return nil, nil, ErrGrounded
		}
		if f.gatheringFor == 0 {
			break
		}
		resumed := f.resumed
		f.mu.Unlock()
		if hold == nil {
			timer := time.NewTimer(holdLimit)
			defer timer.Stop()
			hold = timer.C
		}
		select {
		case <-resumed:
		case <-parent.Done():
			return nil, nil, parent.Err()
		case <-hold:
			return nil, nil, ErrMigrating
		}
	}
	ctx, cancel := context.WithCancel(parent)
	f.nextRequest++
	requestID := f.nextRequest
	f.requests[requestID] = admission{cancel: cancel, stream: stream}
	f.mu.Unlock()
	release := func() {
		f.mu.Lock()
		delete(f.requests, requestID)
		f.mu.Unlock()
		cancel()
	}
	return ctx, release, nil
}

// gatherFor holds new work for a migration to target and interrupts streams.
// It reports whether nothing admitted is still running; work still running
// after gatherGrace is cancelled.
func (f *Flock) gatherFor(target int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gatheringFor != target {
		f.endGatheringLocked()
		f.gatheringFor, f.gatherStarted, f.resumed = target, time.Now(), make(chan struct{})
		slog.Info("geese: gathering for a migration", "target", target)
	}
	overdue := time.Since(f.gatherStarted) > gatherGrace
	for _, work := range f.requests {
		if work.stream || overdue {
			work.cancel()
		}
	}
	return len(f.requests) == 0
}

func (f *Flock) endGathering() {
	f.mu.Lock()
	f.endGatheringLocked()
	f.mu.Unlock()
}

func (f *Flock) endGatheringLocked() {
	if f.gatheringFor == 0 {
		return
	}
	f.gatheringFor = 0
	close(f.resumed)
}

func (f *Flock) gathering() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gatheringFor != 0
}
