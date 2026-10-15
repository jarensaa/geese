package geesehttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/jarensaa/geese"
)

type said struct {
	capabilities geese.Capabilities
	err          error
}

func (s said) Capabilities(context.Context) (geese.Capabilities, error) { return s.capabilities, s.err }

func (s said) Require(_ context.Context, name string) error {
	if s.err != nil {
		return s.err
	}
	if !slices.Contains(s.capabilities.Active, name) {
		return geese.ErrInactive
	}
	return nil
}

func serve(handler http.Handler, method string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(method, "/api/capabilities", nil))
	return response
}

func TestTheCapabilitiesEndpoint(t *testing.T) {
	response := serve(Capabilities(said{capabilities: geese.Capabilities{Active: []string{"comments"}, OldestPage: "8.0.0"}}), http.MethodGet)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("answer: %d %v", response.Code, response.Header())
	}
	if body := response.Body.String(); body != `{"capabilities":["comments"],"oldestPage":"8.0.0"}`+"\n" {
		t.Fatalf("body: %s", body)
	}
	// Before anything is on, and before a floor is set: an empty list, and no oldest page.
	if body := serve(Capabilities(said{}), http.MethodGet).Body.String(); body != `{"capabilities":[]}`+"\n" {
		t.Fatalf("body: %s", body)
	}
	unavailable := serve(Capabilities(said{err: geese.ErrGrounded}), http.MethodGet)
	var problem Problem
	if err := json.Unmarshal(unavailable.Body.Bytes(), &problem); err != nil || unavailable.Code != http.StatusServiceUnavailable || problem.Code != FeatureUnavailable {
		t.Fatalf("unreadable: %d %s", unavailable.Code, unavailable.Body.String())
	}
	if response := serve(Capabilities(said{}), http.MethodPost); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("a POST: %d", response.Code)
	}
}

func TestRequireServesOnlyWhatsOn(t *testing.T) {
	served := 0
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served++ })
	if response := serve(Require(said{capabilities: geese.Capabilities{Active: []string{"comments"}}}, "comments", next), http.MethodGet); response.Code != http.StatusOK || served != 1 {
		t.Fatalf("an active capability: %d, served %d", response.Code, served)
	}
	for name, reader := range map[string]said{"off": {}, "unreadable": {err: errors.New("down")}} {
		response := serve(Require(reader, "comments", next), http.MethodPost)
		var problem Problem
		if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil || response.Code != http.StatusServiceUnavailable || problem.Code != FeatureUnavailable || served != 1 {
			t.Fatalf("%s: %d %s, served %d", name, response.Code, response.Body.String(), served)
		}
	}
}
