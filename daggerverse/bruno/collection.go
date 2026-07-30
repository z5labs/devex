package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"dagger/bruno/internal/dagger"
)

const (
	// envFilePathPrefix is the stem WithEnvFile's file is mounted under. It is
	// outside the collection so that supplying one never shadows a file the
	// collection already ships. The caller's extension is preserved: bru picks
	// its parser from it, so a JSON environment mounted as .bru is read as
	// Bruno source and dies in the grammar.
	envFilePathPrefix = "/tmp/bruno-env-file"

	// reportPathPrefix is the stem every reporter artifact is written under.
	// It lives in /tmp because the image runs as UID 1000 and the collection
	// is mounted root-owned: bru could not write a report beside it.
	reportPathPrefix = "/tmp/bruno-report"

	// sandboxSafe is the QuickJS sandbox bru 3.0 made the default, and
	// sandboxDeveloper the Node one that predates it. A collection whose
	// scripts require() a module or touch the filesystem needs developer, and
	// fails at runtime rather than at parse time without it.
	sandboxSafe      = "safe"
	sandboxDeveloper = "developer"

	// formatJSON, formatJUnit and formatHTML are the reporter formats bru
	// writes, each with the flag and file extension it wants.
	formatJSON  = "json"
	formatJUnit = "junit"
	formatHTML  = "html"

	// exitCollectionFailed is bru's exit code for "a request, test or
	// assertion failed" — a result, not a fault. Every other non-zero exit is
	// a usage error, which is why this one is singled out.
	exitCollectionFailed = 1

	// runNonceEnv carries a per-invocation value into the run's container.
	//
	// `+cache="never"` re-invokes the function; it does not re-run the exec
	// the function builds. Two Runs of the same collection assemble a
	// byte-identical container, so without this the second one is a build
	// cache hit and the target service never sees a second request — the
	// cached pass this module exists to avoid. A collection run is not a pure
	// build step, so it opts out of being treated as one.
	runNonceEnv = "BRUNO_RUN_NONCE"
)

// exitMeanings names bru's usage-error exit codes, from the CLI's own
// constants.js. They exist so that "the environment name is wrong" (6) never
// reads as "your API is broken" (1).
//
// 10 and 11 are each defined twice upstream — ERROR_CSV_FILE_NOT_FOUND and
// ERROR_INVALID_FILE both take 10, ERROR_JSON_FILE_NOT_FOUND and
// ERROR_WORKSPACE_NOT_FOUND both take 11 — so both readings are reported
// rather than a guess between them.
var exitMeanings = map[int]string{
	2:   "the output directory does not exist",
	3:   "the request chain caused an endless loop",
	4:   "bru was called outside a collection root",
	5:   "a requested file was not found",
	6:   "the requested environment was not found",
	7:   "an environment override was neither a string nor an object",
	8:   "an environment override was malformed",
	9:   "the requested output format is not one of json, junit, html",
	10:  "the CSV data file was not found, or a collection file could not be parsed",
	11:  "the JSON data file was not found, or the workspace was not found",
	12:  "a global environment requires a workspace",
	13:  "the requested global environment was not found",
	255: "an unclassified bru error",
}

// Collection is a Bruno collection tree plus the options that apply across
// bru's subcommands. It is immutable: every With* returns a copy.
//
// The input is a directory, not a lone request file: bru exits 4 when invoked
// outside a collection root, and resolves environments/ and .env relative to
// it.
type Collection struct {
	// +private
	Bruno *Bruno
	// +private
	Source *dagger.Directory
	// +private
	Environment string
	// +private
	VarNames []string
	// +private
	VarValues []string
	// +private
	SecretVarNames []string
	// +private
	SecretVarValues []*dagger.Secret
	// +private
	EnvFile *dagger.File
	// +private
	Tags []string
	// +private
	ExcludedTags []string
	// +private
	ServiceAliases []string
	// +private
	Services []*dagger.Service
	// +private
	Sandbox string
	// +private
	Insecure bool
	// +private
	CaCert *dagger.File
	// +private
	IgnoreTruststore bool
	// +private
	ClientCertHosts []string
	// +private
	ClientCerts []*dagger.File
	// +private
	ClientKeys []*dagger.Secret
	// +private
	ClientPassphrases []*dagger.Secret
	// +private
	TestsOnly bool
	// +private
	Bail bool
	// +private
	Delay int
}

