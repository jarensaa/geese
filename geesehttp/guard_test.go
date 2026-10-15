package geesehttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jarensaa/geese"
)

type admission struct {
	admit   func(context.Context) (context.Context, func(), error)
	streams int
}

func (a *admission) Admit(ctx context.Context) (context.Context, func(), error) { return a.admit(ctx) }
func (a *admission) AdmitStream(ctx context.Context) (context.Context, func(), error) {
	a.streams++
	return a.admit(ctx)
}

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

func TestGuardSaysWhenTheFlockIsMigrating(t *testing.T) {
	handler := Guard(&admission{admit: func(context.Context) (context.Context, func(), error) {
		return nil, nil, geese.ErrMigrating
	}}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "migrating") {
		t.Fatalf("held too long during a migration: %d %q", response.Code, response.Body.String())
	}
}

func TestGuardAdmitsSocketsAsStreams(t *testing.T) {
	lease := &admission{admit: func(ctx context.Context) (context.Context, func(), error) { return ctx, func() {}, nil }}
	handler := Guard(lease, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	request := httptest.NewRequest(http.MethodGet, "/live", nil)
	request.Header.Set("Upgrade", "websocket")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if lease.streams != 1 {
		t.Fatalf("admitted %d streams; a socket, and only it, is a stream", lease.streams)
	}
}

func TestGuardAdmitsEventStreamsAsStreams(t *testing.T) {
	lease := &admission{admit: func(ctx context.Context) (context.Context, func(), error) { return ctx, func() {}, nil }}
	handler := Guard(lease, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	events := httptest.NewRequest(http.MethodGet, "/events", nil)
	events.Header.Set("Accept", "text/event-stream")
	handler.ServeHTTP(httptest.NewRecorder(), events)
	// An MCP call accepts a stream as its answer, but is a request like any other.
	call := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	call.Header.Set("Accept", "application/json, text/event-stream")
	handler.ServeHTTP(httptest.NewRecorder(), call)
	if lease.streams != 1 {
		t.Fatalf("admitted %d streams; a page's event stream, and only it, is a stream", lease.streams)
	}
}
