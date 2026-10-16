package geesehttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jarensaa/geese"
)

type admission struct {
	admit func(context.Context) (context.Context, func(), error)
}

func (a *admission) Admit(ctx context.Context) (context.Context, func(), error) { return a.admit(ctx) }

func TestGuardAnswersUnavailableWhenGrounded(t *testing.T) {
	called := false
	handler := Guard(&admission{admit: func(context.Context) (context.Context, func(), error) {
		return nil, nil, geese.ErrGrounded
	}}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if response.Code != http.StatusServiceUnavailable || called || response.Header().Get("Retry-After") == "" {
		t.Fatalf("expected 503 without invoking handler, got status %d and called=%t", response.Code, called)
	}
}

func TestGuardServesWhatItAdmits(t *testing.T) {
	admitted := 0
	handler := Guard(&admission{admit: func(ctx context.Context) (context.Context, func(), error) {
		admitted++
		return ctx, func() {}, nil
	}}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	socket := httptest.NewRequest(http.MethodGet, "/live", nil)
	socket.Header.Set("Upgrade", "websocket")
	for _, request := range []*http.Request{socket, httptest.NewRequest(http.MethodGet, "/api/health", nil)} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("%s: %d", request.URL.Path, response.Code)
		}
	}
	if admitted != 2 {
		t.Fatalf("admitted %d of 2 requests", admitted)
	}
}
