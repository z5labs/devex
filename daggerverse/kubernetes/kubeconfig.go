package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"dagger/kubernetes/internal/dagger"

	"gopkg.in/yaml.v3"
)

// Kubeconfig returns the admin kubeconfig for this cluster as a
// *dagger.File. The `server:` URL is rewritten to point at the
// control-plane's WithHostname alias on port 6443 — the address
// reachable from any other container in the same engine session.
//
// kubeadm originally writes the kubeconfig with
// `server: https://<container-internal-ip>:6443`, which is reachable
// only from inside the control-plane container. Rewriting to the
// WithHostname alias makes the file usable by sidecar consumers and
// by the kubernetes module runtime alike.
//
// The file is fetched by curl'ing `http://<host>:9999/admin.conf`
// from a sidecar container — the control-plane's bootstrap unit
// publishes /etc/kubernetes/ via python3's builtin http.server so
// kubeadm's output is accessible without docker-exec (Dagger has no
// cross-service exec primitive). The fetched bytes are YAML-parsed,
// the server URL is replaced, and the result is written to the
// module-runtime workspace and returned as a *dagger.File.
//
// +cache="never"
func (c *Cluster) Kubeconfig(ctx context.Context) (*dagger.File, error) {
	if err := c.start(ctx); err != nil {
		return nil, err
	}
	raw, err := c.fetchAdminConf(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch admin.conf: %w", err)
	}
	rewritten, err := rewriteKubeconfigServer(raw, c.ControlPlaneHost+":6443")
	if err != nil {
		return nil, fmt.Errorf("rewrite kubeconfig: %w", err)
	}
	return writeWorkdirBytes("kubeconfig", "admin.yaml", rewritten)
}

// fetchAdminConf curls admin.conf from the control-plane's python3
// http.server. Retries until the server starts answering — the
// bootstrap unit only opens port 9999 after kubeadm init finishes.
func (c *Cluster) fetchAdminConf(ctx context.Context) ([]byte, error) {
	deadline := time.Now().Add(180 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out, err := dag.Container().
			From("curlimages/curl:8.10.1").
			WithServiceBinding(c.ControlPlaneHost, c.ControlPlaneSvc).
			WithEnvVariable("CACHEBUST", strconv.FormatInt(time.Now().UnixNano(), 10)).
			WithExec([]string{
				"curl", "-sfS", "--max-time", "5",
				fmt.Sprintf("http://%s:%d/admin.conf", c.ControlPlaneHost, adminConfPort),
			}).
			Stdout(ctx)
		if err == nil && len(out) > 0 {
			return []byte(out), nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timeout fetching admin.conf")
	}
	return nil, lastErr
}

// rewriteKubeconfigServer replaces every `clusters[*].cluster.server`
// value with `https://<endpoint>`. yaml.v3 is used so caller-supplied
// strings are quoted/escaped correctly.
func rewriteKubeconfigServer(raw []byte, endpoint string) ([]byte, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse admin.conf: %w", err)
	}
	clustersAny, ok := doc["clusters"]
	if !ok {
		return nil, fmt.Errorf("admin.conf missing 'clusters' key")
	}
	clusters, ok := clustersAny.([]any)
	if !ok {
		return nil, fmt.Errorf("admin.conf 'clusters' is not a list")
	}
	newURL := "https://" + endpoint
	for i, cAny := range clusters {
		entry, ok := cAny.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("clusters[%d] is not a map", i)
		}
		clusterFieldAny, ok := entry["cluster"]
		if !ok {
			return nil, fmt.Errorf("clusters[%d].cluster missing", i)
		}
		clusterField, ok := clusterFieldAny.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("clusters[%d].cluster is not a map", i)
		}
		clusterField["server"] = newURL
	}
	return yaml.Marshal(doc)
}

// writeWorkdirBytes writes content into a content-addressed subdir of
// the kubernetes module runtime's scratch workdir and returns it as
// a *dagger.File. Distinct callers writing distinct content land at
// distinct paths; identical content collapses to one file
// (idempotent across re-entry). Mirrors daggerverse/kafka/util.go.
func writeWorkdirBytes(label, name string, content []byte) (*dagger.File, error) {
	sum := sha256.Sum256(content)
	dir := "k8s-" + label + "-" + hex.EncodeToString(sum[:8])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return nil, fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("write %s: %w", tmpPath, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("rename %s -> %s: %w", tmpPath, path, err)
	}
	return dag.CurrentModule().WorkdirFile(path), nil
}
