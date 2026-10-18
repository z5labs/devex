package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"dagger/kubernetes/internal/dagger"
)

// adminConfPort is the TCP port the control-plane bootstrap unit
// serves a copy of /etc/kubernetes/admin.conf on — the module
// runtime fetches the admin kubeconfig from this port because Dagger
// has no cross-service exec primitive that could otherwise extract a
// file from a running Service.
const adminConfPort = 9999

// fixedBootstrapToken is the kubeadm bootstrap token used by every
// worker in this cluster to join the control-plane. Hard-coded to a
// well-formed value (token IDs must match `[a-z0-9]{6}` and secrets
// must match `[a-z0-9]{16}`) because the only thing it gates is
// in-engine cross-container traffic — there is no off-host attack
// surface.
const fixedBootstrapToken = "abcdef.0123456789abcdef"

// containerdSeedPath is where each node mounts the cache volume that
// backs its containerd state; see buildNodeService.
const containerdSeedPath = "/var/lib/containerd-volume"

// seedEntrypointPath is the node's PID 1 wrapper; see
// renderSeedEntrypoint.
const seedEntrypointPath = "/usr/local/bin/kind-seed-entrypoint.sh"

// Cluster represents a running Kubernetes cluster: a single
// control-plane node plus N worker nodes, each backed by a privileged
// kindest/node Dagger Service. Holds references to every service so
// callers can bind them into their own containers or open a client-go
// Client against them.
type Cluster struct {
	// +private
	ControlPlaneSvc *dagger.Service
	// +private
	ControlPlaneHost string
	// +private
	WorkerSvcs []*dagger.Service
	// +private
	WorkerHosts []string
}

// KindCluster spins up a Kubernetes cluster of one control-plane node
// plus `workers` worker nodes, each running upstream kindest/node as
// a privileged Dagger Service. Kind always issues TLS internally; the
// auto-generated admin kubeconfig (Cluster.Kubeconfig) is the sole
// security material in this story.
//
// Image: `<registry>/kindest/node:<tag>` — the `kindest/node` portion
// is fixed; only `registry` and `tag` are caller-overridable.
//
// Rejected inputs (each surfaces a descriptive error rather than
// booting a half-broken cluster):
//
//   - `controlPlanes != 1` — HA control-plane requires an HAProxy LB
//     in front of N kube-apiservers; that lands in a follow-up story.
//   - `workers < 0`.
//
// Session-cached so chained method calls on the returned cluster
// (e.g. Client.Apply → Client.Get) observe the SAME underlying
// services. Every method on *Cluster carries +cache="never" on its
// own line so any data-returning call re-executes per invocation.
//
// `clusterName` is a caller-supplied discriminator that folds into
// the session cache key. Parallel test suites should pass a unique
// value per test (e.g. the test function name) so each test gets its
// own backing services.
//
// +cache="session"
func (k *Kubernetes) KindCluster(
	ctx context.Context,
	// +default=""
	clusterName string,
	// +default=1
	controlPlanes int,
	// +default=0
	workers int,
	// +default="docker.io"
	registry string,
	// +default="v1.31.0"
	tag string,
) (*Cluster, error) {
	if controlPlanes != 1 {
		return nil, fmt.Errorf(
			"only single control-plane clusters are supported in this story (got controlPlanes=%d); HA control-plane lands in a follow-up",
			controlPlanes,
		)
	}
	if workers < 0 {
		return nil, fmt.Errorf("workers must be >= 0, got %d", workers)
	}

	image := fmt.Sprintf("%s/kindest/node:%s", registry, tag)

	// Per-cluster hostname suffix so parallel test invocations of
	// freshCluster() with distinct cluster names get fully independent
	// hostnames and don't collide on `cp`. Identical-arg calls in one
	// engine session hit the +cache="session" entry and reuse the
	// same backing services — preserves cluster state across chained
	// Client.Apply -> Client.Get.
	//
	// Total hostname length is kept <= 22 chars because sethostname(2)
	// from runc init fails EINVAL on longer values in this engine
	// setup (empirically confirmed; dgraph hits the same ceiling).
	keyBytes := sha256.Sum256(fmt.Appendf(nil, "%s|%d|%d|%s",
		clusterName, controlPlanes, workers, image,
	))
	hostSuffix := hex.EncodeToString(keyBytes[:6]) // 12 hex chars

	// Random, unlike hostSuffix, so each cluster instance gets fresh
	// containerd volumes; +cache="session" keeps it stable for chained
	// calls within one session.
	volumeBytes := make([]byte, 8)
	if _, err := rand.Read(volumeBytes); err != nil {
		return nil, fmt.Errorf("generate containerd volume key: %w", err)
	}
	volumeKey := hex.EncodeToString(volumeBytes)

	cpHost := "k8s-cp-" + hostSuffix
	cpSvc := buildNodeService(image, "control-plane", cpHost, cpHost, volumeKey, nil, "").
		WithHostname(cpHost)

	workerHosts := make([]string, workers)
	workerSvcs := make([]*dagger.Service, workers)
	for i := 0; i < workers; i++ {
		wHost := fmt.Sprintf("k8s-w%d-%s", i, hostSuffix)
		workerHosts[i] = wHost
		workerSvcs[i] = buildNodeService(image, "worker", wHost, cpHost, volumeKey, cpSvc, cpHost).
			WithHostname(wHost)
	}

	return &Cluster{
		ControlPlaneSvc:  cpSvc,
		ControlPlaneHost: cpHost,
		WorkerSvcs:       workerSvcs,
		WorkerHosts:      workerHosts,
	}, nil
}

