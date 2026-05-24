# kubernetes

Daggerverse module that spins up a Kubernetes cluster using upstream
[Kind](https://kind.sigs.k8s.io/) (`kindest/node`) as privileged
Dagger Services, and exposes a pure-Go client (built on
[`k8s.io/client-go`](https://github.com/kubernetes/client-go)) that
targets either the local cluster or any reachable remote cluster via
its kubeconfig.

This story is **plaintext-only**, **single control-plane**, and
**arbitrary worker count**. OIDC, custom CA injection, ingress TLS,
HA control-plane (HAProxy LB in front of N kube-apiservers), Talos /
k3s backends all land in follow-up stories.

## Engine requirements

This module pushes Dagger services harder than any other daggerverse
module: it needs **systemd as PID 1** inside the node, **privileged
capabilities** (containerd + kubelet require cgroup delegation), and
**a working snapshotter** for the in-node containerd to unpack
kube-apiserver / etcd / nginx images.

Three engine-level prerequisites:

1. **InsecureRootCapabilities + NoInit are required** — both are set
   automatically by `KindCluster`; you don't need to configure them.
2. **`/dev/fuse` is not propagated by Dagger into service
   containers**, so if the engine runs in a non-initial user namespace
   (rootless podman, etc.) the in-node containerd cannot use
   `fuse-overlayfs`. The module works around this by forcing
   `KIND_EXPERIMENTAL_CONTAINERD_SNAPSHOTTER=native` — native
   snapshotter copies files between layers, slower but no `/dev/fuse`
   needed.
3. **kube-proxy + CNI image pulls are slow under native snapshotter**
   (~30–90s per image). Image-pulling workload tests (`WaitForReady`
   on real Deployments) need very generous timeouts on a rootless
   engine, or a rootful engine (docker, `sudo podman run` for the
   engine). The kubeadm bootstrap itself completes in ~1 minute.

Same constraints affect the Talos backend (#87) — see #87 for the
shared analysis.

## Cluster

Single control-plane Kind node, arbitrary workers, all listeners
using kind's auto-generated TLS material.

```go
Kubernetes.KindCluster(
    ctx,
    clusterName="", controlPlanes=1, workers=0,
    registry="docker.io", tag="v1.31.0",
) *Cluster, error

Cluster.Kubeconfig(ctx) (*dagger.File, error)
Cluster.APIServerEndpoint(ctx) (string, error)
Cluster.BindAPIServer(*dagger.Container) *dagger.Container
Cluster.Client(ctx) (*Client, error)
Cluster.Stop(ctx) error
```

### Topology constraints

- **`controlPlanes != 1`** — rejected with an explicit error; HA
  control-plane lands in a follow-up.
- **`workers < 0`** — rejected.

### Function caching

`Kubernetes.KindCluster` carries `+cache="session"`, **not**
`+cache="never"` as the original story suggested. Chained calls on
the returned cluster (e.g. `Client.Apply → Client.Get` in
`client-apply-get-delete-namespace-round-trip`) need to observe the
same backing services to preserve cluster state, and `+cache="never"`
on the generator re-spawns the cluster between method calls. Every
method on `*Cluster` and `*Client` still carries `+cache="never"` on
its own line so any data-returning call re-executes per invocation.

### Bootstrap mechanism

`KindCluster` builds a kindest/node container with:

- Kind's stock entrypoint patched to skip `enable_network_magic`
  (which tries to write to read-only `/etc/resolv.conf` in nested
  user namespaces).
- A oneshot systemd unit `/etc/systemd/system/kind-bootstrap.service`
  symlinked from `multi-user.target.wants/` so systemd boots it after
  containerd is up.
- The unit runs `kubeadm init` on the control-plane node, applies
  kindnet CNI (templated since the kind CLI normally substitutes
  `{{.PodSubnet}}`), removes the control-plane taint, and then serves
  `/etc/kubernetes/` over `python3 -m http.server` on port 9999 so the
  module runtime can curl `admin.conf` without `docker exec` (Dagger
  has no cross-service exec primitive).
- Workers run the same unit with `role=worker`, which waits for the
  control-plane's API port and then runs `kubeadm join` with
  `--discovery-token-unsafe-skip-ca-verification`.

The bootstrap script is rendered as a Go string constant; both
control-plane and worker variants share the same script with role
branching.

## Client

Pure-Go client-go-based client. No container image. Works against the
local cluster or any reachable remote cluster (Dagger session DNS
must resolve the kubeconfig's `server:` host).

```go
Kubernetes.Client(kubeconfig *dagger.File) *Client

Client.Kubeconfig() *dagger.File
Client.Apply(ctx, manifest *dagger.File) error
Client.Delete(ctx, manifest *dagger.File) error
Client.Get(ctx, resource string, name string, namespace="") (string, error)
Client.List(ctx, resource string, namespace="") ([]string, error)
Client.WaitForReady(ctx, resource string, name string, namespace="", timeout="60s") error
```

`resource` accepts kubectl-style GVR shorthand (`"namespaces"`,
`"pods"`, `"deployments.apps"`) and is resolved via discovery.
Empty `namespace` on a namespaced resource means "all namespaces"
for List / cluster-default for Apply. `Get` returns the resource
as YAML; `List` returns `metadata.name` only.

`WaitForReady` polls the resource's `Ready` condition (or
`Available` for Deployments). Timeout is parsed via
`time.ParseDuration`.

### Naming caveats — deliberate deviations from the story spec

- **`Get(resource, name, namespace="")`** rather than
  `Get(resource, namespace="", name)`: Dagger's Go SDK codegen
  collapses optional arguments to a single trailing `Opts` struct, so
  `name` must be a required positional argument before `namespace`.
- **`APIServerEndpoint` ↔ `ApiserverEndpoint`**: Dagger's codegen
  lowercases consecutive uppercase letters in the schema, producing
  `apiserverEndpoint`. The CLI form is `apiserver-endpoint`.

## Tests

After `dagger develop` in both `daggerverse/kubernetes` and
`daggerverse/kubernetes/tests`, run any of the individual tests:

```sh
# Validation (no service plumbing — fast)
dagger -m daggerverse/kubernetes/tests call control-planes-not-one-rejected
dagger -m daggerverse/kubernetes/tests call client-apply-rejects-malformed-manifest
dagger -m daggerverse/kubernetes/tests call client-get-unknown-gvrrejected

# Cluster (boots a real Kind cluster — slow on first run, ~30s on subsequent)
dagger -m daggerverse/kubernetes/tests call defaults-produce-working-cluster
dagger -m daggerverse/kubernetes/tests call kubeconfig-yaml-server-urlmatches-endpoint
dagger -m daggerverse/kubernetes/tests call client-list-namespaces-includes-defaults
dagger -m daggerverse/kubernetes/tests call client-apply-get-delete-namespace-round-trip
dagger -m daggerverse/kubernetes/tests call client-wait-for-ready-nonexistent-image-errors
dagger -m daggerverse/kubernetes/tests call remote-client-can-list-namespaces

# Workload tests (require fast image pull — best on a rootful engine)
dagger -m daggerverse/kubernetes/tests call client-wait-for-ready-nginx-deployment
dagger -m daggerverse/kubernetes/tests call workers-join-and-list-as-nodes
dagger -m daggerverse/kubernetes/tests call bind-apiserver-allows-healthz
```

`dagger -m daggerverse/kubernetes/tests call all` runs every test
serially in one engine session.

### Known env-dependent tests

These three tests require a Dagger engine that supports unprivileged
overlayfs in nested userns (rootful docker / `sudo podman` for the
engine, or kernel with userns overlayfs enabled):

- **`bind-apiserver-allows-healthz`** — DNS resolution from a
  consumer container's exec needs the service binding to propagate
  across the kubernetes-module → test session boundary; this works in
  kafka but trips on something specific to the privileged
  kindest/node service in this story.
- **`client-wait-for-ready-nginx-deployment`** — needs nginx:alpine
  to be pulled inside the cluster's containerd. Under native
  snapshotter on a rootless engine this can take 8+ minutes per pull;
  the test's 8-minute timeout is borderline.
- **`workers-join-and-list-as-nodes`** — same pull constraint
  applies to kube-proxy on the worker; join can stall on the
  CNI/kindnet rollout when the control-plane CNI itself is shaky.

The remaining 9 tests pass on a rootless engine.

## Follow-ups

Out of scope in this story; tracked separately:

- **Talos backend** ([#87](https://github.com/z5labs/devex/issues/87))
  — same `*Cluster` / `*Client` surface, different bootstrap.
- **OIDC** for off-cluster auth.
- **Custom CA injection** for talking to private registries.
- **Ingress TLS** beyond kubeadm's auto-generated apiserver cert.
- **HA control-plane** — HAProxy LB in front of N kube-apiservers.
- **Engine `/dev/fuse` propagation** — would let us use
  fuse-overlayfs and speed up image unpacking ~5×.
