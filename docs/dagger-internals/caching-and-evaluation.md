# Caching and lazy evaluation

When a Dagger pipeline actually *runs* — and whether the engine runs it
at all, or returns a cached result — is two questions answered by the
same machinery. This page documents that machinery: the build graph,
when terminal selections force resolution, and the two caches (dagql
per-session and BuildKit content-addressed) that decide whether work
repeats.

> Engine version: `v0.20.8`. Source permalinks are pinned to
> `dagger/dagger` commit
> [`74bff7d`](https://github.com/dagger/dagger/tree/74bff7d10fd78dd6935c60c4514558598f216451).
> See [README.md](./README.md) for the sourcing rule.

## The build graph (dagql)

Every call on a `Container`, `Directory`, `File`, `Service` — core *or*
module-defined — is a node in the dagql call graph. The engine identifies
each node by a `call.ID`: the receiver chain plus the field name and
arguments. Two nodes with the same ID are, by construction, the same
node. dagql digests the ID into a content-addressed key —
[`dagql/session_cache.go` L120](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/dagql/session_cache.go#L120):

```go
callKey := key.ID.Digest().String()
```

The graph is **constructed lazily**. Calling `WithExec`, `From`,
`WithFile`, … on the SDK client appends to the receiver's ID without
contacting the engine; only when the consumer asks for a value the SDK
cannot synthesize locally does a GraphQL query go to the engine. That is
the distinction between non-terminal and terminal selections, below.

### What forces evaluation: terminal selections

A selection that returns a graph-node type (`Container!`, `Directory!`,
`Service!`, …) is non-terminal — the engine only needs to confirm the
node is well-typed and hand the consumer the chained ID. A selection
that returns a scalar (`String`, `Int`, an ID), a host-side effect
(`export`), or that asks explicitly for resolution (`sync`) is terminal:
the engine has to actually run the chain to produce a value.

The clearest example is `sync` itself. Its resolver is registered with
the `Syncer` helper —
[`core/schema/util.go` L9-L17](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/schema/util.go#L9-L17):

```go
func Syncer[T core.Evaluatable]() dagql.Field[T] {
    return dagql.NodeFunc("sync", func(ctx context.Context, self dagql.ObjectResult[T], _ struct{}) (res dagql.Result[dagql.ID[T]], _ error) {
        _, err := self.Self().Evaluate(ctx)
        if err != nil {
            return res, err
        }
        id := dagql.NewID[T](self.ID())
        return dagql.NewResultForCurrentID(ctx, id)
    })
}
```

It calls `Evaluate(ctx)` on anything implementing `core.Evaluatable`,
then returns the resolved ID. For `Container`, `Evaluate` marshals the
accumulated LLB into a definition and asks BuildKit to solve it with
`Evaluate: true` —
[`core/container.go` L2359-L2391](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/container.go#L2359-L2391):

```go
return bk.Solve(ctx, bkgw.SolveRequest{
    Evaluate:   true,
    Definition: def.ToPB(),
})
```

`stdout`, `stderr`, `exitCode`, and `export` are similarly terminal:
each requires the exec to have run (they read from the meta mount or
write to a host path).

**Experiment — `id` does not run the exec, `stdout` does.** The same
chain is queried two ways. With `id`, the request just returns the
serialized call ID; with `stdout`, the engine has to evaluate the chain.
A side-effecting exec makes the difference visible: it writes
`SIDE-EFFECT` to stderr if and only if it actually runs.

```
$ echo '{ container { from(address:"alpine:3") {
            withExec(args:["sh","-c","echo SIDE-EFFECT >&2; date"]) { id }
          } } }' | dagger --progress=plain query
12 : withExec sh -c 'echo SIDE-EFFECT >&2; date'
12 : Container.withExec DONE [0.0s]
{ "container": { "from": { "withExec": { "id": "ChV4eGgz…" } } } }

$ echo '{ container { from(address:"alpine:3") {
            withExec(args:["sh","-c","echo SIDE-EFFECT >&2; date"]) { stdout }
          } } }' | dagger --progress=plain query
12 : withExec sh -c 'echo SIDE-EFFECT >&2; date'
12 : [0.1s] | SIDE-EFFECT
12 : [0.1s] | Sat May 23 00:58:06 UTC 2026
12 : Container.withExec DONE [0.3s]
{ "container": { "from": { "withExec": { "stdout": "Sat May 23 00:58:06 UTC 2026\n" } } } }
```

With `id`: no `SIDE-EFFECT`, no date, `DONE [0.0s]` — the dagql node
resolved (the chain is well-typed) but the exec did not run. With
`stdout`: the exec ran, the side-effect printed, and the chain took
0.3s. "`Container.withExec DONE`" in the progress stream is dagql
saying *the field resolved*, not *the work happened*.

### Parallel branches resolve concurrently

Within a single query, independent selections at the same level resolve
in parallel —
[`dagql/server.go` `Resolve` L563-L596](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/dagql/server.go#L563-L596):

```go
// Resolve resolves the given selections on the given object.
// Each selection is resolved in parallel ...
...
pool := pool.New().WithErrors()
for _, sel := range sels {
    pool.Go(func() error {
        res, err := s.resolvePath(ctx, self, sel)
        ...
    })
}
```

A single-selection fast path skips the goroutine setup; multi-selection
queries fan out over a `conc/pool` worker pool. Concurrency below that
is bounded by the BuildKit `parallelismSem`
([container-execution.md → Concurrency](./container-execution.md#concurrency-and-this-repos-ci)).

**Experiment — two independent branches run in parallel.** Two
top-level aliases each sleep for 3s and exit. If they serialized, the
query would take ~6s; if they run concurrently, ~3s.

```
$ time dagger --progress=plain query <<'EOF'
{
  slow1: container { from(address:"alpine:3") {
           withExec(args:["sh","-c","sleep 3; echo branch-1-done"]) { stdout } } }
  slow2: container { from(address:"alpine:3") {
           withExec(args:["sh","-c","sleep 3; echo branch-2-done"]) { stdout } } }
}
EOF
14 : Container.stdout DONE [3.4s]
15 : Container.stdout DONE [3.4s]
…
real    0m4.242s
```

Wall time 4.2s with two 3.4s branches — they overlapped almost
completely. Both branches' `stdout` selections triggered Solve at the
same time; the BuildKit worker ran them as two `runc` children inside
the engine container.

## Cache 1 — the dagql session cache

The first cache layer is in-memory and per-session. The `SessionCache`
wraps the inner dagql `Cache` and tracks every result the session has
acquired so they can be released at session end —
[`dagql/session_cache.go` L9-L24](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/dagql/session_cache.go#L9-L24):

```go
type SessionCache struct {
    cache Cache

    results          []AnyResult
    arbitraryResults []ArbitraryCachedResult
    mu               sync.Mutex

    // isClosed is set to true when ReleaseAndClose is called.
    // Any in-progress results will be released and errors returned.
    isClosed bool

    seenKeys sync.Map
    ...
}
```

Lookup is keyed by the call ID's digest —
[L93-L148](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/dagql/session_cache.go#L93-L148):

```go
callKey := key.ID.Digest().String()
...
res, err = c.cache.GetOrInitCall(ctx, key, fn)
```

The underlying `Cache` keeps two maps —
[`dagql/cache.go` L247-L255](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/dagql/cache.go#L247-L255):

```go
// calls that are in progress, keyed by a combination of the call key and the concurrency key
ongoingCalls map[callConcurrencyKeys]*sharedResult
// calls that have completed successfully and are cached, keyed by the storage key
completedCalls map[string]*resultList
```

`GetOrInitCall` consults both: a completed call returns its cached
result, an in-progress call has the second caller wait on the first
(`sharedResult`), and a miss runs `fn` and stores the result. The
practical effect: **two requests with the same ID digest, within the
same session, evaluate at most once.** When the session ends,
`ReleaseAndClose`
([L235-L257](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/dagql/session_cache.go#L235-L257))
walks `results` and calls `Release` on each — nothing in this cache
outlives the `dagger` invocation.

**Experiment — same call selected twice in one query runs once.** Two
top-level aliases name the same content-addressed exec (a `cat` of
`/proc/sys/kernel/random/uuid`, which prints a fresh UUID per run):

```
$ dagger --progress=plain query <<'EOF'
{
  a: container { from(address:"alpine:3") {
       withExec(args:["sh","-c","cat /proc/sys/kernel/random/uuid"]) { stdout } } }
  b: container { from(address:"alpine:3") {
       withExec(args:["sh","-c","cat /proc/sys/kernel/random/uuid"]) { stdout } } }
}
EOF
12 : withExec sh -c 'cat /proc/sys/kernel/random/uuid'
12 : [0.1s] | e9b29e9b-c989-4bd4-838f-7c1bdb1ece35
12 : Container.withExec DONE [0.3s]
{
  "a": { "from": { "withExec": { "stdout": "e9b29e9b-c989-4bd4-838f-7c1bdb1ece35\n" } } },
  "b": { "from": { "withExec": { "stdout": "e9b29e9b-c989-4bd4-838f-7c1bdb1ece35\n" } } }
}
```

The exec line appears once in the progress stream; both `a` and `b`
return the *same* UUID. Two top-level selections collapsed to one
resolution — the second `GetOrInitCall` found the first call already
in `completedCalls` and returned its result.

This is the same mechanism behind service deduplication described in
[networking.md → Service & port scoping](./networking.md#service--port-scoping)
(`ServiceKey` carries the session ID; same digest in one session →
same `RunningService`) and behind the kafka test suite's shared cluster
pointer pattern ([container-execution.md → Concurrency](./container-execution.md#concurrency-and-this-repos-ci)).

## Cache 2 — the BuildKit operation cache

The second cache layer is on-disk and persistent across sessions. It is
BuildKit's, not dagql's. The unit is the LLB op: each `WithExec`,
`WithMountedFile`, image pull, etc. lowers to an LLB vertex, and before
running a vertex the solver asks for its **cache map** — a
content-addressed key over the op and its inputs.

Dagger wraps every op's `CacheMap` to inject client metadata —
[`engine/buildkit/op.go` `CacheMap` L95-L111](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/buildkit/op.go#L95-L111):

```go
func (op *CustomOpWrapper) CacheMap(ctx context.Context, g bksession.Group, index int) (*solver.CacheMap, bool, error) {
    cm, ok, err := op.original.CacheMap(ctx, g, index)
    if cm == nil || !ok || err != nil {
        return cm, ok, err
    }
    ...
    cm, err = op.Backend.CacheMap(ctx, cm)
    ...
}
```

If a result already exists for that cache key, the op is skipped
entirely — the solver returns the cached snapshot to the requester.
Unlike the dagql session cache this lives in the engine's local cache
on disk, so a re-run **in a separate `dagger` invocation** still hits
it.

**Experiment — an identical exec is cached across invocations.** Two
separate `dagger` invocations run the same UUID-printing exec:

```
$ dagger --progress=plain -c 'container | from alpine:3 |
        with-exec cat,/proc/sys/kernel/random/uuid | stdout'
14 : [0.2s] | 4d2eb92b-a3e2-4624-825f-c191d13ad2c4
…
4d2eb92b-a3e2-4624-825f-c191d13ad2c4

$ # second, completely separate invocation:
$ dagger --progress=plain -c 'container | from alpine:3 |
        with-exec cat,/proc/sys/kernel/random/uuid | stdout'
14 : Container.withExec CACHED [0.0s]
…
4d2eb92b-a3e2-4624-825f-c191d13ad2c4
```

`CACHED` and the identical UUID prove the second invocation did not
re-run the exec — the engine returned the snapshot from disk. Pulled
base-image layers (`from`) are cached the same way and tend to dominate
the cache when this happens at scale.

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

## How the two caches stack

A terminal selection on a chain hits both caches in order:

1. **dagql session cache** (`SessionCache.GetOrInitCall`,
   [L93](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/dagql/session_cache.go#L93)).
   Keyed by `call.ID.Digest()`. Hit → return the cached `AnyResult`
   without touching BuildKit at all. Miss → run the resolver `fn`.
2. **BuildKit op cache** (`CacheMap` per LLB vertex,
   [`engine/buildkit/op.go` L95](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/buildkit/op.go#L95)).
   Keyed by op + inputs. Hit → return the cached snapshot. Miss → run
   the executor.

The two layers answer two different questions. The dagql layer makes
intra-session reuse free: a service started twice in one session, an
ID materialized twice in one query, a `Sync()` called twice on the same
container — none of these issue duplicate BuildKit solves. The BuildKit
layer makes cross-session reuse possible: rerunning the same pipeline
in a fresh CI job or after a `dagger` restart skips work that the engine
already has snapshots for.

`DoNotCache: true` on a `CacheKey`
([L33-L43](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/dagql/cache.go#L33-L43))
bypasses the dagql layer for a specific call; `dagger up` uses this so
its blocking foreground process is never collapsed into a prior result.
There is no equivalent generic switch for the BuildKit layer — cache
busting is done by changing the content of an op (a `WithEnvVariable`
of a unique value, a non-cacheable upstream input, …), since both
layers are content-addressed.

## Open questions / unverified

- **Concurrency-key semantics.** `CacheKey.ConcurrencyKey`
  ([L33-L43](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/dagql/cache.go#L33-L43))
  controls in-progress dedup specifically; how this differs in practice
  from result-key dedup, and which callers set it to a non-empty value,
  was not traced.
- **TTL semantics.** `CacheKey.TTL` exists on the cache key; whether
  anything in the engine's standing schema actually sets a non-zero TTL
  (or whether it is a hook for SDKs / modules) was not verified.
- **dagql cache hit on a re-issued query in a new session.** The
  session cache is released at session end (`ReleaseAndClose`); whether
  any portion is also persisted to the sqlite DB referenced by
  `NewCache(ctx, dbPath)` ([`dagql/cache.go` L91](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/dagql/cache.go#L91))
  and survives, vs. the DB being used only for in-process bookkeeping,
  was not investigated.
- **`Resolve` parallelism cap.** The selection-resolver `pool.New()`
  ([`dagql/server.go` L585](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/dagql/server.go#L585))
  is unbounded at the dagql layer; BuildKit's `parallelismSem` is the
  real cap. Whether a deeply branching query can starve the engine
  before BuildKit's semaphore engages was not measured.
- **Per-exec cache-mount serialization.** Whether two concurrent execs
  sharing one cache volume serialize on it or race was not tested
  (carried over from
  [container-execution.md → Open questions](./container-execution.md#open-questions--unverified)).