// buildNodeService produces a privileged kindest/node Service running
// /sbin/init via kind's stock entrypoint, with a oneshot systemd unit
// that runs kubeadm (init for the control-plane, join for workers)
// after the system reaches multi-user.target.
//
// The wrapper-script approach (script as PID 1 with backgrounded
// systemd) does not work: systemd refuses to function unless it is
// itself PID 1, and kubeadm relies on systemd for kubelet + cgroup
// setup. Running kubeadm as a oneshot unit lets us reuse the same
// systemd that kubelet uses.
func buildNodeService(image, role, nodeHost, cpHost, volumeKey string, bindSvc *dagger.Service, bindHost string) *dagger.Service {
	script := renderBootstrapScript()
	unit := renderBootstrapUnit(role, nodeHost, cpHost)
	ctr := dag.Container().
		From(image).
		// Pin the node's containerd to the kernel overlayfs
		// snapshotter. kind's entrypoint would otherwise pick
		// fuse-overlayfs inside a user namespace, and /dev/fuse is
		// not propagated into Dagger execs, so it crash-loops; the
		// native snapshotter works but copies every layer, which made
		// image unpacks ~5x slower.
		//
		// overlayfs needs its upper/work dirs on a filesystem that is
		// not itself overlay, and an exec's rootfs (and a cache volume
		// seeded with Source) is overlay. An empty cache volume is not,
		// so containerdSeedPath holds containerd's state and
		// seedEntrypointPath copies the image's preloaded images into
		// it and binds it over /var/lib/containerd before kind's
		// entrypoint runs.
		WithEnvVariable("KIND_EXPERIMENTAL_CONTAINERD_SNAPSHOTTER", "overlayfs").
		// volumeKey is unique per cluster instance: hostnames are
		// deterministic per clusterName, and reusing a volume across
		// sessions would boot a fresh kubeadm against the previous
		// run's containerd state.
		WithMountedCache(containerdSeedPath, dag.CacheVolume("kubernetes-containerd-"+nodeHost+"-"+volumeKey)).		WithNewFile(seedEntrypointPath, renderSeedEntrypoint(),
			dagger.ContainerWithNewFileOpts{Permissions: 0o755}).
		// Patch kind's entrypoint to skip enable_network_magic (which
		// tries to rewrite /etc/resolv.conf — read-only when running
		// in a nested user namespace). Kind's network magic is for
		// talking to docker's embedded DNS; in Dagger sessions DNS
		// comes from the engine's service DNS, not docker, so the
		// magic is unnecessary.
		WithExec([]string{"sed", "-i", "s/^enable_network_magic$/log_info \"[patched] skipping enable_network_magic in nested userns\"/", "/usr/local/bin/entrypoint"}).
		// Patch kind's iptables mode detection so a tie goes to nft.
		// It counts existing rules per backend and picks legacy when
		// they are equal, and a Dagger exec's fresh netns has none in
		// either. Legacy then fails outright — the host kernel has no
		// ip_tables tables loaded and a user namespace cannot load
		// them — so kube-proxy programs nothing, the kubernetes Service
		// VIP is unreachable, kindnet never writes its CNI config, and
		// the node stays NotReady. The grep fails the build if a new
		// kindest/node release reworded the line.
		WithExec([]string{"sh", "-c", `sed -i 's/if \[ "${num_legacy_lines}" -ge "${num_nft_lines}" \]; then/if [ "${num_legacy_lines}" -gt "${num_nft_lines}" ]; then/' /usr/local/bin/entrypoint && grep -q '"${num_legacy_lines}" -gt "${num_nft_lines}"' /usr/local/bin/entrypoint`}).
		// Stop runc creating a session keyring per container. On a
		// rootless engine every container of every node is the same
		// host UID, whose kernel key quota defaults to 200; a few
		// clusters at once exhaust it and runc fails with "unable to
		// create session key: disk quota exceeded" (measured with the
		// test suite's parallel fan-out). Kubernetes workloads don't
		// rely on a fresh session keyring.
		WithExec([]string{"sh", "-c", `sed -i 's/^\( *\)SystemdCgroup = true$/&\n\1NoNewKeyring = true/' /etc/containerd/config.toml && grep -q 'NoNewKeyring = true' /etc/containerd/config.toml`}).
		WithNewFile("/usr/local/bin/kind-bootstrap.sh", script,
			dagger.ContainerWithNewFileOpts{Permissions: 0o755}).
		WithNewFile("/etc/systemd/system/kind-bootstrap.service", unit,
			dagger.ContainerWithNewFileOpts{Permissions: 0o644}).
		// Symlink (not regular file) into the wants/ directory so
		// systemd treats the unit as a wants-dependency of
		// multi-user.target. A regular file at the same path is
		// silently ignored.
		WithExec([]string{"ln", "-sf",
			"/etc/systemd/system/kind-bootstrap.service",
			"/etc/systemd/system/multi-user.target.wants/kind-bootstrap.service"})
	// adminConfPort gates Service.Start on every node: each node's
	// bootstrap opens it when it finishes, on success and on failure
	// alike (see renderBootstrapScript), so Start returns once the
	// node has joined or failed, and Cluster.start then reads which.
	// A port a node never binds must not be declared — that is what
	// once left a worker's Start polling until the session was
	// cancelled (a 13m+ hang in CI) — so 6443, which only the
	// control-plane serves and which never opens if kubeadm init
	// fails, skips the health check; waitForHealthz checks it instead.
	if role == "control-plane" {
		ctr = ctr.WithExposedPort(6443, dagger.ContainerWithExposedPortOpts{ExperimentalSkipHealthcheck: true})
	}
	ctr = ctr.WithExposedPort(adminConfPort)
	if bindSvc != nil {
		ctr = ctr.WithServiceBinding(bindHost, bindSvc)
	}
	// The seed script execs kind's own entrypoint, which execs
	// /sbin/init, so systemd still ends up as PID 1.
	return ctr.AsService(dagger.ContainerAsServiceOpts{
		Args:                     []string{seedEntrypointPath},
		InsecureRootCapabilities: true,
		NoInit:                   true,
	})
}