// Collection binds a Bruno collection directory to the toolchain.
func (b *Bruno) Collection(source *dagger.Directory) *Collection {
	return &Collection{Bruno: b, Source: source, Sandbox: sandboxSafe}
}

// WithEnvironment selects the environment bru resolves variables from
// (`--env`), by the name of the file under environments/ without its
// extension. An unknown name is bru's exit 6, reported as a usage error.
func (c *Collection) WithEnvironment(name string) *Collection {
	out := c.clone()
	out.Environment = name
	return out
}

// WithVar overrides a single environment variable (`--env-var name=value`),
// taking precedence over whatever the selected environment declares.
//
// It takes a name and a value rather than a map because Dagger functions
// cannot accept map parameters. The value lands on bru's command line — use
// WithSecretVar for anything that should not.
//
// Validation is deferred to the run: builder methods have no error return, so
// a bad name surfaces on the Run or Report that would have used it.
func (c *Collection) WithVar(name string, value string) *Collection {
	out := c.clone()
	out.VarNames = append(out.VarNames, name)
	out.VarValues = append(out.VarValues, value)
	return out
}

// WithSecretVar makes a secret readable from the collection as
// {{process.env.NAME}}.
//
// It is a separate function from WithVar rather than an overload because
// `--env-var` places its value on the process command line. A secret is bound
// with WithSecretVariable instead, so it appears in neither argv nor any
// diagnostic this module echoes back.
func (c *Collection) WithSecretVar(name string, value *dagger.Secret) *Collection {
	out := c.clone()
	out.SecretVarNames = append(out.SecretVarNames, name)
	out.SecretVarValues = append(out.SecretVarValues, value)
	return out
}

// WithEnvFile supplies an environment file (`--env-file`), a .bru or .json
// file holding the variables for the run. It is mounted outside the
// collection, so it never shadows a file the collection ships.
func (c *Collection) WithEnvFile(file *dagger.File) *Collection {
	out := c.clone()
	out.EnvFile = file
	return out
}

// WithTags restricts the run to requests carrying any of these tags
// (`--tags`).
func (c *Collection) WithTags(tags []string) *Collection {
	out := c.clone()
	out.Tags = append(out.Tags, tags...)
	return out
}

// WithoutTags excludes requests carrying any of these tags
// (`--exclude-tags`). It composes with WithTags: a request matching both is
// excluded.
func (c *Collection) WithoutTags(tags []string) *Collection {
	out := c.clone()
	out.ExcludedTags = append(out.ExcludedTags, tags...)
	return out
}

// WithService binds a service into the run's network under alias, so the
// collection can reach it by that hostname. A collection is inert without a
// target, which is why this exists at all.
func (c *Collection) WithService(alias string, service *dagger.Service) *Collection {
	out := c.clone()
	out.ServiceAliases = append(out.ServiceAliases, alias)
	out.Services = append(out.Services, service)
	return out
}

// WithSandbox selects the JavaScript sandbox scripts and assertions run in
// (`--sandbox`): "safe", the QuickJS sandbox bru 3.0 made the default, or
// "developer", the Node one. A collection whose scripts require() a module or
// touch the filesystem needs "developer" — and fails at runtime, not at parse
// time, without it.
//
// A collection that says nothing runs in "safe", matching both bru's default
// and Collection's. Like WithVar, an unknown mode is reported by the run
// rather than here.
func (c *Collection) WithSandbox(mode string) *Collection {
	out := c.clone()
	out.Sandbox = mode
	return out
}

