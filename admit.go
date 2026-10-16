package geese

import (
	"context"
	"log/slog"
	"time"
)

// Admit registers a unit of work with this goose: a request, a socket, a
// message from a queue. Call release when it's done. A goose that loses its
// place cancels the returned context before the flock counts it gone, so no
// work runs on a goose the flock no longer hears. While the flock roosts for a
// migration with downtime, Admit refuses work with ErrRoosting.
func (f *Flock) Admit(parent context.Context) (ctx context.Context, release func(), err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Ready() {
		f.groundLocked()
		return nil, nil, ErrGrounded
	}
	if f.roostingFor != 0 {
		return nil, nil, ErrRoosting
	}
	ctx, cancel := context.WithCancel(parent)
	f.nextRequest++
	id := f.nextRequest
	f.requests[id] = cancel
	return ctx, func() {
		f.mu.Lock()
		delete(f.requests, id)
		f.mu.Unlock()
		cancel()
	}, nil
}

// roost takes this goose down for the migration to target, which asked for
// downtime: it admits nothing new, and cancels work still running after
// roostGrace. It reports whether nothing admitted is still running.
func (f *Flock) roost(target int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.roostingFor != target {
		f.roostingFor, f.roostedAt = target, time.Now()
		slog.Info("geese: down for a migration", "target", target)
	}
	if time.Since(f.roostedAt) > roostGrace {
		for _, cancel := range f.requests {
			cancel()
		}
	}
	return len(f.requests) == 0
}

// wake brings this goose back once the flock no longer roosts.
func (f *Flock) wake() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.roostingFor != 0 {
		f.roostingFor = 0
		slog.Info("geese: serving again")
	}
}
