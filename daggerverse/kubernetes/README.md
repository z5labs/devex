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

Prerequisites:

1. **InsecureRootCapabilities + NoInit are required** — both are set
   automatically by `KindCluster`; you don't need to configure them.
2. **On a rootless engine, delegate the `cpuset` cgroup controller**
   to the engine's user. This module has only been verified with it
   delegated, and k3s refuses to start without it (`failed to find
   cpuset cgroup (v2)`). Check with
   `cat /sys/fs/cgroup/user.slice/user-$(id -u).slice/user@$(id -u).service/cgroup.controllers`;
   a stock `user@.service` does not include `cpuset`, and a drop-in
   with `Delegate=cpu cpuset io memory pids` (then a re-login) adds it.

Everything else a rootless engine (rootless podman, verified) gets in
the way of is handled inside the module:

- **containerd uses the kernel overlayfs snapshotter.** kind would
  pick `fuse-overlayfs` in a user namespace, and Dagger does not
  propagate `/dev/fuse`; `native` works but copies every layer (~5×
  slower unpacks). overlayfs cannot put its upper dir on an exec's
  own overlay rootfs, so each node mounts an empty cache volume
  (btrfs/ext4 on the engine host), seeds it from the image's
  preloaded `/var/lib/containerd` on boot, and binds it in place.
  The volume name carries a random per-cluster key so no run inherits
  another's containerd state.
- **iptables is nft.** kind's entrypoint breaks a tie between the
  legacy and nft backends in favour of legacy, and a fresh netns has
  no rules in either; legacy then cannot even open its tables in a
  user namespace. The module patches the tie to go to nft.
- **kube-proxy leaves `nf_conntrack_max` alone**
  (`conntrack.maxPerCore: 0`), since a user namespace cannot write
  that sysctl and kube-proxy would otherwise exit at startup.

A cluster boots, with its node Ready, in about a minute.

**Mind the kernel key quota when running several clusters.** Every
container in every node is the same host UID, whose quota defaults to
200 keys (`kernel.keys.maxkeys`). With runc's per-container session
keyring left on, a few clusters exhausted it and pods failed with
`unable to create session key: disk quota exceeded`; the module now
sets containerd's `NoNewKeyring`, after which eleven clusters at once
peaked at about 150 keys (the engine's own runc keyrings and systemd's
per-unit invocation IDs remain).

**The tighter limit is inotify.** `fs.inotify.max_user_instances` is
also per host UID (512 on the machine this was measured on, 128 on
many distributions), and each cluster's systemd, kubelet and
containerd take roughly 40. With eleven clusters alive at once,
kubelets failed with `unable to create inotify: too many open files`
and nodes never went Ready. The test suite stops every cluster when
its test ends and boots at most three at a time; to run more
clusters concurrently, raise `fs.inotify.max_user_instances` on the
host, as kind recommends for multi-cluster use.

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
  user namespaces) and to prefer nft iptables (see above).
- A PID 1 wrapper that seeds and binds the containerd volume, then
  execs kind's entrypoint, which execs systemd.
- A oneshot systemd unit `/etc/systemd/system/kind-bootstrap.service`
  symlinked from `multi-user.target.wants/` so systemd boots it after
  containerd is up.
- The unit runs `kubeadm init` on the control-plane node, applies
  kindnet CNI (templated since the kind CLI normally substitutes
  `{{ .PodSubnet }}`), waits for the node to report Ready, removes
  the control-plane taint, and then serves a copy of `admin.conf`
  (never `/etc/kubernetes/` itself, which holds the CA's keys) over
  `python3 -m http.server` on port 9999 so the module runtime can
  curl it without `docker exec` (Dagger has no cross-service exec
  primitive).
- Workers run the same unit with `role=worker`, which waits for the
  control-plane's API port and then runs `kubeadm join` with
  `--discovery-token-unsafe-skip-ca-verification`.

**A failed bootstrap is an error, not a hang.** Port 9999 is the only
port `Service.Start` waits on, and the control-plane opens it on
failure too, serving `bootstrap-failed.log` (the script's output plus
the kubelet/containerd journal and, when the node never goes Ready,
kindnet and kube-proxy logs) in place of `admin.conf`. `KindCluster`'s
methods return that log as their error. Dagger does not surface a
service's systemd journal, so this is the only place it appears.

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
`time.ParseDuration`. On timeout the error carries the resource's
last observed `status.conditions`.

### Naming caveats — deliberate deviations from the story spec

- **`Get(resource, name, namespace="")`** rather than
  `Get(resource, namespace="", name)`: Dagger's Go SDK codegen
  collapses optional arguments to a single trailing `Opts` struct, so
  `name` must be a required positional argument before `namespace`.
- **`APIServerEndpoint` ↔ `ApiserverEndpoint`**: Dagger's codegen
  lowercases consecutive uppercase letters in the schema, producing
  `apiserverEndpoint`. The CLI form is `apiserver-endpoint`.

## Tests

After `dagger generate -y` at the repo root, run any of the
individual tests:

```sh
# Validation (no service plumbing — fast)
dagger -m daggerverse/kubernetes/tests call control-planes-not-one-rejected
dagger -m daggerverse/kubernetes/tests call client-apply-rejects-malformed-manifest
dagger -m daggerverse/kubernetes/tests call client-get-unknown-gvrrejected

# Cluster (each boots a real Kind cluster, ~1 minute)
dagger -m daggerverse/kubernetes/tests call defaults-produce-working-cluster
dagger -m daggerverse/kubernetes/tests call kubeconfig-yaml-server-urlmatches-endpoint
dagger -m daggerverse/kubernetes/tests call client-list-namespaces-includes-defaults
dagger -m daggerverse/kubernetes/tests call client-apply-get-delete-namespace-round-trip
dagger -m daggerverse/kubernetes/tests call client-wait-for-ready-nonexistent-image-errors
dagger -m daggerverse/kubernetes/tests call remote-client-can-list-namespaces
dagger -m daggerverse/kubernetes/tests call client-wait-for-ready-nginx-deployment
dagger -m daggerverse/kubernetes/tests call workers-join-and-list-as-nodes
dagger -m daggerverse/kubernetes/tests call bind-apiserver-allows-healthz
```

Run the whole suite as a check — `dagger call` on a `+check` exits 0
whether it passes or not:

```sh
dagger -m daggerverse/kubernetes/tests check --module tests
```

## Follow-ups

Out of scope in this story; tracked separately:

- **Talos backend** ([#87](https://github.com/z5labs/devex/issues/87))
  — same `*Cluster` / `*Client` surface, different bootstrap.
- **OIDC** for off-cluster auth.
- **Custom CA injection** for talking to private registries.
- **Ingress TLS** beyond kubeadm's auto-generated apiserver cert.
- **HA control-plane** — HAProxy LB in front of N kube-apiservers.