// WithInsecure accepts TLS certificates the run cannot verify
// (`--insecure`) — a self-signed certificate on a service that only exists
// for the length of the pipeline, typically.
//
// It verifies nothing, which is the wrong tool for a target behind a private
// CA: use WithCaCert for that, and WithClientCert to authenticate with a
// certificate of the run's own. bru drops `--cacert` when `--insecure` is set,
// so combining the two is rejected rather than quietly verifying nothing.
func (c *Collection) WithInsecure() *Collection {
	out := c.clone()
	out.Insecure = true
	return out
}

// WithTestsOnly runs only the requests that carry a test or an active
// assertion (`--tests-only`), skipping the ones that exist to set up state
// for a human.
func (c *Collection) WithTestsOnly() *Collection {
	out := c.clone()
	out.TestsOnly = true
	return out
}

// WithBail stops the run at the first failing request, test or assertion
// (`--bail`) instead of working through the rest of the collection.
func (c *Collection) WithBail() *Collection {
	out := c.clone()
	out.Bail = true
	return out
}

// WithDelay waits the given number of milliseconds between requests
// (`--delay`), for a target that rate-limits.
func (c *Collection) WithDelay(milliseconds int) *Collection {
	out := c.clone()
	out.Delay = milliseconds
	return out
}

// Run executes the collection and returns bru's output.
//
// It fails on exit 1 — a failing request, test or assertion — so a collection
// is a gate a pipeline can hang a check on. Every other non-zero exit is a
// usage error reported as itself, so "the environment name is wrong" never
// reads as "your API is broken". Errors carry combined stdout and stderr,
// because bru splits its diagnostics across both.
//
// A failing Run returns no output alongside its error: a Dagger function that
// returns an error forfeits its value. Pair it with Report when the artifact
// matters.
//
// +cache="never"
func (c *Collection) Run(
	ctx context.Context,
	// Descend into the collection's folders. Off, only the requests at the
	// collection root run. Note that a defaulted-true bool cannot be set
	// false from the Go SDK — the zero value is dropped and the default
	// applies — so a non-recursive run is `--recursive=false` from the CLI.
	// +default=true
	recursive bool,
) (string, error) {
	exec, err := c.exec(ctx, recursive, nil)
	if err != nil {
		return "", err
	}
	code, err := exec.ExitCode(ctx)
	if err != nil {
		return "", err
	}
	out := combinedOutput(ctx, exec)
	switch code {
	case 0:
		return out, nil
	case exitCollectionFailed:
		return "", fmt.Errorf("bru run: the collection failed:\n%s", out)
	default:
		return "", usageError(code, out)
	}
}

// Report executes the collection and returns the reporter's artifact.
//
// Unlike Run it does not fail on exit 1: a Dagger function that returns an
// error forfeits its value, so a Report that gated would never hand back the
// file describing the failure — which is exactly when the file matters. CI
// pairs the two, taking the artifact from Report and the gate from Run. A
// usage error is still an error, because then there is no report to return.
//
// The run is recursive, matching Run's default, so the artifact describes the
// whole collection.
//
// +cache="never"
func (c *Collection) Report(
	ctx context.Context,
	// Reporter format: json, junit or html.
	format string,
) (*dagger.File, error) {
	path, err := reportPath(format)
	if err != nil {
		return nil, err
	}
	exec, err := c.exec(ctx, true, []string{format})
	if err != nil {
		return nil, err
	}
	code, err := exec.ExitCode(ctx)
	if err != nil {
		return nil, err
	}
	if code != 0 && code != exitCollectionFailed {
		return nil, usageError(code, combinedOutput(ctx, exec))
	}
	return exec.File(path), nil
}

func (c *Collection) clone() *Collection {
	out := *c
	out.VarNames = append([]string(nil), c.VarNames...)
	out.VarValues = append([]string(nil), c.VarValues...)
	out.SecretVarNames = append([]string(nil), c.SecretVarNames...)
	out.SecretVarValues = append([]*dagger.Secret(nil), c.SecretVarValues...)
	out.Tags = append([]string(nil), c.Tags...)
	out.ExcludedTags = append([]string(nil), c.ExcludedTags...)
	out.ServiceAliases = append([]string(nil), c.ServiceAliases...)
	out.Services = append([]*dagger.Service(nil), c.Services...)
	out.ClientCertHosts = append([]string(nil), c.ClientCertHosts...)
	out.ClientCerts = append([]*dagger.File(nil), c.ClientCerts...)
	out.ClientKeys = append([]*dagger.Secret(nil), c.ClientKeys...)
	out.ClientPassphrases = append([]*dagger.Secret(nil), c.ClientPassphrases...)
	return &out
}

