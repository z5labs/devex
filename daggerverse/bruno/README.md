# bruno

A Dagger module wrapping [`bru`](https://docs.usebruno.com/bru-cli/overview),
the [Bruno](https://www.usebruno.com/) CLI, so API tests written in Bruno stop
reaching CI as a hand-rolled step — a Node install or a `docker run` with the
volume mount spelled correctly, the environment name passed through, and a
wrapper script to turn the exit code into something a pipeline understands.

You bind a collection directory, point it at a service, and get either a
pass/fail gate or a JUnit file.

```sh
dagger -m github.com/z5labs/devex/daggerverse/bruno call \
  collection --source=./api-tests \
  with-environment --name=ci \
  run
```

## The vendor image

Upstream publishes `usebruno/cli` as a Docker Verified Publisher image on both
Docker Hub and ghcr.io, so — unlike `tesseract` or `opentofu` — nothing is
assembled here. The image's entrypoint is `bru`, its working directory is
`/bruno`, and it runs as the non-root `node` user (UID 1000), which is why
reports are staged in a module-owned writable directory rather than written
into the mounted collection.

```go
dag.Bruno()                                                  // docker.io/usebruno/cli:3.4.2
dag.Bruno(dagger.BrunoOpts{Version: "3.5.3"})                // pin a release
dag.Bruno(dagger.BrunoOpts{Registry: "ghcr.io"})             // same image, other registry
dag.Bruno(dagger.BrunoOpts{Debian: true})                    // 3.4.2-debian
dag.Bruno(dagger.BrunoOpts{Version: "3.4.2-debian"})         // the same thing, spelled out
```

The `-debian` suffix is appended when absent, so both spellings agree. The
Debian variant (`node:22-slim`) is not cosmetic: the Alpine default's
musl/OpenSSL build fails against some TLS endpoints, and that variant exists
for exactly those.

`Container()` is the escape hatch for every flag this module does not wrap —
`--proxy`, `--disable-cookies`, `--verbose`, the reporter-redaction switches,
`bru import wsdl` — all reachable via `dagger call container with-exec`.

## Collection, not file

`Collection` takes a `*Directory`, not a lone `*File`: `bru` exits 4 when
invoked outside a collection root, and resolves `environments/` and `.env`
relative to it.

Options that apply across subcommands are hoisted onto `Collection` as chained
modifiers rather than repeated across signatures. It is immutable — every
`With*` returns a copy — so one bound collection can fan out into several
runs.

```go
collection := dag.Bruno().
    Collection(source).
    WithEnvironment("ci").
    WithService("api", api).
    WithVar("baseUrl", "http://api:8080")

smoke := collection.WithTags([]string{"smoke"})
full  := collection.WithTestsOnly()
```

`WithService(alias, service)` exists because a collection is inert without a
target: it puts a Dagger service on the run's network under `alias`, which is
the hostname the collection's environment then points at. A KiCad project needs
no analogue — this is the one input a collection cannot carry itself.

`WithVar` takes a name and a value rather than a map, because Dagger functions
cannot accept map parameters.

`WithEnvFile` stages its file outside the collection, keeping the extension it
arrived with: `bru` picks its environment parser from that extension and not
from the contents, so a `.json` environment staged as `.bru` would die inside
the Bruno grammar. Anything that is neither `.bru` nor `.json` is rejected by
name.

## Run gates, Report reports

```go
// The gate: fails the pipeline when a request, test or assertion failed.
out, err := collection.Run(ctx)

// The artifact: returned whether the run passed or failed.
junit := collection.Report("junit")
```

`Run` returns bru's output and fails on exit 1 — a failing request, test or
assertion. Every other non-zero exit is a *usage* error, reported as itself, so
"the environment name is wrong" (exit 6) never reads as "your API is broken".
Errors carry combined stdout and stderr, because `bru` splits its diagnostics
across both.

`Report` runs the collection and returns the reporter artifact for `"json"`,
`"junit"` or `"html"`, and deliberately does **not** fail on exit 1. A Dagger
function that returns an error forfeits its value, so a failing `Run` could
never also hand back the report — which is exactly when the report matters. CI
pairs the two: `Report` for the artifact, `Run` for the gate. A usage error is
still an error there, because then there is no report to hand back.

```sh
dagger -m github.com/z5labs/devex/daggerverse/bruno call \
  collection --source=./api-tests with-environment --name=ci \
  report --format=junit export --path=./junit.xml
```

`Run`'s `recursive` parameter defaults to true. Note that a `+default=true`
bool cannot be set back to `false` from the Go SDK — the zero value is dropped
before it reaches the engine — so a root-only run is `run --recursive=false`
from the CLI.

### Exit codes

| Exit | Meaning | Module behaviour |
|---|---|---|
| 0 | Everything passed | `Run` returns bru's output |
| 1 | A request, test or assertion failed | `Run` errors; `Report` still returns the artifact |
| 2–13, 255 | Usage error (see below) | Both error, naming what the code means |

`bru` enumerates 2 (output directory missing), 3 (endless request chain), 4
(not a collection root), 5 (file not found), 6 (environment not found), 7 and 8
(malformed environment override), 9 (bad reporter format), 10 and 11 (data file
or workspace not found, or an unparseable collection file), 12 and 13 (global
environment problems), and 255 for everything else.

## Linting

Bruno ships no linter, and the failure modes that leaves open are the expensive
kind: a `{{baseUrl}}` that resolves nowhere fails at request time in CI rather
than at review time, and an API key committed as a plaintext value under
`environments/` is a leak nobody notices.

`Lint` runs without issuing a single request — and without starting a
container, since every rule is evaluated in pure Go over the source tree.

```sh
dagger -m github.com/z5labs/devex/daggerverse/bruno call \
  collection --source=./api-tests with-environment --name=ci \
  lint --fail-on-warnings
```

The rules:

| Rule | Level |
|---|---|
| `bruno.json` exists at the collection root and declares `version`, `name` and `type` | error |
| The environment `WithEnvironment` selected exists under `environments/` | error |
| Every `{{var}}` resolves to a collection, folder or request variable, the selected environment, a `WithVar` override, a `WithEnvFile` file, a `bru.setVar` call, or `process.env` | error |
| No credential-shaped name (`token`, `key`, `password`, `secret`) carries a literal value in a committed `environments/*.bru` `vars` block | error |
| Every request declares `meta { name, seq }`, with `seq` unique within its folder | error |
| A request declares neither `tests` nor `asserts` | warning |

The first two are bru's exit 4 and exit 6, caught before a container starts and
named against the file that is actually missing.

Findings are folded into the returned error, following `kicad`'s `Drc` and
`Erc`: Dagger drops a function's value when it also returns a non-nil error, so
a `(findings, error)` signature would hide the findings on exactly the path
that needs them. Warnings that fail nothing are written to stderr instead, so
they still land in the run's logs.

```
bru lint found 2 errors and 1 warning:
  error   environments/local.bru:3: "apiKey" is credential-shaped and carries a literal value in a committed file: move the name to a vars:secret block, or set the value to {{process.env.<NAME>}}
  error   tenant.bru:8: {{tenantId}} resolves to nothing: declare it in the selected environment "local" or in a vars block, pass it with with-var, or read it from process.env
  warning ping.bru: declares neither tests nor asserts: it passes whatever the API returns
```

Three things about the variable rule worth knowing. With **no** environment
selected, every file under `environments/` contributes — otherwise linting a
collection would force a choice the caller has not made, and report every
`{{baseUrl}}` as broken. A `WithSecretVar` secret is deliberately *not* a bru
variable, so `{{API_TOKEN}}` is a finding where `{{process.env.API_TOKEN}}` is
not. And references inside `docs`, `tests` and `script:*` blocks are skipped:
bru does not interpolate them, so linting them would invent findings out of
prose.

Scope limit: this reads the collection's block and reference structure, not a
full `.bru` parse. Blocks are recognised by their opener at column 0, which is
where Bruno's own writer puts them; everything indented under one — a JS
script, a JSON body, an `example`'s nested request and response — is stepped
over. A complete grammar is tracked separately.

## Generating a collection from OpenAPI

A collection hand-maintained beside an OpenAPI document drifts. `Generate`
wraps `bru import openapi`, so the collection can be produced in CI instead of
committed and forgotten.

```go
collection := dag.Bruno().Generate(spec, dagger.BrunoGenerateOpts{Name: "petstore"})
out, err := dag.Bruno().Collection(collection).WithService("api", api).Run(ctx)
```

```sh
dagger -m github.com/z5labs/devex/daggerverse/bruno call \
  generate --spec=./openapi.yaml --name=petstore \
  export --path=./api-tests
```

The spec is a `*File`, not a URL string, so a local document and a remote one
are the same call — `dag.HTTP(url)` covers the URL case without a second
parameter, and without needing `bru`'s `--insecure`, because the fetch never
happens inside the container. YAML or JSON either way: `bru import` reads the
contents, not the file name.

`format` defaults to `"bru"` where **upstream defaults to
`"opencollection"`**. Only the `bru` shape carries the `bruno.json` and `.bru`
requests that `Collection`, `Run` and `Report` read; `"opencollection"` writes
an `opencollection.yml` and no `bruno.json`, so it is not runnable here. The
default is the one whose output feeds straight back in.

What comes out, for a spec with a `servers:` entry and tagged operations:

```
bruno.json                     # the collection manifest, named by --name
collection.bru                 # collection-level settings
environments/<server>.bru      # baseUrl, from servers[].url
<tag>/folder.bru               # one folder per OpenAPI tag
<tag>/<summary>.bru            # one request per operation
```

Two things worth knowing about that tree. Requests are grouped by tag, so they
land a folder deep and need `Run`'s recursive default to be reached. And the
environment file is named after the server's *description* (falling back to
`Environment 1`), which is the spec's business rather than this module's — read
it off the generated directory rather than hardcoding it, or override `baseUrl`
with `WithVar` and skip `WithEnvironment` entirely.

`Generate` is `+cache="session"`, not `"never"`: conversion is a pure function
of the document and touches no live service.

`--group-by path` (the alternative to grouping by tag) and `bru import wsdl`
are not wrapped; both are reachable through `Container()`.

## Secrets

`WithSecretVar(name, secret)` is a separate function from `WithVar` rather than
an overload because `--env-var name=value` places its value on the process
command line. A `*Secret` is bound with `WithSecretVariable` instead and read
from the collection as `{{process.env.NAME}}`, so it appears in neither argv
nor any diagnostic this module echoes back.

```
# request.bru
headers {
  Authorization: Bearer {{process.env.API_TOKEN}}
}
```

```go
collection.WithSecretVar("API_TOKEN", token).Run(ctx)
```

Note what this does *not* cover: `bru`'s reporters record the resolved request,
so a secret that ends up in a URL or an echoed header lands in the JSON, JUnit
or HTML artifact. Keep secrets in headers you do not report on, or treat the
artifact as sensitive. Reporter redaction (`--reporter-skip-headers` and
friends) is reachable through `Container()`.

## Sandbox

Bruno CLI 3.0 flipped `--sandbox` from Developer Mode to Safe Mode (QuickJS),
and `Collection` matches that default. A collection whose scripts `require()` a
module or touch the filesystem needs `WithSandbox("developer")` — and fails at
*runtime*, not at parse time, without it.

## Caching

`Run` and `Report` carry `+cache="never"`: a collection run hits a live
service, so a cached pass would report a now-broken API as green. `Container`,
`Version`, `Generate` and `Lint` stay `+cache="session"` — those are pure.

That directive governs the *function* result; the `WithExec` layer underneath
is still content-addressed, so both terminals stamp a per-call nonce onto the
run. Without it, a second `Run` of the same collection against the same service
would be a build-cache hit and never leave the engine.

## Security posture

`WithInsecure()` accepts a certificate the run cannot verify — the usual shape
for a service that only exists for the length of the pipeline. Custom CA
certificates (`--cacert`, `--ignore-truststore`) and client certificates
(`--client-cert-config`) are not wrapped yet; until they are, they are
reachable through `Container()`.

## Function surface

| Name | Purpose |
|---|---|
| `Container()` | The pinned `usebruno/cli` image — the escape hatch for unwrapped flags. |
| `Version()` | The Bruno CLI release the image ships. |
| `Generate(spec, name, format)` | Convert an OpenAPI document into a collection directory. |
| `Collection(source)` | Bind a collection tree to the toolchain. |
| `Collection.WithEnvironment(name)` | `--env`; the file under `environments/` without its extension. |
| `Collection.WithVar(name, value)` | `--env-var name=value`. A pair, not a map. |
| `Collection.WithSecretVar(name, secret)` | The same override as a secret environment variable — never argv. |
| `Collection.WithEnvFile(file)` | `--env-file`, staged outside the collection under its own `.bru`/`.json` extension. |
| `Collection.WithTags(tags)` | `--tags`; run only requests carrying one of them. |
| `Collection.WithoutTags(tags)` | `--exclude-tags`; skip requests carrying one of them. |
| `Collection.WithService(alias, service)` | Put a Dagger service on the run's network under `alias`. |
| `Collection.WithSandbox(mode)` | `--sandbox`; `safe` (default) or `developer`. |
| `Collection.WithInsecure()` | `--insecure`; accept unverifiable TLS certificates. |
| `Collection.WithTestsOnly()` | `--tests-only`; skip requests with no test or assertion. |
| `Collection.WithBail()` | `--bail`; stop at the first failure. |
| `Collection.WithDelay(milliseconds)` | `--delay`; wait between requests. |
| `Collection.Lint(failOnWarnings)` | Check the collection's structure in pure Go, issuing no requests. |
| `Collection.Run(recursive)` | Run the collection; bru's output, failing on exit 1. |
| `Collection.Report(format)` | Run it and return the `json`, `junit` or `html` artifact, failing only on usage errors. |

## Tests

```sh
dagger -m daggerverse/bruno/tests call all
```

The suite is hermetic: `tests/responder.go` stands up a request-recording HTTP
service per test and binds it as `api`, which is the host every fixture's
environment points at. That responder runs on the Bruno CLI image itself —
`node:22-alpine`, so `node -e` needs nothing the module under test has not
already pulled — and each instance carries an id minted with
`dag.Random().Sha256()`, because Dagger content-addresses services and a suite
that counts requests would otherwise be counting a previous session's.

Counting requests at the service, rather than reading bru's summary, is what
makes the caching assertions mean anything: a cached `Run` prints a perfectly
convincing "1 request, 1 passed".

`fixtures/petstore.yaml` is the one fixture that is not a Bruno collection: it
is the OpenAPI document `Generate` converts. Its single `servers:` entry names
that same responder, which is what makes
`GenerateProducesRunnableCollection` a real test — the generated collection is
handed straight to `Collection` and run, rather than merely inspected for a
`bruno.json`.

The `fixtures/lint/` tree is the exception to all of that: one deliberately
broken collection per rule, linted rather than run, so nothing there needs the
responder at all. Each carries controls beside its violation — a
`{{process.env.*}}` value beside the plaintext credential, a second folder
reusing a `seq` beside the folder that duplicates one — because a rule that
fires is only half of what needs pinning.

`LintAcceptsValidCollection` deliberately reuses `fixtures/api`, the same
collection the run tests drive against a live service: a linter that only
accepts collections written for it is a linter nobody can adopt.

`SecretVarIsNotOnArgv` is the one test that needs the collection's own
scripting. Its fixture reports `process.argv` — bru's own command line — back
to the responder in a header, which is the only vantage point from which "the
secret never reached argv" can be checked at all. That script needs the
developer sandbox, since the safe one has no `process`.
