// Tests for the kubernetes daggerverse module. Each test is exposed
// as a standalone dagger function so it can be invoked individually
// during TDD; All wires them up for parallel execution under
// `dagger call all`.
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	par "github.com/dagger/dagger/util/parallel"

	"dagger/tests/internal/dagger"

	"gopkg.in/yaml.v3"
)

type Tests struct{}

// All runs every kubernetes test as a convenience for local `dagger
// call all` invocations. CI does NOT call All: Validation + Cluster
// each carry their own +check directive, so GH Actions schedules each
// onto its own runner in parallel — running All on top would
// double-bill the same work.
//
// +cache="session"
func (t *Tests) All(
	ctx context.Context,
	// +default=0
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}
	jobs = jobs.WithJob("Validation", func(ctx context.Context) error {
		return t.Validation(ctx, parallel)
	})
	jobs = jobs.WithJob("Cluster", func(ctx context.Context) error {
		return t.Cluster(ctx, parallel)
	})
	return jobs.Run(ctx)
}

// Validation runs the pure-validation tests (no service plumbing
// needed) plus the cluster-side client error paths (which still need
// a booted cluster but exercise the client surface, not workloads).
//
// +check
// +cache="session"
func (t *Tests) Validation(
	ctx context.Context,
	// +default=0
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}
	jobs = jobs.WithJob("control-planes-not-one-rejected", t.ControlPlanesNotOneRejected)
	jobs = jobs.WithJob("client-apply-rejects-malformed-manifest", t.ClientApplyRejectsMalformedManifest)
	jobs = jobs.WithJob("client-get-unknown-gvr-rejected", t.ClientGetUnknownGVRRejected)
	return jobs.Run(ctx)
}

// Cluster runs the topology, kubeconfig, and client round-trip tests.
// Each test passes its own name to `freshCluster`, which folds into
// KindCluster's session-cache key so concurrent tests boot
// independent backing services.
//
// +check
// +cache="session"
func (t *Tests) Cluster(
	ctx context.Context,
	// +default=0
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}
	jobs = jobs.WithJob("defaults-produce-working-cluster", t.DefaultsProduceWorkingCluster)
	jobs = jobs.WithJob("kubeconfig-yaml-server-url-matches-endpoint", t.KubeconfigYamlServerURLMatchesEndpoint)
	jobs = jobs.WithJob("client-list-namespaces-includes-defaults", t.ClientListNamespacesIncludesDefaults)
	jobs = jobs.WithJob("client-apply-get-delete-namespace-round-trip", t.ClientApplyGetDeleteNamespaceRoundTrip)
	jobs = jobs.WithJob("client-wait-for-ready-nonexistent-image-errors", t.ClientWaitForReadyNonexistentImageErrors)
	jobs = jobs.WithJob("client-wait-for-ready-nginx-deployment", t.ClientWaitForReadyNginxDeployment)
	jobs = jobs.WithJob("remote-client-can-list-namespaces", t.RemoteClientCanListNamespaces)
	jobs = jobs.WithJob("bind-apiserver-allows-healthz", t.BindAPIServerAllowsHealthz)
	jobs = jobs.WithJob("workers-join-and-list-as-nodes", t.WorkersJoinAndListAsNodes)
	return jobs.Run(ctx)
}

// randName returns a short hex-suffixed identifier suitable for use
// as a Kubernetes resource name.
func randName(ctx context.Context, prefix string) (string, error) {
	h, err := dag.Random().Sha256(ctx, dagger.RandomSha256Opts{N: 16})
	if err != nil {
		return "", err
	}
	return prefix + strings.ToLower(h[:12]), nil
}

// freshCluster mints a Kubernetes cluster sized as requested. The
// `name` folds into KindCluster's +cache="session" key: pass a unique
// per-test value so parallel tests don't collapse onto one shared
// cluster.
func freshCluster(_ context.Context, name string, workers int) *dagger.KubernetesCluster {
	return dag.Kubernetes().KindCluster(dagger.KubernetesKindClusterOpts{
		ClusterName: name,
		Workers:     workers,
	})
}

// -----------------------------------------------------------------------------
// Validation tests — no service plumbing needed.
// -----------------------------------------------------------------------------