// exec assembles the container and stages the `bru run`. reportFormats is empty
// for a plain run, and may name more than one reporter: bru accepts every
// `--reporter-*` flag at once, so a pipeline that wants both a JUnit file and an
// HTML page gets them out of the same pass rather than out of two runs that
// describe two different sets of responses.
//
// Expect=ReturnTypeAny keeps a non-zero exit on the value path: both terminals
// have to read the exit code and the output to say anything useful about it,
// and Report has to reach the artifact of a run that failed.
func (c *Collection) exec(ctx context.Context, recursive bool, reportFormats []string) (*dagger.Container, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	envFile, err := c.envFilePath(ctx)
	if err != nil {
		return nil, err
	}
	args, err := c.args(recursive, reportFormats, envFile)
	if err != nil {
		return nil, err
	}
	ctr, err := c.container(ctx, envFile)
	if err != nil {
		return nil, err
	}
	return ctr.WithExec(args, dagger.ContainerWithExecOpts{
		Expect: dagger.ReturnTypeAny,
	}), nil
}

// envFilePath is where WithEnvFile's file is mounted, keeping the extension it
// arrived with. bru selects its environment parser from that extension rather
// than from the contents, so a .json environment served up as .bru fails in
// the Bruno grammar with a stack trace instead of being read as JSON. An
// extension bru has no parser for is rejected here, where the message can say
// so.
func (c *Collection) envFilePath(ctx context.Context) (string, error) {
	if c.EnvFile == nil {
		return "", nil
	}
	name, err := c.EnvFile.Name(ctx)
	if err != nil {
		return "", fmt.Errorf("WithEnvFile: read the file's name: %w", err)
	}
	switch ext := strings.ToLower(filepath.Ext(name)); ext {
	case ".bru", ".json":
		return envFilePathPrefix + ext, nil
	default:
		return "", fmt.Errorf(
			"WithEnvFile: %q must be a .bru or .json file: bru picks its environment parser from the extension", name)
	}
}

// container mounts the collection at the image's own working directory and
// wires up everything that is not a command-line flag: the bound services, the
// secrets, which travel as environment variables precisely so they stay off
// argv, and the TLS material the `--cacert` and `--client-cert-config` flags
// point at.
func (c *Collection) container(ctx context.Context, envFile string) (*dagger.Container, error) {
	ctr := c.Bruno.Container().
		WithMountedDirectory(collectionDir, c.Source).
		WithWorkdir(collectionDir).
		WithEnvVariable(runNonceEnv, strconv.FormatInt(time.Now().UnixNano(), 10))
	for i, alias := range c.ServiceAliases {
		ctr = ctr.WithServiceBinding(alias, c.Services[i])
	}
	for i, name := range c.SecretVarNames {
		ctr = ctr.WithSecretVariable(name, c.SecretVarValues[i])
	}
	if c.EnvFile != nil {
		ctr = ctr.WithMountedFile(envFile, c.EnvFile)
	}
	return c.withTLS(ctx, ctr)
}

