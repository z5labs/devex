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

## The Ci pipeline

Lint, run and report are three calls plus the glue that decides what fails the
build. `Ci` bundles them, so an API repo's pipeline is one declarative
`dagger call`.

```sh
dagger -m github.com/z5labs/devex/daggerverse/bruno call \
  ci --source=./api-tests \
  with-environment --name=ci \
  with-lint \
  with-report --format=junit \
  check
```

```go
ci := dag.Bruno().Ci(source).
    WithEnvironment("ci").
    WithService("api", api).
    WithSecretVar("API_TOKEN", token).
    WithLint().
    WithReport("junit")

err := ci.Check(ctx)   // the gate
reports := ci.Run()    // the artifacts: reports/report.xml
```

It composes `Collection` without adding capability — every stage is a call the
caller could make by hand. What it adds is the *ordering*: lint runs before the
collection, so a `{{baseUrl}}` that resolves nowhere or a credential committed
in plaintext is reported without spending a request on discovering it.

| Stage | What it does |
|---|---|
| `WithEnvironment(name)` | `--env`, and the environment lint resolves references against. |
| `WithService(alias, service)` | Put a Dagger service on the pipeline's network. |
| `WithSecretVar(name, secret)` | A credential readable as `{{process.env.NAME}}`, never on argv. |
| `WithLint(failOnWarnings)` | Add the lint stage, ahead of the collection. |
| `WithReport(format)` | Add `json`, `junit` or `html` to the set `Run` returns. Call it more than once. |
| `Check()` | The gate: fails on a lint error, on a failing request/test/assertion, and on a usage error. |
| `Run()` | The artifacts: `report.json`, `report.xml` (junit), `report.html`. |

`Check` gates and `Run` does not, which is the same split as `Collection`'s
`Run` and `Report` and for the same reason: a Dagger function that returns an
error forfeits its value, so a gating `Run` would hand back nothing on exactly
the runs whose report a pipeline needs. Pair them — `Run` for the artifacts,
`Check` for the gate. A lint error and a usage error still fail `Run`, because
then the collection never ran and there is no report to return.

Every requested format comes out of **one** collection pass: `bru` accepts all
of its `--reporter-*` flags at once, so asking for both JUnit and HTML costs one
run and the two artifacts describe the same set of responses.

`Run` with no `WithReport` is an error rather than an empty directory: running
the collection to hand back nothing reads as a pass, and a pipeline that only
wants to gate wants `Check`.

Lint is opt-in rather than always-on because it is an opinion about how a
collection is written, and a pipeline should not start failing on one the day it
adopts the builder.

The pipeline deliberately wraps a subset of `Collection`'s surface: secrets but
not plain `WithVar` overrides (a value passed into CI by hand is usually a
credential, and `--env-var` would put it on the command line), and no sandbox,
tag, bail or delay switches. A collection that needs those is assembled through
`Collection` directly.

The TLS controls are the exception, and they are wrapped for the same reason
secrets are: an internal endpoint behind a private CA is exactly the kind of
target a repo hangs a CI check on, and a pipeline that had to drop to
`Collection` to reach one would lose the bundled lint stage and the reports.

Both terminals are `+cache="never"`, and the suite proves the second `Check` in
one session really re-runs by counting requests at the service.

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

`Run`, `Report`, `Ci.Check` and `Ci.Run` carry `+cache="never"`: a collection
run hits a live service, so a cached pass would report a now-broken API as
green. `Container`, `Version`, `Generate` and `Lint` stay `+cache="session"` —
those are pure.

That directive governs the *function* result; the `WithExec` layer underneath
is still content-addressed, so every terminal stamps a per-call nonce onto the
run. Without it, a second `Run` of the same collection against the same service
would be a build-cache hit and never leave the engine.

`Ci.Run` returns a directory, and a never-cached function is re-invoked by each
selection off its result — so reading two reports out of one `Run` would be two
runs of the collection. Resolve the directory's `ID` once and load it back
before fanning out, as `tests/main.go`'s `pin` does.

## TLS

`WithInsecure()` accepts a certificate the run cannot verify — the usual shape
for a service that only exists for the length of the pipeline. It is the wrong
tool for an internal endpoint behind a private CA, because it verifies nothing:

```go
collection.
    WithCaCert(ca).                                  // --cacert
    WithoutTruststore().                             // --ignore-truststore
    WithClientCert("*.internal", cert, key).         // --client-cert-config
    Run(ctx)
```

`WithCaCert(file)` adds a CA to the truststore the image ships rather than
replacing it, so a collection that also reaches a public endpoint keeps working.
`WithoutTruststore()` narrows verification to that CA alone, and means nothing
without it — `bru` evaluates `--ignore-truststore` "in combination with
`--cacert` only" — so on its own it is rejected by the run. So is `WithCaCert`
alongside `WithInsecure`: `bru` drops `--cacert` when `--insecure` is set, with
a message on stderr and a zero exit, leaving a run that verifies nothing when
the caller asked to verify against a named CA.

Neither certificate is a path the caller supplies. A `--cacert` pointing at a
file that is not there is not one of `bru`'s usage errors: it prints "Cacert
File … does not exist" and carries on with the default truststore, so the run
fails verification for a reason that looks nothing like the mistake. The module
mounts the files it names.

