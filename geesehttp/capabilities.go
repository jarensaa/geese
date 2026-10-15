package geesehttp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jarensaa/geese"
)

// Reader reads what the flock says to clients, as a *geese.Flock does.
type Reader interface {
	Capabilities(context.Context) (geese.Capabilities, error)
}

// Problem is the body of every answer here that isn't the capabilities:
// a stable code for clients to branch on, and words for people.
type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// FeatureUnavailable is the code of an answer refused because its capability
// isn't on yet, or what's on can't be read just now. A client treats it as
// "not yet", and reads the capabilities again before it tries.
const FeatureUnavailable = "feature_unavailable"

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func unavailable(w http.ResponseWriter, message string) {
	w.Header().Set("Retry-After", "5")
	writeJSON(w, http.StatusServiceUnavailable, Problem{Code: FeatureUnavailable, Message: message})
}

// Capabilities serves what the flock says to clients, for GET and HEAD:
//
//	{"capabilities": ["comments"], "oldestPage": "8.0.0"}
//
// never cached, as it changes as the flock does. Mount it where pages read it,
// such as /api/capabilities; it needs no session, and says nothing a client
// couldn't learn by trying. When it can't be read, it answers 503
// feature_unavailable, and clients keep what they knew.
func Capabilities(reader Reader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeJSON(w, http.StatusMethodNotAllowed, Problem{Code: "method_not_allowed", Message: "Read the capabilities with GET"})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		capabilities, err := reader.Capabilities(r.Context())
		if err != nil {
			if r.Context().Err() == nil && !errors.Is(err, geese.ErrGrounded) {
				slog.Error("geesehttp: read the capabilities", "error", err)
			}
			unavailable(w, "Capabilities are unavailable for a moment; try again shortly")
			return
		}
		if capabilities.Active == nil {
			capabilities.Active = []string{}
		}
		writeJSON(w, http.StatusOK, capabilities)
	})
}

// Requirer says whether a capability is on, as a *geese.Flock does.
type Requirer interface {
	Require(ctx context.Context, name string) error
}

// Require serves next only while the capability is on across the deployment,
// and otherwise answers 503 feature_unavailable: a route of a feature that
// waits for its schema, or for every goose to serve it. Handlers that serve
// several features, or decide within, ask the flock's Require instead.
func Require(requirer Requirer, capability string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := requirer.Require(r.Context(), capability)
		switch {
		case err == nil:
			next.ServeHTTP(w, r)
		case r.Context().Err() != nil:
			// The client went away.
		case errors.Is(err, geese.ErrInactive):
			unavailable(w, "This isn't on yet")
		default:
			unavailable(w, "This isn't available for a moment; try again shortly")
		}
	})
}
