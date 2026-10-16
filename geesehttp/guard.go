// Package geesehttp is geese for net/http. Guard admits every request to the
// flock: each is work the goose serving it admits, and a goose that loses its
// place cancels it. Capabilities answers what
// the flock says to clients, and Require refuses a route whose capability
// isn't on.
package geesehttp

import (
	"context"
	"errors"
	"net/http"

	"github.com/jarensaa/geese"
)

// Admission admits work, as a *geese.Flock does.
type Admission interface {
	Admit(context.Context) (context.Context, func(), error)
}

// Guard admits every request before next serves it, sockets and streams of
// server-sent events included: a goose that loses its place cancels them. A
// request the flock can't admit, as its goose is grounded or the flock is
// down for a migration, is answered 503.
func Guard(admission Admission, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, release, err := admission.Admit(r.Context())
		if err != nil {
			w.Header().Set("Retry-After", "1")
			message := "Unavailable for a moment: this server has no place in its flock"
			if errors.Is(err, geese.ErrRoosting) {
				message = "Unavailable for a moment: the service is migrating"
			}
			http.Error(w, message, http.StatusServiceUnavailable)
			return
		}
		defer release()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
