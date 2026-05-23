# Networking

How Dagger services, hostnames, and DNS work — and the answers to two
questions that are not written down officially: *are services scoped or
global?* and *can a container be placed in a specific network?*

> Engine version: `v0.20.8`. Source permalinks are pinned to
> `dagger/dagger` commit
> [`74bff7d`](https://github.com/dagger/dagger/tree/74bff7d10fd78dd6935c60c4514558598f216451).
> See [README.md](./README.md) for the sourcing rule.

## The service model

A `Service` is "a content-addressed service providing TCP connectivity"
—
[`core/service.go` L51-L89](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/service.go#L51-L89).
There are three kinds, dispatched by
[`Service.Start`, L296-L311](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/service.go#L296-L311):

| Kind            | Backed by                | Direction          |
| --------------- | ------------------------ | ------------------ |
| Container svc   | `Container` (`AsService`)| container↔container|
| Tunnel svc      | `TunnelUpstream`         | host → container   |
| Reverse tunnel  | `HostSockets`            | container → host   |

The everyday building blocks:

- **`WithExposedPort(port)`** — records a port on the container and adds
  it to the OCI `ExposedPorts` config —
  [`core/container.go` L2228-L2254](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/container.go#L2228-L2254).
- **`AsService(args)`** — turns a container into a `Service`, resolving
  the command from explicit args / entrypoint / `Cmd` —
  [`core/container.go` L2329+](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/container.go#L2329-L2360).
- **`WithServiceBinding(svc, alias)`** — binds a service into a
  consumer container under an `alias`, after resolving the service's
  hostname —
  [`core/container.go` L2275-L2297](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/container.go#L2275-L2297).
- **`Endpoint(port, scheme)`** — formats `host:port`, defaulting to the
  first exposed port —
  [`core/service.go` L169-L237](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/service.go#L169-L237).

This repo uses all of these: `daggerverse/kafka/cluster_kafka.go` wires
brokers together with `WithExposedPort` + `AsService` +
`WithServiceBinding`, and `daggerverse/grafana-stack/main.go` exposes
Loki/Tempo/Mimir each via a `Service()` / `Endpoint()` pair.

### Hostnames are content-addressed

A service's hostname is **not** random — it is derived from the digest
of the call ID that produced the service —
[`Service.Hostname`, L114-L142](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/service.go#L114-L142):

```go
case svc.Container != nil, // container=>container
    len(svc.HostSockets) > 0: // container=>host
    return network.HostHash(id.Digest()), nil
```

`network.HostHash` is an `xxh3` hash of the ID digest, base32-encoded —
[`network/hosts.go` L15-L22](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/network/hosts.go#L15-L22).
A custom hostname can be set with `withHostname`
([`WithHostname`, L108-L112](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/service.go#L108-L112)).

**Experiment — the hostname is deterministic.** The same pipeline run
twice produces the same hostname:

```
$ dagger -c 'container | from python:3.13-alpine | with-exposed-port 8080 |
             as-service --args="python3,-m,http.server,8080" | hostname'
9961bfn7kemr6

$ # ...run the identical command again:
9961bfn7kemr6
```

Because the hostname is a pure function of the call graph, two callers
that build the *same* service get the *same* hostname — which is also
why the engine can deduplicate them (see scoping, below).

### DNS resolution between containers

`WithServiceBinding` makes the service resolvable inside the consumer
container, both by its content-addressed hostname and by the `alias`.

**Experiment — a bound service resolves by alias.**

```
$ dagger -c 'container | from alpine:3 |
             with-service-binding web $(container | from python:3.13-alpine |
               with-exposed-port 8080 |
               as-service --args="python3,-m,http.server,8080") |
             with-exec getent hosts web | stdout'

10.87.190.29      web  web
```

The alias `web` resolves to `10.87.190.29` — an address inside the
engine's `10.87.0.0/16` range (see next section).

## Service & port scoping

**Services and their ports are scoped per Dagger session, not shared
globally.** The unit of identity is `ServiceKey` —
[`core/services.go` L74-L79](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/services.go#L74-L79):

```go
type ServiceKey struct {
    Digest    digest.Digest // the service's content-addressed ID digest
    SessionID string        // always set
    ClientID  string        // set only when clientSpecific
}
```

Every lookup, start, and stop keys on this struct, and `SessionID` is
*always* populated —
[`Get`, L94-L127](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/services.go#L94-L127)
and
[`StartWithIO`, L145-L217](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/services.go#L145-L217).
The consequences:

- **Within one session**, two requests for a service with the *same*
  digest collapse to one running instance. `StartWithIO` finds it
  already running, increments a binding count, and returns the existing
  `RunningService`
  ([L176-L180](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/services.go#L176-L180)).
  This is the mechanism behind the kafka test suite sharing one cluster
  pointer across many tests.
- **Across sessions**, nothing is shared — a different `SessionID`
  yields a different `ServiceKey`. When a session ends,
  [`StopSessionServices`, L305-L331](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/services.go#L305-L331)
  force-stops every service whose `Key.SessionID` matches. Services do
  not outlive the `dagger` invocation that created them.
- **Per-client scoping** is opt-in: `ClientID` is only added to the key
  when `clientSpecific` is true — used for host tunnels, so a tunnel is
  private to the client that opened it
  ([`StartAndTrack`, L239-L250](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/service.go#L239-L250)
  passes `clientSpecific` for tunnel services).

There is no global port registry. "Port collisions" between unrelated
services cannot happen, because each service has its own hostname/IP and
the engine never reuses a frontend port across services. The only
"collision" case is *the same service started twice* — which, as above,
is resolved by reuse rather than a second bind. Frontend ports for host
tunnels default to OS-chosen (`frontend = 0`) when unspecified —
[`startTunnel`, L827-L833](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/service.go#L827-L833).

### Same port, many containers — worked example

Multiple containers in a single function call can all `WithExposedPort`
the *same* port number without serializing. The reason is that
`WithExposedPort` writes OCI `ExposedPorts` metadata on **that specific
container's** config —
[`core/container.go` L2228-L2254](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/container.go#L2228-L2254)
— and each `Service` gets its own content-addressed hostname via
`network.HostHash(id.Digest())` —
[`Service.Hostname`, L114-L142](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/service.go#L114-L142).
A unique hostname means a unique IP on the `10.87.0.0/16` bridge, so
`<broker-A>:9092` and `<broker-B>:9092` are different sockets — same
port number, different addresses.

Take the kafka module's brokers as a concrete case. Inside one cluster,
`daggerverse/kafka/cluster_kafka.go` boots multiple brokers that all
expose `9092`, `9093`, and `19092` — they do not fight for those ports
because each broker has its own hostname (`broker-100-<suffix>`,
`broker-101-<suffix>`, …) and therefore its own IP. The same logic
extends across clusters: spinning up *two* Kafka clusters in one
function gives you 2×N brokers, each a distinct `Service` (distinct
`id.Digest()` because their cluster IDs / CAs / configs differ),
each on its own IP, all listening on `:9092` simultaneously. Nothing
runs serially; nothing waits its turn for a port.

There are two situations where this picture changes:

- **Identical content digests dedup.** If two clusters happen to
  produce *exactly* the same content digest — same image, same args,
  same env, same security material — the engine reuses one
  `RunningService` for both and increments its binding count
  ([`StartWithIO` L176-L180](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/services.go#L176-L180)).
  That is *deduplication*, not port sharing. The kafka module keeps
  clusters distinct (per-cluster CA / random suffixes) when isolation
  matters, and deliberately reuses the same cluster pointer when it
  doesn't (see `daggerverse/kafka/tests/main.go`).
- **Host tunnels can collide.** With `dagger up` you bind a port on
  the *real* host (`0.0.0.0:<frontend>`), so two `up` calls trying to
  take the same frontend port on the same host machine will collide.
  Inside the engine network there is no such restriction.

## One network, or many?

**There is a single, flat network per engine — not per-container
networks.** Containers are isolated by DNS domain, not by separate
networks, and **there is no public API to place a container into a
specific or isolated network.**

The engine has one bridge network with a fixed address range —
[`network/consts.go`](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/network/consts.go):

```go
const DomainSuffix = ".dagger.local"
const DefaultName  = "dagger"        // the bridge interface name
const DefaultCIDR  = "10.87.0.0/16"  // address range for all networked containers
```

The bridge takes the `.1` address of that CIDR —
[`network/bridge.go` L5-L16](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/network/bridge.go#L5-L16).

Isolation is achieved with **DNS search domains**, not network
segmentation:

- A **session domain** scopes every service in a session:
  `SessionDomain(sid)` = `{hash(sessionID)}.dagger.local` —
  [`network/hosts.go` L28-L31](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/network/hosts.go#L28-L31).
- A **module domain** further scopes services started by a module:
  `ModuleDomain(modID, sid)` =
  `{hash(modID)}.{hash(sessionID)}.dagger.local` —
  [`network/hosts.go` L33-L41](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/network/hosts.go#L33-L41).

When a container service starts,
[`startContainer` L377-L391](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/service.go#L377-L391)
picks the module domain if the service has a custom hostname and was
started by a module (and adds it to `ExtraSearchDomains` so the service
can still reach peers in the starting module), otherwise the session
domain. The fully-qualified name is `host + "." + domain`. So
"isolation" is two services simply not sharing a search domain — they
are still on the same `10.87.0.0/16` bridge.

**Experiment — the flat network and the session search domain.**
`/etc/resolv.conf` inside any container in a session shows both:

```
$ dagger -c 'container | from alpine:3 |
             with-service-binding web $(container | from python:3.13-alpine |
               with-exposed-port 8080 |
               as-service --args="python3,-m,http.server,8080") |
             with-exec cat /etc/resolv.conf | stdout'

nameserver 10.87.0.1
search gl4e8gk0lc05c.dagger.local
```

`nameserver 10.87.0.1` is the bridge (`.1` of `10.87.0.0/16`);
`search …​.dagger.local` is the session domain. Every container in the
session gets the same nameserver and the same flat network. A reader
who wants per-network isolation will not find a knob for it — the only
boundary available is the DNS domain, and it is chosen by the engine,
not the caller.

## Host ↔ container

### Host → container tunnels (`dagger up`)

`up` forwards a host port into a service. `Container.up` first converts
the container to a service, then calls `Service.up` —
[`core/schema/service.go` L401-L462](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/schema/service.go#L401-L462).
Under the hood it builds a tunnel service and calls
`bk.ListenHostToContainer` to bind `0.0.0.0:frontend` on the host,
forwarding to `upstream.Host:backend` —
[`startTunnel`, L788-L878](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/service.go#L788-L878).
The running tunnel's host is `127.0.0.1`, so the service becomes
reachable at `localhost:<frontend>` — `up` logs exactly that
([`L444-L456`](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/schema/service.go#L444-L456)).
A reader can reproduce this with:

```
dagger -c 'container | from python:3.13-alpine | with-exposed-port 8080 |
           as-service --args="python3,-m,http.server,8080" | up --ports 8080:8080'
# then, from the host:  curl localhost:8080
```

`up` is marked `DoNotCache` and blocks until its context is canceled
(`<-ctx.Done()`), i.e. until you Ctrl-C it.

### Container → host (reverse tunnels)

The reverse direction exposes a host socket *into* the engine network.
`startReverseTunnel` provisions a CNI network namespace
(`bk.NewNetworkNamespace`) and runs a `c2hTunnel` —
[`core/service.go` L880-L960](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/service.go#L880-L960).
Its hostname lives in the session domain like any other service.

### Service lifecycle

- **Start.** A container service implicitly starts its dependency
  bindings via `StartBindings` before its own exec —
  [`startContainer` L371-L375](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/service.go#L371-L375).
  `Service.start` can also start one explicitly and wait for health
  checks
  ([`core/schema/service.go` L341-L355](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/schema/service.go#L341-L355)).
- **Reuse.** As above, an already-running service is shared and its
  binding count incremented.
- **Teardown.** `Detach` decrements the binding count; only when it
  reaches zero does the service stop, and even then after a grace
  period —
  [`Detach`, L333-L367](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/services.go#L333-L367).
  Two timers govern this —
  [`core/services.go` L20-L29](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/services.go#L20-L29):
  `DetachGracePeriod` (10s, "avoid repeated stopping and re-starting…​in
  rapid succession") and `TerminateGracePeriod` (10s between SIGTERM and
  SIGKILL, applied by `stopGraceful`). Session end force-kills
  everything (`StopSessionServices`, `force: true`).

## Open questions / unverified

- **Engine-wide vs. container-wide bridge.** `DefaultCIDR` is a single
  `/16` for the whole engine. Whether two *separate* sessions on the
  same engine get disjoint IP sub-ranges, or simply disjoint DNS
  domains over the same address space, was not confirmed — the source
  shows one CIDR and per-session *domains*, but not per-session IP
  allocation.
- **Custom CIDR.** `DefaultCIDR` is a `const`; whether an engine
  operator can override the `10.87.0.0/16` range via engine config was
  not investigated.
- **Tunnel/`up` live run.** The `up` host-tunnel flow is documented
  from source and the schema; an end-to-end `dagger up` + host `curl`
  was not captured here because it requires a foreground process.
- **Health checks.** `Service.start` "waits for health checks to
  succeed"; the exact probe (TCP connect vs. port-open poll) was seen
  referenced (`newPortHealth`) but not traced in detail.
- **Cross-session service reachability.** Nested (child) Dagger
  sessions propagate parent search domains via `DomainSuffix`; the
  precise rules for a nested session reaching a parent's services were
  not exercised experimentally.