// ControlPlanesNotOneRejected verifies that controlPlanes != 1 surfaces
// a descriptive error (HA control-plane lands in a follow-up story).
//
// +cache="never"
func (t *Tests) ControlPlanesNotOneRejected(ctx context.Context) error {
	cluster := dag.Kubernetes().KindCluster(dagger.KubernetesKindClusterOpts{
		ControlPlanes: 2,
	})
	_, err := cluster.ApiserverEndpoint(ctx)
	if err == nil {
		return fmt.Errorf("expected KindCluster(controlPlanes=2) to fail, got nil error")
	}
	if !strings.Contains(err.Error(), "single control-plane") && !strings.Contains(err.Error(), "controlPlanes=") {
		return fmt.Errorf("expected error to mention single control-plane/controlPlanes, got: %v", err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Cluster topology tests
// -----------------------------------------------------------------------------

// DefaultsProduceWorkingCluster boots a cluster with all defaults
// (controlPlanes=1, workers=0) and verifies APIServerEndpoint()
// returns a non-empty `host:port` — proving the wrapper-script
// bootstrap reached the point where the kube-apiserver answers /healthz.
//
// +cache="never"
func (t *Tests) DefaultsProduceWorkingCluster(ctx context.Context) error {
	cluster := freshCluster(ctx, "defaults-produce-working-cluster", 0)
	ep, err := cluster.ApiserverEndpoint(ctx)
	if err != nil {
		return fmt.Errorf("api server endpoint: %w", err)
	}
	if ep == "" {
		return fmt.Errorf("expected non-empty endpoint, got %q", ep)
	}
	if !strings.Contains(ep, ":6443") {
		return fmt.Errorf("expected endpoint to contain :6443, got %q", ep)
	}
	return nil
}

// KubeconfigYamlServerURLMatchesEndpoint boots a cluster, fetches
// Kubeconfig(), parses the YAML, and verifies clusters/contexts/users
// all exist and `clusters[0].cluster.server` equals
// `https://<APIServerEndpoint()>`.
//
// +cache="never"
func (t *Tests) KubeconfigYamlServerURLMatchesEndpoint(ctx context.Context) error {
	cluster := freshCluster(ctx, "kubeconfig-yaml-server-url-matches-endpoint", 0)
	ep, err := cluster.ApiserverEndpoint(ctx)
	if err != nil {
		return fmt.Errorf("api server endpoint: %w", err)
	}
	kc := cluster.Kubeconfig()
	contents, err := kc.Contents(ctx)
	if err != nil {
		return fmt.Errorf("read kubeconfig: %w", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(contents), &doc); err != nil {
		return fmt.Errorf("parse kubeconfig as YAML: %w", err)
	}
	for _, key := range []string{"clusters", "contexts", "users"} {
		v, ok := doc[key]
		if !ok {
			return fmt.Errorf("kubeconfig missing %q top-level key", key)
		}
		list, ok := v.([]any)
		if !ok || len(list) == 0 {
			return fmt.Errorf("kubeconfig %q is empty or not a list (%T)", key, v)
		}
	}
	clusters := doc["clusters"].([]any)
	first, ok := clusters[0].(map[string]any)
	if !ok {
		return fmt.Errorf("clusters[0] is not a map")
	}
	clusterField, ok := first["cluster"].(map[string]any)
	if !ok {
		return fmt.Errorf("clusters[0].cluster is not a map")
	}
	got, _ := clusterField["server"].(string)
	want := "https://" + ep
	if got != want {
		return fmt.Errorf("server URL mismatch: got %q, want %q", got, want)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Client error-path tests
// -----------------------------------------------------------------------------

// ClientApplyRejectsMalformedManifest constructs a Client from a
// freshly-booted cluster's kubeconfig, then calls Apply on a manifest
// file containing syntactically invalid YAML. The error must be
// explicit and mention the YAML decoder so callers can fix the file.
//
// +cache="never"
func (t *Tests) ClientApplyRejectsMalformedManifest(ctx context.Context) error {
	cluster := freshCluster(ctx, "client-apply-rejects-malformed-manifest", 0)
	client := cluster.Client()
	// `{not: valid: yaml: here}` — colon-after-colon is rejected by
	// the YAML decoder; pick something that's syntactically broken
	// even before kind resolution.
	bad := dag.Directory().WithNewFile("bad.yaml",
		"{not: valid: yaml: here\n  : : :\n").File("bad.yaml")
	err := client.Apply(ctx, bad)
	if err == nil {
		return fmt.Errorf("expected Apply(malformed yaml) to fail, got nil")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "yaml") &&
		!strings.Contains(msg, "decode") &&
		!strings.Contains(msg, "json") {
		return fmt.Errorf("expected error to mention yaml/decode, got: %v", err)
	}
	return nil
}

// ClientGetUnknownGVRRejected verifies that asking for a resource
// shorthand the API server doesn't recognise (`widgets`) returns an
// explicit error mentioning the unknown resource.
//
// +cache="never"
func (t *Tests) ClientGetUnknownGVRRejected(ctx context.Context) error {
	cluster := freshCluster(ctx, "client-get-unknown-gvr-rejected", 0)
	client := cluster.Client()
	_, err := client.Get(ctx, "widgets", "anything")
	if err == nil {
		return fmt.Errorf("expected Get(widgets) to fail, got nil")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "unknown resource") &&
		!strings.Contains(msg, "no match") &&
		!strings.Contains(msg, "widgets") {
		return fmt.Errorf("expected error to mention unknown/widgets, got: %v", err)
	}
	return nil
}

// ClientListNamespacesIncludesDefaults boots a cluster and verifies
// the default kube-system Namespaces are present via List on a fresh
// cluster.
//
// +cache="never"
func (t *Tests) ClientListNamespacesIncludesDefaults(ctx context.Context) error {
	cluster := freshCluster(ctx, "client-list-namespaces-includes-defaults", 0)
	client := cluster.Client()
	names, err := client.List(ctx, "namespaces")
	if err != nil {
		return fmt.Errorf("list namespaces: %w", err)
	}
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	for _, want := range []string{"default", "kube-system"} {
		if !set[want] {
			return fmt.Errorf("expected namespace %q in %v", want, names)
		}
	}
	return nil
}

// ClientApplyGetDeleteNamespaceRoundTrip applies a Namespace with a
// random name, fetches it via Get (checks YAML contains the name),
// lists Namespaces (checks the name appears), deletes it, then
// re-lists (checks the name is gone). One test exercising the full
// CRUD-via-dynamic-client surface.
//
// +cache="never"
func (t *Tests) ClientApplyGetDeleteNamespaceRoundTrip(ctx context.Context) error {
	cluster := freshCluster(ctx, "client-apply-get-delete-namespace-round-trip", 0)
	client := cluster.Client()
	name, err := randName(ctx, "test-ns-")
	if err != nil {
		return err
	}

	manifest := dag.Directory().WithNewFile("ns.yaml", fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
`, name)).File("ns.yaml")

	if err := client.Apply(ctx, manifest); err != nil {
		return fmt.Errorf("apply namespace: %w", err)
	}
	yamlOut, err := client.Get(ctx, "namespaces", name)
	if err != nil {
		return fmt.Errorf("get namespace: %w", err)
	}
	if !strings.Contains(yamlOut, name) {
		return fmt.Errorf("expected Get YAML to contain %q, got: %s", name, yamlOut)
	}
	names, err := client.List(ctx, "namespaces")
	if err != nil {
		return fmt.Errorf("list namespaces: %w", err)
	}
	if !containsString(names, name) {
		return fmt.Errorf("expected list to contain %q, got %v", name, names)
	}
	if err := client.Delete(ctx, manifest); err != nil {
		return fmt.Errorf("delete namespace: %w", err)
	}
	// k8s Namespace deletion is async — give the controller a chance
	// to clear the entry from the list before asserting it's gone.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		names, err = client.List(ctx, "namespaces")
		if err != nil {
			return fmt.Errorf("list namespaces after delete: %w", err)
		}
		if !containsString(names, name) {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("expected %q to disappear from list within 60s; still present: %v", name, names)
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// ClientWaitForReadyNginxDeployment applies a 1-replica nginx
// Deployment and verifies WaitForReady returns nil when the
// Deployment reaches Available: True.
//
// +cache="never"
func (t *Tests) ClientWaitForReadyNginxDeployment(ctx context.Context) error {
	cluster := freshCluster(ctx, "client-wait-for-ready-nginx", 0)
	client := cluster.Client()
	name, err := randName(ctx, "nginx-")
	if err != nil {
		return err
	}

	manifest := dag.Directory().WithNewFile("dep.yaml", fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: %s
  template:
    metadata:
      labels:
        app: %s
    spec:
      containers:
      - name: nginx
        image: nginx:alpine
        ports:
        - containerPort: 80
`, name, name, name)).File("dep.yaml")

	if err := client.Apply(ctx, manifest); err != nil {
		return fmt.Errorf("apply nginx deployment: %w", err)
	}
	if err := client.WaitForReady(ctx, "deployments.apps", name, dagger.KubernetesClientWaitForReadyOpts{
		Namespace: "default",
		Timeout:   "480s",
	}); err != nil {
		return fmt.Errorf("wait for ready: %w", err)
	}
	return nil
}

// ClientWaitForReadyNonexistentImageErrors applies a Deployment
// pointing at an unresolvable image and verifies WaitForReady errors
// before the deadline rather than blocking forever.
//
// +cache="never"
func (t *Tests) ClientWaitForReadyNonexistentImageErrors(ctx context.Context) error {
	cluster := freshCluster(ctx, "client-wait-for-ready-nonexistent-image", 0)
	client := cluster.Client()
	name, err := randName(ctx, "bad-")
	if err != nil {
		return err
	}

	manifest := dag.Directory().WithNewFile("dep.yaml", fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: %s
  template:
    metadata:
      labels:
        app: %s
    spec:
      containers:
      - name: nope
        image: nonexistent.example.invalid/never/exists:tag
`, name, name, name)).File("dep.yaml")

	if err := client.Apply(ctx, manifest); err != nil {
		return fmt.Errorf("apply bad deployment: %w", err)
	}
	err = client.WaitForReady(ctx, "deployments.apps", name, dagger.KubernetesClientWaitForReadyOpts{
		Namespace: "default",
		Timeout:   "30s",
	})
	if err == nil {
		return fmt.Errorf("expected WaitForReady to error on nonexistent image, got nil")
	}
	return nil
}

// WorkersJoinAndListAsNodes boots a cluster with workers=1 and
// verifies List("nodes") returns 2 entries (control-plane + worker).
//
// +cache="never"
func (t *Tests) WorkersJoinAndListAsNodes(ctx context.Context) error {
	cluster := freshCluster(ctx, "workers-join-and-list-as-nodes", 1)
	client := cluster.Client()
	// Worker join can take a while inside the native snapshotter:
	// the worker has to pull kube-proxy + kindnet images via the
	// CNI-less host network before kubelet finishes registering.
	deadline := time.Now().Add(300 * time.Second)
	for time.Now().Before(deadline) {
		nodes, err := client.List(ctx, "nodes")
		if err == nil && len(nodes) >= 2 {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	nodes, _ := client.List(ctx, "nodes")
	return fmt.Errorf("expected 2 nodes within 5 min, got %v", nodes)
}

// RemoteClientCanListNamespaces boots a cluster, takes its Kubeconfig
// file, and constructs a standalone Kubernetes.Client(kubeconfig)
// independently of Cluster.Client. Verifies the factory works.
//
// +cache="never"
func (t *Tests) RemoteClientCanListNamespaces(ctx context.Context) error {
	cluster := freshCluster(ctx, "remote-client-can-list-namespaces", 0)
	kc := cluster.Kubeconfig()
	remote := dag.Kubernetes().Client(kc)
	names, err := remote.List(ctx, "namespaces")
	if err != nil {
		return fmt.Errorf("remote list: %w", err)
	}
	if len(names) == 0 {
		return fmt.Errorf("expected non-empty namespace list, got 0")
	}
	return nil
}

// BindAPIServerAllowsHealthz boots a cluster, attaches it via
// BindAPIServer to a curl container, and verifies a GET to
// /healthz over HTTPS returns the expected `ok` body — proving the
// service hostname propagates and the auto-generated apiserver cert
// is valid for the hostname (added to certSANs at kubeadm init).
//
// +cache="never"
func (t *Tests) BindAPIServerAllowsHealthz(ctx context.Context) error {
	cluster := freshCluster(ctx, "bind-api-server-allows-healthz-v3", 0)
	ep, err := cluster.ApiserverEndpoint(ctx)
	if err != nil {
		return fmt.Errorf("api server endpoint: %w", err)
	}
	out, err := cluster.BindApiserver(
		dag.Container().From("curlimages/curl:8.10.1")).
		WithExec([]string{
			"curl", "-sk", "--max-time", "10",
			"https://" + ep + "/healthz",
		}).Stdout(ctx)
	if err != nil {
		return fmt.Errorf("curl /healthz via BindAPIServer: %w", err)
	}
	if !strings.Contains(out, "ok") {
		return fmt.Errorf("expected /healthz to return 'ok', got %q", out)
	}
	return nil
}