`WithClientCert(host, cert, key, passphrase)` takes the key as a `*Secret` and
not a `*File`, matching the `skill-gen` postgres precedent: a file's contents are
content-addressed into the build cache and readable from a trace. `host` is
matched against the request URL, wildcards included, and `bru` presents the first
configured host that matches — so a host configured twice is rejected rather than
silently resolved to the first one.

`bru` takes client certificates as a JSON document that refers to the
certificate and key *by path*. That document is rendered by this module at run
time against the paths it mounts them under, because a caller writing it by hand
would have to name paths inside a container they cannot see. It is rendered into
a `*Secret` rather than a file: the passphrase is a plaintext field of it, which
is the shape `bru` reads, so keeping it out of a cacheable layer is the only
place that can be dealt with. Certificate, key and document are all mounted
outside the collection — the tree a caller lints and commits — and handed to the
image's non-root user, since a secret mount is root-owned `0400` by default and
`certificate-management` writes its PEM files `0600`.

All three are on `Ci` as well, delegating to the same builder — see the pipeline
section for why they are the one part of `Collection`'s surface it wraps beyond
secrets.

The one client-certificate shape not wrapped is `bru`'s `"pfx"` entry: a PKCS#12
archive is a single file, so it would not need the key to travel separately. It
stays reachable through `Container()`, along with the rest of `bru`'s TLS long
tail.

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
| `Collection.WithCaCert(cert)` | `--cacert`; verify peers against a private CA as well as the default truststore. |
| `Collection.WithoutTruststore()` | `--ignore-truststore`; verify against the `WithCaCert` CA alone. |
| `Collection.WithClientCert(host, cert, key, passphrase)` | `--client-cert-config`; present a client certificate to hosts matching `host`. |
| `Collection.WithTestsOnly()` | `--tests-only`; skip requests with no test or assertion. |
| `Collection.WithBail()` | `--bail`; stop at the first failure. |
| `Collection.WithDelay(milliseconds)` | `--delay`; wait between requests. |
| `Collection.Lint(failOnWarnings)` | Check the collection's structure in pure Go, issuing no requests. |
| `Collection.Run(recursive)` | Run the collection; bru's output, failing on exit 1. |
| `Collection.Report(format)` | Run it and return the `json`, `junit` or `html` artifact, failing only on usage errors. |
| `Ci(source)` | Bind a collection to the standardized pipeline builder. |
| `Ci.WithEnvironment(name)` | `--env`, and the environment lint resolves against. |
| `Ci.WithService(alias, service)` | Put a Dagger service on the pipeline's network. |
| `Ci.WithSecretVar(name, secret)` | A credential readable as `{{process.env.NAME}}`. |
| `Ci.WithCaCert(cert)` | `--cacert` for the pipeline; verify peers against a private CA. |
| `Ci.WithoutTruststore()` | `--ignore-truststore` for the pipeline; verify against that CA alone. |
| `Ci.WithClientCert(host, cert, key, passphrase)` | `--client-cert-config` for the pipeline; authenticate to an mTLS endpoint. |
| `Ci.WithLint(failOnWarnings)` | Add the lint stage, ahead of the collection. |
| `Ci.WithReport(format)` | Add a reporter format to the set `Ci.Run` returns. |
| `Ci.Check()` | The pipeline as a gate: lint, then the collection. Produces nothing. |
| `Ci.Run()` | The pipeline's reports as a directory. Does not gate. |

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
developer sandbox, since the safe one has no `process` — which is why
`fixtures/ci-secret/` exists as a script-free copy of the same request: the
pipeline builder wraps no sandbox switch.

`ClientCertMaterialStaysOutOfTheCollection` uses the same trick for the same
reason, and adds a `readdirSync` of the working directory, because the collection
bru sees is the mount and not the caller's directory.

The TLS tests get a second responder. `tests/tlsresponder.go` serves the same
recording handler over HTTPS on 8443 — presenting a leaf signed by a CA minted
for that one test — and keeps the record on a *plaintext* listener on 8080. That
is not a shortcut: under mTLS the recording listener rejects any client that does
not present a certificate, so asking it what happened would mean handing it the
credential the test is trying to prove arrived. Every key, password and serial
comes from the `crypto`, `random` and `certificate-management` modules at test
time; `tests/certs.go` is ported from `skill-gen`, which took it from `postgres`.

Each of those tests runs the same collection twice, because verification against
a private CA is indistinguishable from no verification unless the run *without*
the CA fails. And the mTLS assertions are about the certificate rather than the
handshake: the responder reports the peer certificate's Common Name back, so a
run that connected without presenting anything fails instead of passing on the
strength of having connected. `ClientCertPassphraseUnlocksTheKey` needs a key
that genuinely wants one, which neither `crypto` nor `certificate-management`
produces and the image has no `openssl` to make — so `certs.go` re-exports the
issued key through Node's own `crypto`, encrypted, reading it back as a file
rather than off stdout.

The `Ci` tests lean on the request counter for the two things the builder itself
adds. `CiLintFailsBeforeAnyRequest` runs the `fixtures/lint/unresolved`
collection, which bru is perfectly happy to execute — it interpolates the
literal `{{tenantId}}` and gets a 200 — so a count of zero is the lint stage
short-circuiting and not an unrunnable fixture, and the same pipeline without
`WithLint` is checked afterwards to prove it. And
`CiRunProducesEveryRequestedReport` asks for two formats over the 2-request
`fixtures/api`: a count of two says both reports came out of one pass.
