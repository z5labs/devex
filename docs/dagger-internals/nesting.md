# Nesting (`dagger in dagger`)

How a container started with `experimentalPrivilegedNesting: true` reaches
back into the engine — and why the popular "DinD" mental model (a second
engine running inside the container) is wrong.

> Engine version: `v0.20.8`. Source permalinks are pinned to
> `dagger/dagger` commit
> [`74bff7d`](https://github.com/dagger/dagger/tree/74bff7d10fd78dd6935c60c4514558598f216451).
> See [README.md](./README.md) for the sourcing rule.

## The headline

There is **no nested engine**. When a `withExec` sets
`experimentalPrivilegedNesting: true`, the parent engine binds a
per-exec TCP listener **inside that container's network namespace** and
injects `DAGGER_SESSION_PORT` / `DAGGER_SESSION_TOKEN` into the
container's env. Anything inside the container — the `dagger` CLI, an
SDK, or plain `curl` — speaks GraphQL to `http://127.0.0.1:$DAGGER_SESSION_PORT/query`,
and the **same** engine serves the request as a "nested client" sharing
the outer session.

What this is **not**:

- **Not Docker-in-Docker.** No second daemon, no nested BuildKit, no
  `dagger-engine` process spawned inside the container.
- **Not a socket bind-mount.** Nothing under `/run/dagger/` is mounted
  into the container.
- **Not a new dagql server.** The HTTP/2 listener is a thin proxy onto
  the same dagql server that is already serving the outer session.

## The exec-side wiring

The flag rides from the GraphQL call all the way into the buildkit exec
spec:

- The schema-level option, defined on `ContainerExecOpts`:
  [`core/container_exec.go` L66-L67](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/container_exec.go#L66-L67).
- When set, `execMeta` allocates a fresh nested-client identity on the
  exec's `ExecutionMetadata` —
  [`core/container_exec.go` L139-L145](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/container_exec.go#L139-L145):

  ```go
  // this allows executed containers to communicate back to this API
  if opts.ExperimentalPrivilegedNesting {
      // establish new client ID for the nested client
      if execMD.ClientID == "" {
          execMD.ClientID = identity.NewID()
      }
  }
  ```

- The buildkit worker's exec pipeline includes a `setupNestedClient`
  step, sandwiched between `createCWD` and `installCACerts` —
  [`engine/buildkit/executor.go` L175-L191](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/buildkit/executor.go#L175-L191).
- `setupNestedClient` does the real work —
  [`engine/buildkit/executor_spec.go` L968-L1064](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/buildkit/executor_spec.go#L968-L1064).
  When `execMD.ClientID != ""` it:

  1. Writes the new `ClientID` to a meta-mount file —
     [`engine/buildkit/ref.go` L55](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/buildkit/ref.go#L55)
     (`MetaMountClientIDPath = "clientID"`).
  2. Generates a per-exec `SecretToken` if one is not already set
     (`randid.NewID()`).
  3. Calls `runInNetNS` to **join the container's network namespace**
     and binds a TCP listener inside it —
     [`engine/buildkit/linux_namespace.go` L49-L88](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/buildkit/linux_namespace.go#L49-L88):

     ```go
     httpListener, err := runInNetNS(ctx, state, func() (net.Listener, error) {
         return net.Listen("tcp", "127.0.0.1:0")
     })
     ```

     The listener is loopback-only and lives inside the container's
     netns, so it is reachable from the container as `127.0.0.1:<port>`
     and invisible to the host.
  4. Appends three env vars to the OCI spec —
     [`engine/buildkit/executor_spec.go` L1035-L1036](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/buildkit/executor_spec.go#L1035-L1036)
     (and L991 for the token):

     | Env var                  | Value                                     |
     | ------------------------ | ----------------------------------------- |
     | `DAGGER_SESSION_PORT`    | port chosen by the netns listener         |
     | `DAGGER_SESSION_TOKEN`   | per-exec secret (`randid.NewID()`)        |
     | `DAGGER_ENGINE_NUM_CPU` | `runtime.NumCPU()` of the engine host     |

     The full set of `_DAGGER_*` / `DAGGER_*` env constants is listed
     at
     [`engine/buildkit/executor_spec.go` L58-L72](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/buildkit/executor_spec.go#L58-L72).

  5. Starts an h2c (HTTP/2 cleartext) server on the listener; the
     handler is a closure over `execMD` calling
     `sessionHandler.ServeHTTPToNestedClient(resp, req, w.execMD)` —
     [`engine/buildkit/executor_spec.go` L1038-L1055](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/buildkit/executor_spec.go#L1038-L1055).
     Server, listener, and net-ns context are all registered with the
     exec's cleanup chain
     ([`L1057-L1062`](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/buildkit/executor_spec.go#L1057-L1062))
     so they die with the exec.

## The container-side wiring

Whatever runs inside the container picks up those three env vars and
dials loopback. There are three callers that all do this the same way:

- **Plain HTTP** — `curl -u $DAGGER_SESSION_TOKEN: http://127.0.0.1:$DAGGER_SESSION_PORT/query`.
  Upstream's smoke test is exactly this —
  [`core/integration/dind_test.go` L24-L50](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/integration/dind_test.go#L24-L50).
- **The Go SDK** — `sdk/go/engineconn/env.go` reads `DAGGER_SESSION_PORT`
  and requires `DAGGER_SESSION_TOKEN` —
  [`sdk/go/engineconn/env.go` L11-L22](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/sdk/go/engineconn/env.go#L11-L22).
- **The `dagger` CLI** — same logic in the higher-level client
  bootstrap. When `DAGGER_SESSION_PORT` is set, the client
  **short-circuits the entire "start engine / start session" flow** and
  just dials the loopback port —
  [`engine/client/client.go` L245-L272](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/client/client.go#L245-L272):

  ```go
  nestedSessionPortVal, isNestedSession := os.LookupEnv("DAGGER_SESSION_PORT")
  if isNestedSession {
      // …
      c.nestedSessionPort = nestedSessionPort
      c.SecretToken = os.Getenv("DAGGER_SESSION_TOKEN")
      // …
      c.httpClient = c.newHTTPClient()
      if err := c.init(connectCtx); err != nil {
          return nil, fmt.Errorf("initialize nested client: %w", err)
      }
      // …
      return c, nil
  }
  ```

The `dagger` CLI does **not** need to be in the container. If you only
need to make a couple of GraphQL calls, `apk add curl` is enough.

**Experiment — the env vars are present, and a nested GraphQL call
round-trips.** Save this as `nest.graphql`:

```graphql
{
  container {
    from(address: "alpine:3.20") {
      withExec(args: ["apk", "add", "--no-cache", "curl"]) {
        withExec(
          args: ["sh", "-c", "printf 'PORT=%s TOKEN_LEN=%s CPU=%s\\n' \"$DAGGER_SESSION_PORT\" \"${#DAGGER_SESSION_TOKEN}\" \"$DAGGER_ENGINE_NUM_CPU\"; curl -fsS -u \"$DAGGER_SESSION_TOKEN:\" -H 'content-type:application/json' -d '{\"query\":\"{host{directory(path:\\\"/tmp\\\"){entries}}}\"}' http://127.0.0.1:$DAGGER_SESSION_PORT/query"]
          experimentalPrivilegedNesting: true
        ) { stdout }
      }
    }
  }
}
```

Run it from this repo (any directory with a `dagger.json` works):

```
$ dagger query < nest.graphql
…
"stdout": "PORT=45431 TOKEN_LEN=25 CPU=32\n{\"data\":{\"host\":{\"directory\":{\"entries\":[]}}}}"
```

Both lines are evidence:

- `PORT=45431 TOKEN_LEN=25 CPU=32` — the three env vars from
  `setupNestedClient` are present inside the container (`TOKEN_LEN=25`
  is the length of `randid.NewID()`; `CPU=32` reflects the engine
  host's `runtime.NumCPU()`).
- `{"data":{"host":{…}}}` — the loopback listener accepted a GraphQL
  request bearing the token and returned a real response from the same
  engine.

**Negative control — without the flag, the env vars are absent.** Same
container, no `experimentalPrivilegedNesting`:

```graphql
{
  container {
    from(address: "alpine:3.20") {
      withExec(args: ["sh", "-c", "printf 'PORT=[%s] TOKEN=[%s]\\n' \"$DAGGER_SESSION_PORT\" \"$DAGGER_SESSION_TOKEN\""]) {
        stdout
      }
    }
  }
}
```

```
$ dagger query < no-nest.graphql
…
"stdout": "PORT=[] TOKEN=[]\n"
```

The container can prove its negative: with the flag, the env vars exist
and the loopback listener responds; without it, neither is true. There
is no global / always-on socket the engine quietly mounts in.

## What "nested" means on the engine

When a nested request arrives, the engine does **not** treat it as a
brand-new client. It rebuilds the per-request identity from the
`execMD` captured in the listener's closure rather than from HTTP
headers —
[`engine/server/session.go` L1030-L1078](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/server/session.go#L1030-L1078).
The doc comment explains why: execution metadata can carry arbitrary
user values from the function call that would blow past HTTP header
size limits.

```go
// ServeHTTPToNestedClient serves nested clients, including module
// function calls. The only difference is that additional execution
// metadata is passed alongside the request from the executor.
```

The metadata each nested call inherits, set back in `execMeta`
([`core/container_exec.go` L94-L96`](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/container_exec.go#L94-L96)):

| Field             | Value                                                       |
| ----------------- | ----------------------------------------------------------- |
| `SessionID`       | **same** as the outer call                                  |
| `ClientID`        | **new** ID allocated for the nested client                  |
| `CallerClientID`  | the outer call's `ClientID` (lineage)                       |
| `CallID`          | the outer call's dagql call ID                              |
| `EncodedModuleID` / `EncodedFunctionCall` / `ParentIDs` | propagated         |

So nested calls live in the **same session** as the outer call, with
the outer client recorded as the caller. This is the actual reason the
feature exists at all:

- Services started in the outer call are reachable from the nested
  call (same session ⇒ same `ServiceKey` set; see
  [networking.md](./networking.md#service--port-scoping)).
- Cache lookups in the nested call hit the same dagql per-session call
  cache as the outer call; see
  [caching-and-evaluation.md](./caching-and-evaluation.md). Chains of
  `dagger call`s issued from inside a container "just work" and stay
  cached together.

Nesting is not a containment boundary — it is a way to keep a chain of
calls inside the same session.

## What "privileged" means in this name

`ExperimentalPrivilegedNesting` is not about Linux capabilities. Linux
capabilities are a **separate** flag,
[`InsecureRootCapabilities`](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/container_exec.go#L69-L70):

```go
// Grant the process all root capabilities
InsecureRootCapabilities bool `default:"false"`
```

The trust boundary that "privileged" refers to is the **GraphQL
surface** the nested client can see. Once the container holds a valid
`DAGGER_SESSION_TOKEN`, it has the same GraphQL surface a top-level
client has:

- `host { directory(...) }`, `host { file(...) }`, `host { unixSocket(...) }`
  — the nested client can read files, mount directories, and dial host
  sockets via the engine.
- It can `container { … }` and start more nested sessions in turn.
- It can list and reach other services in the session (subject to the
  session/module DNS domain rules in
  [networking.md](./networking.md#one-network-or-many)).

If the container in question runs untrusted code, this is the boundary
to think about. Linux-level isolation is unchanged — the container is
no more `--privileged` than it would otherwise be — but its blast
radius via the engine is now equivalent to a local Dagger client.

## How this differs from the engine-dev socket pattern

The integration suite has a related-but-different pattern that is easy
to confuse with nesting. The `engine-dev` toolchain bind-mounts a host
Unix socket to talk to an **outer** engine from a child container —
e.g.
[`core/integration/engine_test.go` L149-L150](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/integration/engine_test.go#L149-L150):

```go
WithUnixSocket("/run/dagger-engine.sock", c.Host().UnixSocket("/run/dagger-engine.sock")).
WithEnvVariable("_EXPERIMENTAL_DAGGER_RUNNER_HOST", "unix:///run/dagger-engine.sock")
```

That code path is the *un-nested* one (`engine/client/client.go`
falling through past the `DAGGER_SESSION_PORT` branch and starting a
fresh engine connection via `_EXPERIMENTAL_DAGGER_RUNNER_HOST`). It is
used to drive engine-development workflows where the inner client must
start its own session against an outer engine — explicitly **not** a
nested-client relationship, and the inner call lives in its own
session, with its own session-scoped caches and services.

`ExperimentalPrivilegedNesting` is the in-session path; the engine-dev
socket bind-mount is the cross-session path. They look superficially
similar from outside the container and behave completely differently.

## Open questions / unverified

- **Long-lived children after the exec returns.** `setupNestedClient`
  registers cleanups that fire when the exec's cleanup chain runs
  ([`executor_spec.go` L1057-L1062](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/buildkit/executor_spec.go#L1057-L1062)).
  I would expect a process the exec leaves behind to see
  `http.ErrServerClosed` / `net.ErrClosed` once the exec finishes, but
  this was not experimentally confirmed.
- **Token rotation / reuse.** The token is generated once per exec via
  `randid.NewID()`
  ([`executor_spec.go` L982`](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/buildkit/executor_spec.go#L982)).
  Whether the engine rejects a token replayed against a different
  exec's listener (vs. the listener being closed making it moot) was
  not traced end-to-end.
- **Non-Linux executors.** `runInNetNS` has an `unimplemented_namespace.go`
  build-tag fallback
  ([`engine/buildkit/unimplemented_namespace.go` L55](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/buildkit/unimplemented_namespace.go#L55)),
  which suggests nesting on non-Linux executors falls through to a
  stub. What that stub returns, and whether the engine even runs in
  that configuration, was not investigated.
- **Workspace / extra-modules overlay.** `ServeHTTPToNestedClient`
  *does* still read HTTP headers for `clientMetadata` and overlays
  workspace/extra-modules onto the execMD-supplied identity
  ([`engine/server/session.go` L1045-L1054](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/server/session.go#L1045-L1054)).
  The precedence rules between execMD and header-supplied workspace
  refs were not exercised here.
- **Token visibility in caching.** `DAGGER_SESSION_TOKEN` is appended
  to the OCI process env. It is not in the `removeEnvs` set
  ([`engine/buildkit/executor_spec.go` L84-L93](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/engine/buildkit/executor_spec.go#L84-L93)),
  but those `removeEnvs` are for runtime stripping rather than
  cache-key construction. Whether the per-exec random token leaks into
  any operation cache key for a nested exec was not traced.
