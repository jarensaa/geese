// Package kube is the weather of a flock that Kubernetes rolls out: calm once
// the Deployment has rolled out every replica, so no goose of an older image is
// still coming. The service account needs get on that Deployment.
package kube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Rollout is a Deployment's rollout, as weather: calm when every replica is
// updated and ready. A *Rollout is a geese.Weather.
type Rollout struct {
	Client                             *http.Client
	Host, Namespace, Deployment, Token string
	TokenPath                          string
}

// InCluster is the rollout of a Deployment in the cluster this process runs
// in, read with its service account. Its namespace usually comes from the
// downward API (metadata.namespace), and the Deployment's name from the
// process's environment.
func InCluster(namespace, deployment string) (*Rollout, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	if host == "" || namespace == "" || deployment == "" {
		return nil, fmt.Errorf("kube: a rollout needs the Kubernetes API's host, a namespace and a deployment")
	}
	tokenPath := "/var/run/secrets/kubernetes.io/serviceaccount/token"
	_, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("kube: the service account's CA is invalid")
	}
	port := os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
	if port == "" {
		port = "443"
	}
	return &Rollout{Client: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}, Host: "https://kubernetes.default.svc:" + port, Namespace: namespace, Deployment: deployment, TokenPath: tokenPath}, nil
}

func (g *Rollout) get(ctx context.Context, path string, value any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.Host+path, nil)
	if err != nil {
		return err
	}
	token := g.Token
	if g.TokenPath != "" {
		contents, readErr := os.ReadFile(g.TokenPath)
		if readErr != nil {
			return readErr
		}
		token = strings.TrimSpace(string(contents))
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := g.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kube: the Kubernetes API answered %s with %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(value)
}

// Calm says whether the Deployment has rolled out: its spec observed, and
// every replica updated and ready. The service account's token is read again
// for every request, as Kubernetes rotates it.
func (g *Rollout) Calm(ctx context.Context) (bool, error) {
	var deployment struct {
		Metadata struct {
			Generation int64 `json:"generation"`
		} `json:"metadata"`
		Spec struct {
			Replicas int `json:"replicas"`
		} `json:"spec"`
		Status struct {
			ObservedGeneration int64 `json:"observedGeneration"`
			UpdatedReplicas    int   `json:"updatedReplicas"`
			ReadyReplicas      int   `json:"readyReplicas"`
		} `json:"status"`
	}
	base := "/apis/apps/v1/namespaces/" + url.PathEscape(g.Namespace) + "/deployments/" + url.PathEscape(g.Deployment)
	if err := g.get(ctx, base, &deployment); err != nil {
		return false, err
	}
	settled := deployment.Metadata.Generation == deployment.Status.ObservedGeneration && deployment.Spec.Replicas >= 1 &&
		deployment.Status.UpdatedReplicas == deployment.Spec.Replicas && deployment.Status.ReadyReplicas == deployment.Spec.Replicas
	return settled, nil
}