// renderSeedEntrypoint returns the node's PID 1 wrapper: on a fresh
// volume it copies the image's /var/lib/containerd (content store,
// metadata and the overlayfs snapshots of kind's preloaded images)
// into containerdSeedPath, binds that over /var/lib/containerd, and
// execs kind's stock entrypoint.
func renderSeedEntrypoint() string {
	return fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail

SEED=%q

if [[ -z "$(ls -A "$SEED")" ]]; then
    echo "INFO: seeding containerd state into $SEED"
    cp -a /var/lib/containerd/. "$SEED"/
fi
mount --bind "$SEED" /var/lib/containerd

exec /usr/local/bin/entrypoint /sbin/init
`, containerdSeedPath)
}

// APIServerEndpoint returns the host:port pair the cluster's
// kube-apiserver advertises on its external HTTPS listener (port
// 6443). The hostname is a per-cluster Dagger WithHostname alias,
// reachable from any container in the same engine session.
//
// +cache="never"
func (c *Cluster) APIServerEndpoint(ctx context.Context) (string, error) {
	if err := c.start(ctx); err != nil {
		return "", err
	}
	return c.ControlPlaneHost + ":6443", nil
}

// HealthzResponse curls https://<APIServerEndpoint>/healthz from a
// sidecar container bound to the control-plane service, and returns
// the response body. Useful as an end-to-end probe that the cluster's
// kube-apiserver is up, the WithHostname-registered alias resolves
// in-session, and the auto-generated apiserver cert is valid for the
// hostname (added to certSANs at kubeadm init).
//
// Returns the body (expected `ok`) on a 2xx; surfaces curl's error on
// transport failure or non-2xx status.
//
// Lives inside the kubernetes module because Dagger Service handles
// returned through GraphQL boundaries do not preserve the binding
// state needed for cross-module WithServiceBinding — the binding
// must be applied in the same chain that produces the exec.
//
// +cache="never"
func (c *Cluster) HealthzResponse(ctx context.Context) (string, error) {
	if err := c.start(ctx); err != nil {
		return "", err
	}
	return dag.Container().
		From("curlimages/curl:8.10.1").
		WithServiceBinding(c.ControlPlaneHost, c.ControlPlaneSvc).
		WithEnvVariable("CACHEBUST", strconv.FormatInt(time.Now().UnixNano(), 10)).
		WithExec([]string{
			"curl", "-sk", "--max-time", "10",
			fmt.Sprintf("https://%s:6443/healthz", c.ControlPlaneHost),
		}).Stdout(ctx)
}

// Stop tears down every service container backing this cluster (the
// control-plane plus every worker). SIGKILL skips graceful shutdown
// — kubeadm drain timeouts a torn-down test cluster doesn't need.
//
// +cache="never"
func (c *Cluster) Stop(ctx context.Context) error {
	opts := dagger.ServiceStopOpts{Kill: true}
	var errs []error
	if c.ControlPlaneSvc != nil {
		if _, err := c.ControlPlaneSvc.Stop(ctx, opts); err != nil {
			errs = append(errs, fmt.Errorf("stop control-plane: %w", err))
		}
	}
	for i, svc := range c.WorkerSvcs {
		if svc == nil {
			continue
		}
		if _, err := svc.Stop(ctx, opts); err != nil {
			errs = append(errs, fmt.Errorf("stop worker %d: %w", i, err))
		}
	}
	return errors.Join(errs...)
}

// start explicitly Starts the control-plane and every worker so the
// WithHostname aliases become session-reachable from the kubernetes
// module runtime, then polls the API server's /healthz until ready.
// kubeadm init takes 30-90s in a kind container, so the deadline is
// generous.
func (c *Cluster) start(ctx context.Context) error {
	if c.ControlPlaneSvc != nil {
		if _, err := c.ControlPlaneSvc.Start(ctx); err != nil {
			return fmt.Errorf("start control-plane: %w", err)
		}
		if err := bootstrapFailure(ctx, "control-plane", c.ControlPlaneHost, c.ControlPlaneSvc); err != nil {
			return err
		}
	}
	for i, svc := range c.WorkerSvcs {
		if _, err := svc.Start(ctx); err != nil {
			return fmt.Errorf("start worker %d: %w", i, err)
		}
		if err := bootstrapFailure(ctx, fmt.Sprintf("worker %d", i), c.WorkerHosts[i], svc); err != nil {
			return err
		}
	}
	if err := waitForHealthz(ctx, c.ControlPlaneSvc, c.ControlPlaneHost); err != nil {
		return fmt.Errorf("control-plane (%s) not ready: %w", c.ControlPlaneHost, err)
	}
	return nil
}

// bootstrapFailure returns a node's bootstrap log as an error when
// its bootstrap failed. A failed bootstrap serves bootstrap-failed.log
// in place of admin.conf (control-plane) or the joined marker
// (worker), and adminConfPort only opens once it has finished either
// way, so a single probe after Service.Start is conclusive.
func bootstrapFailure(ctx context.Context, role, host string, svc *dagger.Service) error {
	out, err := dag.Container().
		From("curlimages/curl:8.10.1").
		WithServiceBinding(host, svc).
		WithEnvVariable("CACHEBUST", strconv.FormatInt(time.Now().UnixNano(), 10)).
		WithExec([]string{"sh", "-c", fmt.Sprintf(
			"curl -sf --max-time 10 http://%s:%d/bootstrap-failed.log || true",
			host, adminConfPort,
		)}).
		Stdout(ctx)
	if err != nil {
		return fmt.Errorf("probe %s bootstrap status: %w", role, err)
	}
	if out == "" {
		return nil
	}
	return fmt.Errorf("%s (%s) bootstrap failed:\n%s", role, host, out)
}

// waitForHealthz polls `https://<host>:6443/healthz` from a sidecar
// curl container until it returns 200 OK or the deadline passes. The
// per-iteration CACHEBUST env var defeats Dagger's container-level
// function cache so each poll actually re-executes.
func waitForHealthz(ctx context.Context, svc *dagger.Service, host string) error {
	deadline := time.Now().Add(360 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		out, err := dag.Container().
			From("curlimages/curl:8.10.1").
			WithServiceBinding(host, svc).
			WithEnvVariable("CACHEBUST", strconv.FormatInt(time.Now().UnixNano(), 10)).
			WithExec([]string{
				"curl", "-skf", "-o", "/dev/null", "-w", "%{http_code}",
				"--max-time", "5",
				fmt.Sprintf("https://%s:6443/healthz", host),
			}).
			Stdout(ctx)
		if err == nil && strings.TrimSpace(out) == "200" {
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("got status %q", strings.TrimSpace(out))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timeout")
	}
	return lastErr
}

// renderBootstrapScript returns the shell script invoked by the
// kind-bootstrap.service systemd unit. The role/cpHost arguments
// come from the unit's ExecStart line so the same script body works
// for both control-plane and worker nodes.
//
// The script:
//
//   - Control-plane: renders /kind/kubeadm.conf, runs kubeadm init,
//     applies the CNI manifest shipped with kindest/node, untaints
//     the control-plane, then serves a copy of admin.conf over HTTP
//     on adminConfPort so the dagger module runtime can extract the
//     kubeconfig without docker-exec.
//   - Worker: waits for the control-plane API port to open, then
//     runs kubeadm join.
//
// On failure the script appends recent kubelet/containerd logs to its
// own log, and the control-plane serves that log as
// bootstrap-failed.log in place of admin.conf (see Cluster.start).
func renderBootstrapScript() string {
	return fmt.Sprintf(`#!/usr/bin/env bash
set -uo pipefail

ROLE="${1:?role required}"
NODE_HOST="${2:?node host required}"
CP_HOST="${3:?control-plane host required}"
TOKEN=%q
ADMIN_PORT=%d

log() { echo "[kind-bootstrap] $*" >&2; }

# Everything this script prints also lands in LOG, so a failure can be
# handed back to the module runtime: Dagger surfaces none of a
# service's systemd journal.
LOG=/var/log/kind-bootstrap.log
exec > >(tee -a "$LOG") 2>&1

# serve_export publishes EXPORT_DIR, and nothing else, on ADMIN_PORT.
# The module treats the port opening as "bootstrap finished": it then
# finds either admin.conf or bootstrap-failed.log there.
EXPORT_DIR=/kind/export
serve_export() {
    cd "$EXPORT_DIR"
    exec python3 -m http.server $ADMIN_PORT --bind 0.0.0.0
}

# fail records why the bootstrap stopped plus the kubelet/containerd
# journal, and serves that log in place of admin.conf (control-plane)
# or the joined marker (worker) so the module returns it as its error
# rather than timing out on a port that never opens.
fail() {
    log "FATAL: $*"
    journalctl -u kubelet --no-pager -n 200 || true
    journalctl -u containerd --no-pager -n 100 || true
    mkdir -p "$EXPORT_DIR"
    cp "$LOG" "$EXPORT_DIR/bootstrap-failed.log"
    serve_export
}

# Make sure containerd is ready before kubeadm tries to use it.
log "waiting for containerd"
for i in $(seq 1 60); do
    if systemctl is-active --quiet containerd; then
        log "containerd is active"
        break
    fi
    sleep 1
done

if [[ "$ROLE" == "control-plane" ]]; then
    mkdir -p /kind

    cat > /kind/kubeadm.conf <<EOF
apiVersion: kubeadm.k8s.io/v1beta3
kind: InitConfiguration
bootstrapTokens:
- token: $TOKEN
  ttl: 24h0m0s
  usages:
  - signing
  - authentication
localAPIEndpoint:
  advertiseAddress: 0.0.0.0
  bindPort: 6443
nodeRegistration:
  criSocket: unix:///run/containerd/containerd.sock
  # Pin the registered node name to the per-cluster hostname instead
  # of the container's default 'debuerreotype'; otherwise every
  # kindest/node in this engine reports the same name and worker
  # joins collide with the control-plane node object.
  name: $NODE_HOST
---
apiVersion: kubeadm.k8s.io/v1beta3
kind: ClusterConfiguration
controlPlaneEndpoint: $CP_HOST:6443
apiServer:
  certSANs:
  - $CP_HOST
  - $NODE_HOST
  - 127.0.0.1
  - localhost
networking:
  serviceSubnet: 10.96.0.0/16
  podSubnet: 10.244.0.0/16
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
# Host has zram swap enabled; kubelet refuses to start with swap on
# by default. Disable the swap-on guard so kubeadm init can finish.
failSwapOn: false
cgroupDriver: systemd
# When the engine runs under a non-initial user namespace (rootless
# podman, etc.) kubelet's default oomWatcher fails to open /dev/kmsg
# because the kernel hides it from nested namespaces. The feature gate
# tells kubelet to skip kmsg-based OOM tracking instead of refusing
# to start.
featureGates:
  KubeletInUserNamespace: true
---
apiVersion: kubeproxy.config.k8s.io/v1alpha1
kind: KubeProxyConfiguration
# kube-proxy otherwise raises net/netfilter/nf_conntrack_max at
# startup, which a user namespace may not write, and exits; with no
# kube-proxy the kubernetes Service VIP is unreachable and the node
# never goes Ready. 0 leaves the sysctl alone, as kind does rootless.
conntrack:
  maxPerCore: 0
EOF

    log "running kubeadm init"
    if ! kubeadm init --config=/kind/kubeadm.conf --skip-phases=preflight --ignore-preflight-errors=all; then
        fail "kubeadm init failed"
    fi

    if [[ ! -f /kind/manifests/default-cni.yaml ]]; then
        fail "kindest/node image is missing /kind/manifests/default-cni.yaml"
    fi
    log "templating + applying default CNI manifest"
    # kind's default-cni.yaml ships with Go-template placeholders
    # (kindest/node v1.31.0 spells it {{ .PodSubnet }}) that the kind
    # CLI normally renders. We don't have the kind CLI; substitute the
    # same value our kubeadm.conf declares, with or without the spaces.
    sed 's|{{ *\.PodSubnet *}}|10.244.0.0/16|g' /kind/manifests/default-cni.yaml > /kind/manifests/default-cni.rendered.yaml
    if grep -q '{{' /kind/manifests/default-cni.rendered.yaml; then
        fail "default-cni.yaml has template placeholders left unrendered: $(grep -o '{{[^}]*}}' /kind/manifests/default-cni.rendered.yaml | sort -u | tr '\n' ' ')"
    fi
    if ! kubectl --kubeconfig=/etc/kubernetes/admin.conf apply -f /kind/manifests/default-cni.rendered.yaml; then
        fail "kubectl apply default-cni.yaml failed"
    fi

    log "waiting for node $NODE_HOST to report Ready"
    # A ready kindnet pod is not enough: the node stays NotReady
    # ("cni plugin not initialized") until kindnet has written its CNI
    # config, and nothing schedules onto it until then. Waiting on the
    # node itself is the condition every caller actually needs.
    if ! kubectl --kubeconfig=/etc/kubernetes/admin.conf wait \
        --for=condition=Ready "node/$NODE_HOST" --timeout=180s; then
        kubectl --kubeconfig=/etc/kubernetes/admin.conf -n kube-system logs ds/kindnet --tail=100 || true
        kubectl --kubeconfig=/etc/kubernetes/admin.conf -n kube-system logs ds/kube-proxy --previous --tail=50 || true
        kubectl --kubeconfig=/etc/kubernetes/admin.conf describe node "$NODE_HOST" || true
        ls -la /etc/cni/net.d || true
        fail "node $NODE_HOST never became Ready"
    fi

    log "removing control-plane taint"
    kubectl --kubeconfig=/etc/kubernetes/admin.conf taint nodes --all node-role.kubernetes.io/control-plane- || true

    log "serving admin.conf on tcp/$ADMIN_PORT via python3 http.server"
    # kindest/node ships python3 but not socat/ncat; use python's
    # builtin http.server so the module runtime can curl admin.conf
    # from a sidecar container. Only a copy of admin.conf is exported,
    # never /etc/kubernetes itself, which holds the cluster CA's keys.
    mkdir -p "$EXPORT_DIR"
    install -m 644 /etc/kubernetes/admin.conf "$EXPORT_DIR/admin.conf"
    serve_export
else
    log "worker: waiting for control-plane $CP_HOST:6443"
    reachable=0
    for i in $(seq 1 360); do
        if (exec 3<>/dev/tcp/$CP_HOST/6443) 2>/dev/null; then
            exec 3<&- || true
            exec 3>&- || true
            log "control-plane reachable, joining"
            reachable=1
            break
        fi
        sleep 2
    done
    if [[ "$reachable" != 1 ]]; then
        fail "control-plane $CP_HOST:6443 never became reachable"
    fi

    # Pre-write kubelet extra flags so kubelet won't refuse on swap.
    mkdir -p /var/lib/kubelet
    echo 'KUBELET_EXTRA_ARGS="--fail-swap-on=false"' > /etc/default/kubelet || true

    if ! kubeadm join $CP_HOST:6443 \
        --token $TOKEN \
        --discovery-token-unsafe-skip-ca-verification \
        --node-name $NODE_HOST \
        --ignore-preflight-errors=all; then
        fail "kubeadm join failed"
    fi

    log "waiting for node $NODE_HOST to report Ready"
    # The node's own credentials may read its own Node object, which is
    # all this needs; the control-plane's admin.conf never leaves it.
    ready=""
    for i in $(seq 1 90); do
        ready=$(kubectl --kubeconfig=/etc/kubernetes/kubelet.conf get node "$NODE_HOST" \
            -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
        if [[ "$ready" == "True" ]]; then
            break
        fi
        sleep 2
    done
    if [[ "$ready" != "True" ]]; then
        kubectl --kubeconfig=/etc/kubernetes/kubelet.conf get node "$NODE_HOST" -o yaml || true
        ls -la /etc/cni/net.d || true
        fail "node $NODE_HOST never became Ready (last Ready=$ready)"
    fi

    log "worker joined; serving joined marker on tcp/$ADMIN_PORT"
    mkdir -p "$EXPORT_DIR"
    echo "$NODE_HOST" > "$EXPORT_DIR/joined"
    serve_export
fi
`, fixedBootstrapToken, adminConfPort)
}

// renderBootstrapUnit returns the body of the systemd oneshot unit
// that invokes the bootstrap script after multi-user.target. Drops
// the script as /etc/systemd/system/kind-bootstrap.service AND
// /etc/systemd/system/multi-user.target.wants/kind-bootstrap.service
// (the second path simulates `systemctl enable` so the unit fires at
// boot without an interactive enable step).
//
// Every node's unit ends by exec'ing python3's http.server on
// adminConfPort (Type=simple), success or failure; RemainAfterExit
// keeps systemd from restarting it if that ever exits.
func renderBootstrapUnit(role, nodeHost, cpHost string) string {
	return fmt.Sprintf(`[Unit]
Description=Kind cluster bootstrap (%s)
After=containerd.service
Wants=containerd.service

[Service]
Type=simple
ExecStart=/usr/local/bin/kind-bootstrap.sh %s %s %s
Restart=no
RemainAfterExit=yes
StandardOutput=journal+console
StandardError=journal+console

[Install]
WantedBy=multi-user.target
`, role, role, nodeHost, cpHost)
}
