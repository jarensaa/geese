// Package geesehttp admits HTTP requests to the flock: each request is work
// the goose serving it admits, so a migration waits for it, and a goose that
// loses its place cancels it.
package geesehttp

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jarensaa/geese"
)

// Admission admits work, as a *geese.Flock does. A stream, such as a socket
// or a stream of server-sent events, is interrupted by a migration instead of
// waited for.
type Admission interface {
	Admit(context.Context) (context.Context, func(), error)
	AdmitStream(context.Context) (context.Context, func(), error)
}

// Guard admits every request before next serves it: a WebSocket upgrade or a
// request for server-sent events as a stream, everything else as work a
// migration waits for. A request the flock can't admit is answered 503: the
// goose is grounded, or the flock is migrating and held it too long.
func Guard(admission Admission, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		admit := admission.Admit
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || strings.TrimSpace(r.Header.Get("Accept")) == "text/event-stream" {
			admit = admission.AdmitStream
		}
		ctx, release, err := admit(r.Context())
		if err != nil {
			if r.Context().Err() != nil {
				// The client went away while its request was held.
				return
			}
			w.Header().Set("Retry-After", "1")
			message := "Unavailable for a moment: this server has no place in its flock"
			if errors.Is(err, geese.ErrMigrating) {
				message = "Unavailable for a moment: the service is migrating"
			}
			http.Error(w, message, http.StatusServiceUnavailable)
			return
		}
		defer release()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
