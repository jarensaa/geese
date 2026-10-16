package geese

import "context"

// Admit registers a unit of work with this goose: a request, a socket, a
// message from a queue. Call release when it's done. A goose that loses its
// place cancels the returned context before the flock counts it gone, so no
// work runs on a goose the flock no longer hears.
func (f *Flock) Admit(parent context.Context) (ctx context.Context, release func(), err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Ready() {
		f.groundLocked()
		return nil, nil, ErrGrounded
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