// args renders the `bru run` command line.
func (c *Collection) args(recursive bool, reportFormats []string, envFile string) ([]string, error) {
	// The collection root is named explicitly rather than left implicit:
	// `bru run` with no path descends into every folder whatever -r says, so
	// without the "." the recursive parameter would only ever mean "true".
	args := []string{"bru", "run", "."}
	if recursive {
		args = append(args, "-r")
	}
	if c.Environment != "" {
		args = append(args, "--env", c.Environment)
	}
	if c.EnvFile != nil {
		args = append(args, "--env-file", envFile)
	}
	for i, name := range c.VarNames {
		args = append(args, "--env-var", name+"="+c.VarValues[i])
	}
	if len(c.Tags) > 0 {
		args = append(args, "--tags", strings.Join(c.Tags, ","))
	}
	if len(c.ExcludedTags) > 0 {
		args = append(args, "--exclude-tags", strings.Join(c.ExcludedTags, ","))
	}
	if c.Sandbox != "" {
		args = append(args, "--sandbox", c.Sandbox)
	}
	if c.Insecure {
		args = append(args, "--insecure")
	}
	args = append(args, c.tlsArgs()...)
	if c.TestsOnly {
		args = append(args, "--tests-only")
	}
	if c.Bail {
		args = append(args, "--bail")
	}
	if c.Delay > 0 {
		args = append(args, "--delay", strconv.Itoa(c.Delay))
	}
	for _, format := range reportFormats {
		path, err := reportPath(format)
		if err != nil {
			return nil, err
		}
		args = append(args, "--reporter-"+format, path)
	}
	return args, nil
}

// validate reports the deferred builder validation. Every builder method
// returns a *Collection with no error, so a name bru would have misread is
// caught by the run that would have used it.
func (c *Collection) validate() error {
	for _, name := range c.VarNames {
		if err := checkVarName("WithVar", name); err != nil {
			return err
		}
		if strings.Contains(name, "=") {
			// `--env-var` takes name=value, so a name carrying an = would
			// silently set a different variable than the caller asked for.
			return fmt.Errorf("WithVar: variable name %q must not contain %q", name, "=")
		}
	}
	for _, name := range c.SecretVarNames {
		if err := checkVarName("WithSecretVar", name); err != nil {
			return err
		}
	}
	for _, alias := range c.ServiceAliases {
		if strings.TrimSpace(alias) == "" {
			return fmt.Errorf("WithService: service alias is required")
		}
	}
	switch c.Sandbox {
	case "", sandboxSafe, sandboxDeveloper:
	default:
		return fmt.Errorf("WithSandbox: invalid mode %q: must be one of %s, %s",
			c.Sandbox, sandboxSafe, sandboxDeveloper)
	}
	if c.Delay < 0 {
		return fmt.Errorf("WithDelay: delay %d must not be negative", c.Delay)
	}
	return c.validateTLS()
}

func checkVarName(fn string, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%s: variable name is required", fn)
	}
	return nil
}

// reportExtension maps a reporter format onto the extension its artifact
// carries. bru itself rejects an unknown format with exit 9, but only after
// running the whole collection against a live service; rejecting it here
// costs nothing and names the alternatives.
func reportExtension(format string) (string, error) {
	switch format {
	case formatJSON:
		return "json", nil
	case formatJUnit:
		return "xml", nil
	case formatHTML:
		return "html", nil
	default:
		return "", fmt.Errorf("invalid format %q: must be one of %s, %s, %s",
			format, formatJSON, formatJUnit, formatHTML)
	}
}

// reportPath is where a reporter writes its artifact inside the container. One
// path per format, so every reporter can be asked for in the same pass without
// two of them writing over each other.
func reportPath(format string) (string, error) {
	ext, err := reportExtension(format)
	if err != nil {
		return "", err
	}
	return reportPathPrefix + "." + ext, nil
}

// usageError turns one of bru's non-1 exits into an error that says what the
// code means, so a mistyped environment name never reads as a failing API.
func usageError(code int, out string) error {
	meaning, ok := exitMeanings[code]
	if !ok {
		meaning = "an unrecognised bru exit code"
	}
	return fmt.Errorf("bru run failed (exit %d: %s):\n%s", code, meaning, out)
}

// combinedOutput joins a finished exec's stdout and stderr. bru writes its
// run summary to stdout and its usage errors to stderr, so an error message
// built from either stream alone drops half of what went wrong.
func combinedOutput(ctx context.Context, exec *dagger.Container) string {
	stdout, _ := exec.Stdout(ctx)
	stderr, _ := exec.Stderr(ctx)
	return strings.TrimSpace(strings.TrimSpace(stdout) + "\n" + strings.TrimSpace(stderr))
}
