# Container execution

What a container exec actually does inside the Dagger engine, what it
costs, how caching changes that cost, and what it means for this repo's
container-heavy CI.

> Engine version: `v0.20.8`. Source permalinks are pinned to
> `dagger/dagger` commit
> [`74bff7d`](https://github.com/dagger/dagger/tree/74bff7d10fd78dd6935c60c4514558598f216451).
> See [README.md](./README.md) for the sourcing rule.

## What `WithExec` does

`WithExec` does not run anything by itself — it appends a step to a
build graph. The actual execution happens when the resulting container
(or something derived from it, like `.stdout`) is evaluated.

`Container.WithExec` —
[`core/container_exec.go` L216-L582](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/container_exec.go#L216-L582)
— assembles the pieces of one execution:

1. **Process spec.** `metaSpec` builds an `executor.Meta` — args, env,
   cwd, user, expected exit codes —
   [L167-L201](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/container_exec.go#L167-L201).
2. **Mounts.** `bkcontainer.PrepareMounts` turns the container rootfs,
   bind mounts, cache mounts, tmpfs, and secret mounts into a BuildKit
   mount set —
   [L272-L279](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/container_exec.go#L272-L279).
3. **Secrets / services.** Secret env vars are resolved
   ([L459-L463](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/container_exec.go#L459-L463))
   and any bound services are started first
   ([L469](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/container_exec.go#L469)).
4. **Run.** It gets the BuildKit worker's executor and runs the process
   against the prepared rootfs and mounts —
   [L475-L483](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/container_exec.go#L475-L483):
   ```go
   worker := opt.Worker.(*buildkit.Worker)
   worker = worker.ExecWorker(opt.CauseCtx, *execMD)
   exec := worker.Executor()
   procInfo := executor.ProcessInfo{Meta: meta}
   _, execErr := exec.Run(ctx, "", p.Root, p.Mounts, procInfo, nil)
   ```
5. **Commit outputs.** The mutable result refs are committed back into
   the container's filesystem and mounts
   ([L485-L572](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/container_exec.go#L485-L572)).

### Mapping onto BuildKit LLB

The chain `WithExec` builds is BuildKit **LLB** (low-level build
definition). Each exec becomes an LLB `ExecOp` vertex in the solver
graph. When the solver resolves a vertex, Dagger's custom worker
recognizes the exec op and wires itself in as the executor —
[`engine/buildkit/worker.go` `ResolveOp` L138-L175](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/buildkit/worker.go#L138-L175):

```go
if execOp, ok := baseOp.Op.(*pb.Op_Exec); ok {
    ...
    return ops.NewExecOp(vtx, execOp, baseOp.Platform,
        w.workerCache, w.parallelismSem, sm, w /* executor */, w)
}
```

So a `WithExec` is: an LLB `ExecOp` → solved by the BuildKit solver →
executed by Dagger's `Worker`, which is also the executor
([`Worker.Executor()` L134-L136](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/buildkit/worker.go#L134-L136)).

### The meta mount

Every exec gets an extra **meta mount** — output ref index 1 becomes
`container.Meta`
([L356-L359](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/container_exec.go#L356-L359)).
It captures stdout, stderr, combined output, the exit code, and the
client ID, each read back as a file —
[L593-L621](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/container_exec.go#L593-L621).
That is why `.stdout`, `.stderr`, and `.exitCode` are available after an
exec without re-running it: they are files in the meta mount, not live
process state.

## Where the work runs, and what it costs

**Exec work runs inside the engine container.** The Dagger CLI is a thin
client; the engine is a long-lived container (here, the OCI image
`registry.dagger.io/engine:v0.20.8`, run by `podman` on this host). The
`Worker` holds a `runc` handle and a cgroup parent —
[`engine/buildkit/worker.go` `Worker` struct L38-L70](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/buildkit/worker.go#L38-L70).
Each exec is run by the executor as a `runc` container *nested inside
the engine container* —
[`engine/buildkit/executor.go` `Worker.Run` L157-L192](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/buildkit/executor.go#L157-L192)
runs a setup pipeline (`setupNetwork`, `injectInit`,
`generateBaseSpec`, `setupRootfs`, …, `runContainer`) culminating in a
runc process.

The practical consequences:

- **CPU / memory.** An exec's CPU and memory come out of the engine
  container's own budget. There is no separate VM or host process per
  exec — `N` concurrent execs are `N` runc children of one engine
  process tree, all sharing the engine container's CPU shares and
  memory limit. If the engine container is memory-capped, a heavy exec
  (or many of them) is OOM-killed within that cap, not by the host at
  large.
- **Disk.** Every exec's rootfs, mounts, and committed outputs are
  snapshots in the engine's local cache (see below). Disk is consumed in
  the engine's storage, and it is *retained* after the exec finishes —
  that is the cache. `Worker.Run` returns a
  `bkresourcestypes.Recorder`, i.e. the executor records per-exec
  resource usage.
- **The engine container itself** grows over a session: started
  services stay resident until detached, and cache snapshots accumulate
  until GC runs.

## The caching model

### Operation cache

The unit of caching is the LLB operation. Before executing a vertex the
solver computes a **cache map** — a content-addressed key over the op
and its inputs —
[`engine/buildkit/op.go` `CacheMap` L103-L120](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/buildkit/op.go#L103-L120).
If a result already exists for that key, the op is skipped entirely.

**Experiment — an exec is cached by its inputs, not re-run.** Running
the *same* exec twice: `date` would print a different time if it
actually ran, but the second run returns the first run's output:

```
$ dagger --progress=plain -c 'container | from alpine:3 | with-exec date | stdout'
24 : withExec date
24 : [0.1s] | Thu May 21 22:30:45 UTC 2026
24 : Container.withExec DONE [0.2s]
Thu May 21 22:30:45 UTC 2026

$ # identical command again:
24 : withExec date
24 : Container.withExec CACHED [0.0s]
Thu May 21 22:30:45 UTC 2026
```

`CACHED` and the *identical* timestamp prove the second exec did not
run — the engine returned the cached snapshot. Pulled base-image layers
(`from`) are cached the same way.

### Cache mounts

A cache mount (`withMountedCache`) is a *mutable* directory that is
**deliberately excluded** from the content-addressed result — it
persists across execs and across `dagger` invocations, scoped by a
cache-volume key. It is the escape hatch for things you want to reuse
but not bake into the immutable result (package caches, build caches).

**Experiment — a cache mount survives across separate invocations.**

```
$ dagger -c 'container | from alpine:3 |
             with-mounted-cache /data $(cache-volume devex-exec-demo) |
             with-exec touch /data/written-by-run-1 |
             with-exec ls /data | stdout'
written-by-run-1

$ # a separate dagger invocation, same cache-volume key, no touch:
$ dagger -c 'container | from alpine:3 |
             with-mounted-cache /data $(cache-volume devex-exec-demo) |
             with-exec ls /data | stdout'
written-by-run-1
```

The file written by the first invocation is still present in the
second — the cache volume is shared engine state, not part of any
container's snapshot.

### Prune and GC — reclaiming disk

Cached snapshots accumulate. The engine exposes its on-disk cache and a
garbage-collection policy through `dagger core engine local-cache`.

**Experiment — the GC policy.** Each policy field is a queryable value
(bytes; the CLI prints them in scientific notation):

```
$ dagger core engine local-cache max-used-space    # max bytes kept before pruning
6.98e+11                                           # ~698 GB

$ dagger core engine local-cache reserved-space    # min bytes always retained
1e+10                                              # ~10 GB

$ dagger core engine local-cache min-free-space    # free-space target for GC
1.86e+11                                           # ~186 GB

$ dagger core engine local-cache target-space
0
```

**Experiment — the live cache.** `entry-set` reports what is currently
cached on disk:

```
$ dagger core engine local-cache entry-set entry-count
180379
$ dagger core engine local-cache entry-set disk-space-bytes
3.3531539275e+10                                    # ~33.5 GB
```

**Experiment — the prune command.** `prune` runs the GC pass on demand
and accepts overrides so a reader can prune harder or softer than the
standing policy:

```
$ dagger core engine local-cache prune --help
Prune the cache of releaseable entries

USAGE
  dagger core engine local-cache prune [arguments]

ARGUMENTS
      --max-used-space string   Override the maximum disk space to keep
                                before pruning (e.g. "200GB" or "80%").
      --min-free-space string   Override the minimum free disk space
                                target during pruning (e.g. "20GB" or "20%").
      --reserved-space string   Override the minimum disk space to retain
                                during pruning (e.g. "500GB" or "10%").
```

`prune` only releases entries that are not pinned by something still in
use — cache mounts and in-use snapshots survive. Run with no arguments
it applies the standing policy (the `*-space` values above); the
override flags let CI reclaim disk more aggressively on demand.

## Concurrency and this repo's CI

Concurrency is bounded at two layers:

- **Engine-wide.** The `Worker` carries a `parallelismSem`, a weighted
  semaphore passed into every `ExecOp` —
  [`engine/buildkit/worker.go` L62](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/buildkit/worker.go#L62)
  and
  [`ResolveOp` L160-L169](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/buildkit/worker.go#L160-L169).
  It caps how many execs run at once across the *whole* engine,
  regardless of how many `dagger call`s are in flight. Beyond that cap,
  execs queue.
- **Application-level.** A module can additionally limit its own fan-out
  in code.

Because every concurrent exec shares the single engine container's
CPU/memory/disk, "more parallelism" is not free: past the
`parallelismSem` cap it does not even increase true concurrency, and
before that cap it raises peak memory and disk pressure inside the
engine.

This repo's CI is a single entrypoint — `.github/workflows/ci.yml` runs
one `dagger call test` against the root `ci` module (engine pinned to
`0.20.8`). The heavy load is the kafka suite, which boots real Kafka
clusters in containers. `daggerverse/kafka/tests/main.go` is explicit
about managing this:

- It fans tests out with a `par` pool under an explicit **`parallel`
  cap (default 4)**, applied at *two* levels (distro groups × tests
  within a group), so peak concurrency is bounded rather than
  unbounded.
- Same-shape clusters **share one `*dagger.KafkaCluster` pointer** so
  "the engine collapses their boots to a single service" — this is the
  service deduplication described in
  [networking.md](./networking.md#service--port-scoping): one
  `ServiceKey` per content digest per session.

Practical guidance that follows from the resource model:

- The `parallel` knob is a memory/disk dial, not just a speed dial.
  Raising it multiplies the number of simultaneously-resident Kafka
  containers inside the one engine container; size it to the engine's
  memory, not the host's core count.
- Reusing cluster pointers (so identical services dedup) is strictly
  better than booting fresh clusters — it cuts both wall-clock and peak
  resident containers.
- For long or repeated CI, budget disk for the cache and prune on a
  schedule (`dagger core engine local-cache prune`) rather than letting
  it ride the 698 GB default ceiling.

## Open questions / unverified

- **Per-exec resource limits.** `Worker.Run` returns a resource
  `Recorder` and the worker has a `cgroupParent`, but whether an
  individual `WithExec` can be given an explicit CPU/memory *limit*
  (vs. only being recorded and bounded by the engine's cgroup) was not
  confirmed.
- **`parallelismSem` default.** The semaphore exists and is engine-wide;
  its default weight / how it is configured was not traced to a config
  value.
- **GC policy origin.** The `max-used-space` / `reserved-space` /
  `min-free-space` numbers above are this engine's live values; whether
  they are hard defaults or derived from host disk size at engine
  startup was not verified.
- **`target-space` meaning.** `target-space` reported `0`; its exact
  role relative to `max-used-space` was not determined from source.
- **Prune reclaim figure.** A full `dagger core engine local-cache
  prune` over this engine's live cache (~180k entries / ~33.5 GB) did
  not complete within a bounded time on the test machine, so a concrete
  before/after reclaimed-bytes number is not captured here — only the
  pre-prune `entry-set` and the `prune` command surface are shown.
- **Cache-mount concurrency.** Whether two concurrent execs sharing one
  cache volume serialize or race on it was not tested.
