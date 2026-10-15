package kube

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRolloutRequiresSettledDeployment(t *testing.T) {
	for _, stage := range []struct {
		name   string
		status string
		calm   bool
	}{
		{"new replicas still starting", `"replicas":3,"updatedReplicas":1,"readyReplicas":2`, false},
		// The last step of a surge: every replica the spec asks for is new and
		// ready, and an old one is still running.
		{"an old replica still running", `"replicas":3,"updatedReplicas":2,"readyReplicas":2`, false},
		{"settled", `"replicas":2,"updatedReplicas":2,"readyReplicas":2`, true},
	} {
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			status := 200
			body := ""
			if r.Header.Get("Authorization") != "Bearer test-token" {
				status = 401
			}
			switch r.URL.Path {
			case "/apis/apps/v1/namespaces/birds/deployments/flock":
				body = `{"metadata":{"generation":2},"spec":{"replicas":2},"status":{"observedGeneration":2,` + stage.status + `}}`
			default:
				status = 404
			}
			return &http.Response{StatusCode: status, Status: fmt.Sprint(status), Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})}
		gate := &Rollout{Client: client, Host: "https://kubernetes.test", Namespace: "birds", Deployment: "flock", Token: "test-token"}
		calm, err := gate.Calm(context.Background())
		if err != nil || calm != stage.calm {
			t.Errorf("%s: calm=%v err=%v, expected calm=%v", stage.name, calm, err, stage.calm)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRolloutRereadsRotatedServiceAccountToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	seen := []string{}
	gate := &Rollout{Host: "https://kubernetes.test", TokenPath: path, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = append(seen, r.Header.Get("Authorization"))
		return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	})}}
	for _, token := range []string{"first", "second"} {
		if token == "second" {
			if err := os.WriteFile(path, []byte(token), 0600); err != nil {
				t.Fatal(err)
			}
		}
		var result struct{}
		if err := gate.get(context.Background(), "/test", &result); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 2 || seen[0] != "Bearer first" || seen[1] != "Bearer second" {
		t.Fatalf("service account token was cached: %v", seen)
	}
}
