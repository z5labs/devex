package main

import (
	"context"
	"fmt"
	"path"
	"strings"

	"dagger/opentofu/internal/dagger"
)

const (
	// workDir is where the caller's root module is copied. It is a copy, not a
	// mount: tofu writes .terraform/, the dependency lock file, the plan file
	// and (in file-carried mode) the state next to the configuration.
	workDir = "/src"

	// pluginCacheDir backs TF_PLUGIN_CACHE_DIR. It is a Dagger cache volume,
	// so provider downloads survive across calls and across modules.
	pluginCacheDir = "/plugin-cache"

	// pluginCacheVolume names the shared provider cache. LOCKED sharing
	// serialises concurrent writers: the tofu provider cache is not
	// concurrency-safe, and a parallel test suite would otherwise have several
	// `tofu init` runs unpacking into it at once.
	pluginCacheVolume = "opentofu-plugin-cache"

	// varFileDir and backendFileDir stage caller-supplied files outside the
	// root module, so they never collide with a file the configuration owns.
	varFileDir     = "/tmp/tofu-var-files"
	backendFileDir = "/tmp/tofu-backend-config"

	// stateFileName is the local backend's state file, relative to the root
	// module (or to terraform.tfstate.d/<workspace>/ when a workspace is
	// selected).
	stateFileName = "terraform.tfstate"

	// workspaceStateDir is where the local backend keeps non-default
	// workspaces' state.
	workspaceStateDir = "terraform.tfstate.d"

	// planFileName is the saved plan Plan emits and Apply consumes.
	planFileName = "plan.tfplan"

	// planJSONFileName, planTextFileName and changesFileName are the readings
	// Plan derives from that saved plan: the machine-readable form, the
	// human-readable one, and the one-word verdict Ci's drift gate reads.
	planJSONFileName = "plan.json"
	planTextFileName = "plan.txt"
	changesFileName  = "changes"

	// mountedPlanPath is where Apply mounts a caller-supplied saved plan. It
	// lives inside the root module because tofu resolves the plan file
	// relative to the working directory.
	mountedPlanPath = workDir + "/" + planFileName

	// secretVarPrefix is how tofu picks up an input variable from the
	// environment, which is how WithSecretVar keeps plaintext out of argv.
	secretVarPrefix = "TF_VAR_"

	// changesNone and changesPresent are the two values Plan writes into the
	// `changes` file, derived from `-detailed-exitcode`.
	changesNone    = "none"
	changesPresent = "changes"

	// planExitChanges is what `tofu plan -detailed-exitcode` returns when the
	// plan is non-empty. 0 means no changes and 1 means the plan failed.
	planExitChanges = 2
)

// Config is a bound root module plus the settings that apply to nearly every
// tofu subcommand. It is immutable: every With* returns a copy.
//
// Variables, credentials and backend settings are hoisted here as chained
// modifiers rather than repeated as optional parameters across eight
// lifecycle signatures.
type Config struct {
	// +private
	Opentofu *Opentofu
	// +private
	Source *dagger.Directory

	// +private
	VarNames []string
	// +private
	VarValues []string
	// +private
	SecretVarNames []string
	// +private
	SecretVarValues []*dagger.Secret
	// +private
	VarFiles []*dagger.File

	// +private
	EnvNames []string
	// +private
	EnvValues []string
	// +private
	SecretEnvNames []string
	// +private
	SecretEnvValues []*dagger.Secret

	// +private
	BackendNames []string
	// +private
	BackendValues []string
	// +private
	BackendFiles []*dagger.File

	// +private
	Workspace string
	// +private
	NoPluginCache bool
	// +private
	State *dagger.File
}

// WithVar sets an input variable (`-var name=value`).
//
// It takes a name and a value rather than a map because Dagger functions
// cannot accept map parameters. Use WithSecretVar for anything sensitive:
// a value passed here lands in argv and in the plan.
func (c *Config) WithVar(name string, value string) *Config {
	out := c.clone()
	out.VarNames = append(out.VarNames, name)
	out.VarValues = append(out.VarValues, value)
	return out
}

// WithSecretVar sets an input variable from a secret.
//
// The value is bound as the container environment variable TF_VAR_<name>
// rather than passed as `-var name=value`, so the plaintext never enters
// argv, the CLI log, or a saved plan's command line. tofu still marks the
// variable's own value in the plan unless the variable is declared
// `sensitive = true`, which a configuration handling secrets should do.
func (c *Config) WithSecretVar(name string, value *dagger.Secret) *Config {
	out := c.clone()
	out.SecretVarNames = append(out.SecretVarNames, name)
	out.SecretVarValues = append(out.SecretVarValues, value)
	return out
}

// WithVarFile adds a variable definitions file (`-var-file`). The file is
// staged outside the root module so it cannot collide with a file the
// configuration owns; tofu is pointed at the staged path.
func (c *Config) WithVarFile(file *dagger.File) *Config {
	out := c.clone()
	out.VarFiles = append(out.VarFiles, file)
	return out
}

// WithEnvVariable sets a plain environment variable on every tofu exec — the
// escape hatch for the non-sensitive knobs tofu reads from the environment
// (TF_LOG, TF_CLI_ARGS_*, provider region settings, ...). Credentials belong
// in WithSecretVariable.
func (c *Config) WithEnvVariable(name string, value string) *Config {
	out := c.clone()
	out.EnvNames = append(out.EnvNames, name)
	out.EnvValues = append(out.EnvValues, value)
	return out
}

// WithSecretVariable binds a secret as an environment variable on every tofu
// exec. This is how provider credentials reach tofu — AWS_ACCESS_KEY_ID,
// AWS_SECRET_ACCESS_KEY, TF_TOKEN_app_terraform_io and friends — as
// *dagger.Secret, never as a string.
func (c *Config) WithSecretVariable(name string, value *dagger.Secret) *Config {
	out := c.clone()
	out.SecretEnvNames = append(out.SecretEnvNames, name)
	out.SecretEnvValues = append(out.SecretEnvValues, value)
	return out
}

// WithBackendConfig sets one backend setting (`-backend-config=name=value`),
// merged into the configuration's own backend block at init.
//
// Selecting a remote backend is mutually exclusive with WithState: they are
// two different answers to where state lives, and combining them is rejected
// rather than silently resolved.
func (c *Config) WithBackendConfig(name string, value string) *Config {
	out := c.clone()
	out.BackendNames = append(out.BackendNames, name)
	out.BackendValues = append(out.BackendValues, value)
	return out
}

// WithBackendConfigFile adds a backend settings file
// (`-backend-config=<file>`). Like WithBackendConfig, it is mutually
// exclusive with WithState.
func (c *Config) WithBackendConfigFile(file *dagger.File) *Config {
	out := c.clone()
	out.BackendFiles = append(out.BackendFiles, file)
	return out
}

// WithWorkspace selects a tofu workspace, creating it when it does not exist
// (`tofu workspace select -or-create`). With the local backend this moves the
// state to terraform.tfstate.d/<name>/terraform.tfstate, which is where
// WithState writes and where Apply reads the emitted state from.
func (c *Config) WithWorkspace(name string) *Config {
	out := c.clone()
	out.Workspace = name
	return out
}

// WithoutPluginCache disables the shared provider cache, so every init
// downloads its providers afresh.
//
// This is a Without* modifier rather than a `pluginCache bool` parameter
// defaulting to true, because a `+default=true` bool cannot be set back to
// false from the Go SDK: the zero value is dropped before it reaches the
// engine.
func (c *Config) WithoutPluginCache() *Config {
	out := c.clone()
	out.NoPluginCache = true
	return out
}

// WithState supplies the state file for file-carried mode: state is written
// into the container, and every mutating operation hands the resulting
// terraform.tfstate back out in its output directory. Fully hermetic, no
// backend required, and the caller owns persistence.
//
// Omit it entirely for a first apply against an empty state.
func (c *Config) WithState(state *dagger.File) *Config {
	out := c.clone()
	out.State = state
	return out
}

// ------------------------------------------------------------------ lifecycle

// Fmt reports formatting drift with `tofu fmt -check -diff -recursive`. It
// returns the diff and fails when anything needs rewriting, so it is usable
// as a CI gate; rewriting in place is a separate function.
//
// A failing run carries the diff in the error rather than the return value:
// Dagger drops a function's value whenever its error is non-nil.
//
// +cache="session"
func (c *Config) Fmt(ctx context.Context) (string, error) {
	if err := c.validate(); err != nil {
		return "", err
	}
	exec := c.container().WithExec(
		[]string{"tofu", "fmt", "-check", "-diff", "-recursive", "-no-color"},
		dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny},
	)
	code, err := exec.ExitCode(ctx)
	if err != nil {
		return "", err
	}
	out := combinedOutput(ctx, exec)
	if code != 0 {
		return "", fmt.Errorf("tofu fmt: configuration is not formatted (exit %d):\n%s", code, out)
	}
	return out, nil
}

// Validate checks the configuration for internal consistency with
// `tofu validate`.
//
// It initialises with `-backend=false` first, so a configuration declaring a
// remote backend validates without any credentials and without touching the
// backend at all. Providers are still installed, because validation needs
// their schemas.
//
// +cache="session"
func (c *Config) Validate(ctx context.Context) error {
	if err := c.validate(); err != nil {
		return err
	}
	initExec := c.container().WithExec(
		// No backend settings here: tofu rejects -backend-config on an init
		// that skips the backend, which is exactly the point of this one.
		c.initArgs(false),
		dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny},
	)
	if err := expectSuccess(ctx, initExec, "tofu init -backend=false"); err != nil {
		return err
	}
	exec := initExec.WithExec(
		[]string{"tofu", "validate", "-no-color"},
		dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny},
	)
	return expectSuccess(ctx, exec, "tofu validate")
}

// Init runs a full `tofu init` — backend included — and returns the root
// module with the dependency lock file init produced.
//
// .terraform/ is stripped from the result: with the shared provider cache in
// play its provider entries are symlinks into a cache volume that does not
// exist outside this container, so carrying them out would hand the caller
// dangling links. The lock file is the portable artifact of an init, and it
// is what a repo commits.
//
// +cache="session"
func (c *Config) Init(ctx context.Context) (*dagger.Directory, error) {
	exec, err := c.initialized(ctx)
	if err != nil {
		return nil, err
	}
	return exec.Directory(workDir).WithoutDirectory(".terraform"), nil
}

// Plan produces a saved plan and everything needed to read it, in a single
// tofu run: plan.tfplan (the saved plan Apply consumes), plan.json
// (`tofu show -json`), plan.txt (the human-readable rendering) and changes
// (`none` or `changes`, from `-detailed-exitcode`).
//
// One run, not two: under +cache="never" a second `tofu plan` invocation to
// obtain the JSON form could legitimately disagree with the first. The JSON
// and text renderings are derived from the saved plan file, so they describe
// exactly the plan that was made.
//
// +cache="never"
func (c *Config) Plan(
	ctx context.Context,
	// Plan the destruction of all remote objects (`-destroy`).
	// +default=false
	destroy bool,
	// Limit the plan to these resource addresses (`-target`).
	// +optional
	targets []string,
) (*dagger.Directory, error) {
	ctr, err := c.initialized(ctx)
	if err != nil {
		return nil, err
	}
	nonce, err := randHex()
	if err != nil {
		return nil, err
	}

	args := []string{"tofu", "plan", "-input=false", "-no-color", "-detailed-exitcode", "-out=" + planFileName}
	if destroy {
		args = append(args, "-destroy")
	}
	args = append(args, c.varArgs()...)
	args = append(args, targetArgs(targets)...)

	exec := ctr.
		WithEnvVariable("TOFU_RUN_NONCE", nonce).
		WithExec(args, dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny})
	code, err := exec.ExitCode(ctx)
	if err != nil {
		return nil, err
	}
	changes := changesNone
	switch code {
	case 0:
	case planExitChanges:
		changes = changesPresent
	default:
		return nil, fmt.Errorf("tofu plan failed (exit %d):\n%s", code, combinedOutput(ctx, exec))
	}

	planJSON, err := exec.
		WithExec([]string{"tofu", "show", "-json", planFileName}).
		Stdout(ctx)
	if err != nil {
		return nil, fmt.Errorf("tofu show -json %s: %s", planFileName, errText(err))
	}
	planText, err := exec.
		WithExec([]string{"tofu", "show", "-no-color", planFileName}).
		Stdout(ctx)
	if err != nil {
		return nil, fmt.Errorf("tofu show %s: %s", planFileName, errText(err))
	}

	return dag.Directory().
		WithFile(planFileName, exec.File(path.Join(workDir, planFileName))).
		WithNewFile(planJSONFileName, planJSON).
		WithNewFile(planTextFileName, planText).
		WithNewFile(changesFileName, changes), nil
}

// Apply realises the configuration and returns terraform.tfstate (in
// file-carried mode), outputs.json and apply.log.
//
// Pass the plan.tfplan emitted by Plan to apply exactly that plan; without
// one, Apply plans and applies in a single run. A saved plan already carries
// its variables and target set, so neither is re-sent with it.
//
// A non-zero tofu exit is an error, and Dagger drops a function's value when
// its error is non-nil — so a partially failed apply forfeits the state it
// produced in file-carried mode. That is deliberate: the alternative, always
// returning the directory with an exit code inside it, turns a failed apply
// into a silent green whenever a caller forgets to look.
//
// +cache="never"
func (c *Config) Apply(
	ctx context.Context,
	// A saved plan from Plan. Without it, Apply plans and applies in one run.
	// +optional
	plan *dagger.File,
	// Limit the apply to these resource addresses (`-target`). Rejected
	// alongside a saved plan, which already fixes what it changes.
	// +optional
	targets []string,
) (*dagger.Directory, error) {
	if plan != nil && len(targets) > 0 {
		return nil, fmt.Errorf("Apply: targets cannot be combined with a saved plan; the plan already fixes what it changes — pass targets to Plan instead")
	}
	ctr, err := c.initialized(ctx)
	if err != nil {
		return nil, err
	}

	args := []string{"tofu", "apply", "-input=false", "-no-color"}
	if plan != nil {
		ctr = ctr.WithMountedFile(mountedPlanPath, plan)
		args = append(args, planFileName)
	} else {
		args = append(args, "-auto-approve")
		args = append(args, c.varArgs()...)
		args = append(args, targetArgs(targets)...)
	}
	return c.runMutation(ctx, ctr, args, "tofu apply")
}

// Destroy tears down everything the state tracks and returns the post-destroy
// terraform.tfstate (in file-carried mode), outputs.json and destroy.log.
//
// It is rejected outright when there is neither state to destroy nor a
// backend to read it from: tofu would happily report "0 destroyed" against an
// empty state, which reads as success while leaving the real infrastructure
// untouched.
//
// +cache="never"
func (c *Config) Destroy(
	ctx context.Context,
	// Limit the destroy to these resource addresses (`-target`).
	// +optional
	targets []string,
) (*dagger.Directory, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	hasState, err := c.hasState(ctx)
	if err != nil {
		return nil, err
	}
	hasBackend, err := c.hasBackend(ctx)
	if err != nil {
		return nil, err
	}
	if !hasState && !hasBackend {
		return nil, fmt.Errorf(
			"Destroy: no state to destroy — supply file-carried state with WithState, " +
				"or configure a remote backend; destroying against an empty state " +
				"would report success while leaving the infrastructure untouched")
	}

	ctr, err := c.initialized(ctx)
	if err != nil {
		return nil, err
	}
	args := []string{"tofu", "destroy", "-input=false", "-no-color", "-auto-approve"}
	args = append(args, c.varArgs()...)
	args = append(args, targetArgs(targets)...)
	return c.runMutation(ctx, ctr, args, "tofu destroy")
}

// Outputs returns the root module's output values as JSON
// (`tofu output -json`).
//
// +cache="never"
func (c *Config) Outputs(ctx context.Context) (string, error) {
	return c.read(ctx, []string{"tofu", "output", "-json", "-no-color"}, "tofu output -json")
}

// Show returns the human-readable rendering of the current state
// (`tofu show`).
//
// +cache="never"
func (c *Config) Show(ctx context.Context) (string, error) {
	return c.read(ctx, []string{"tofu", "show", "-no-color"}, "tofu show")
}

// ------------------------------------------------------------------- internals

func (c *Config) clone() *Config {
	out := *c
	out.VarNames = append([]string(nil), c.VarNames...)
	out.VarValues = append([]string(nil), c.VarValues...)
	out.SecretVarNames = append([]string(nil), c.SecretVarNames...)
	out.SecretVarValues = append([]*dagger.Secret(nil), c.SecretVarValues...)
	out.VarFiles = append([]*dagger.File(nil), c.VarFiles...)
	out.EnvNames = append([]string(nil), c.EnvNames...)
	out.EnvValues = append([]string(nil), c.EnvValues...)
	out.SecretEnvNames = append([]string(nil), c.SecretEnvNames...)
	out.SecretEnvValues = append([]*dagger.Secret(nil), c.SecretEnvValues...)
	out.BackendNames = append([]string(nil), c.BackendNames...)
	out.BackendValues = append([]string(nil), c.BackendValues...)
	out.BackendFiles = append([]*dagger.File(nil), c.BackendFiles...)
	return &out
}

// validate reports everything the builder methods could not, because a
// builder has no error return: the two state modes being mutually exclusive,
// and the name constraints on the `name=value` flags.
func (c *Config) validate() error {
	if c.State != nil && (len(c.BackendNames) > 0 || len(c.BackendFiles) > 0) {
		return fmt.Errorf(
			"WithState and WithBackendConfig/WithBackendConfigFile are mutually exclusive: " +
				"file-carried state keeps the state in the returned directory, a remote backend " +
				"keeps it in the backend — pick one")
	}
	for _, name := range c.VarNames {
		if err := checkPairName("WithVar", "variable name", name); err != nil {
			return err
		}
	}
	for _, name := range c.SecretVarNames {
		if err := checkPairName("WithSecretVar", "variable name", name); err != nil {
			return err
		}
	}
	for _, name := range c.BackendNames {
		if err := checkPairName("WithBackendConfig", "setting name", name); err != nil {
			return err
		}
	}
	for _, name := range c.EnvNames {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("WithEnvVariable: variable name is required")
		}
	}
	for _, name := range c.SecretEnvNames {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("WithSecretVariable: variable name is required")
		}
	}
	return nil
}

// container copies the root module into a writable workdir and binds
// everything that applies to every subcommand: the provider cache, staged
// var/backend files, environment variables, secret variables and state.
//
// The source is copied rather than mounted because tofu writes next to the
// configuration — .terraform/, the lock file, the plan file and, in
// file-carried mode, the state.
func (c *Config) container() *dagger.Container {
	ctr := c.Opentofu.Container().
		WithDirectory(workDir, c.Source).
		WithWorkdir(workDir)

	if !c.NoPluginCache {
		ctr = ctr.
			WithMountedCache(
				pluginCacheDir,
				dag.CacheVolume(pluginCacheVolume),
				dagger.ContainerWithMountedCacheOpts{Sharing: dagger.CacheSharingModeLocked},
			).
			WithEnvVariable("TF_PLUGIN_CACHE_DIR", pluginCacheDir)
	}

	for i, f := range c.VarFiles {
		ctr = ctr.WithMountedFile(varFilePath(i), f)
	}
	for i, f := range c.BackendFiles {
		ctr = ctr.WithMountedFile(backendFilePath(i), f)
	}
	for i, name := range c.EnvNames {
		ctr = ctr.WithEnvVariable(name, c.EnvValues[i])
	}
	for i, name := range c.SecretEnvNames {
		ctr = ctr.WithSecretVariable(name, c.SecretEnvValues[i])
	}
	for i, name := range c.SecretVarNames {
		ctr = ctr.WithSecretVariable(secretVarPrefix+name, c.SecretVarValues[i])
	}
	if c.State != nil {
		ctr = ctr.WithFile(path.Join(workDir, c.statePath()), c.State)
	}
	return ctr
}

// initialized returns a container whose root module has been initialised and,
// when one was selected, switched to the requested workspace.
func (c *Config) initialized(ctx context.Context) (*dagger.Container, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	exec := c.container().WithExec(
		append(c.initArgs(true), c.backendArgs()...),
		dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny},
	)
	if err := expectSuccess(ctx, exec, "tofu init"); err != nil {
		return nil, err
	}
	if c.Workspace == "" {
		return exec, nil
	}
	ws := exec.WithExec(
		[]string{"tofu", "workspace", "select", "-or-create=true", c.Workspace},
		dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny},
	)
	if err := expectSuccess(ctx, ws, "tofu workspace select "+c.Workspace); err != nil {
		return nil, err
	}
	return ws, nil
}

// runMutation executes an apply or destroy and packages its results:
// terraform.tfstate when the state is file-carried, the output values, and
// the run's own log.
func (c *Config) runMutation(ctx context.Context, ctr *dagger.Container, args []string, label string) (*dagger.Directory, error) {
	nonce, err := randHex()
	if err != nil {
		return nil, err
	}
	logName := strings.TrimPrefix(label, "tofu ") + ".log"

	exec := ctr.
		WithEnvVariable("TOFU_RUN_NONCE", nonce).
		WithExec(args, dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny})
	code, err := exec.ExitCode(ctx)
	if err != nil {
		return nil, err
	}
	log := combinedOutput(ctx, exec)
	if code != 0 {
		return nil, fmt.Errorf("%s failed (exit %d):\n%s", label, code, log)
	}

	outputs, err := exec.
		WithExec([]string{"tofu", "output", "-json", "-no-color"}).
		Stdout(ctx)
	if err != nil {
		return nil, fmt.Errorf("tofu output -json: %s", errText(err))
	}

	out := dag.Directory().
		WithNewFile("outputs.json", outputs).
		WithNewFile(logName, log)

	hasBackend, err := c.hasBackend(ctx)
	if err != nil {
		return nil, err
	}
	if !hasBackend {
		out = out.WithFile(stateFileName, exec.File(path.Join(workDir, c.statePath())))
	}
	return out, nil
}

// read runs a read-only tofu subcommand against the initialised root module
// and returns its stdout.
func (c *Config) read(ctx context.Context, args []string, label string) (string, error) {
	ctr, err := c.initialized(ctx)
	if err != nil {
		return "", err
	}
	nonce, err := randHex()
	if err != nil {
		return "", err
	}
	exec := ctr.
		WithEnvVariable("TOFU_RUN_NONCE", nonce).
		WithExec(args, dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny})
	code, err := exec.ExitCode(ctx)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("%s failed (exit %d):\n%s", label, code, combinedOutput(ctx, exec))
	}
	out, err := exec.Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("%s: %s", label, errText(err))
	}
	return out, nil
}

// initArgs renders `tofu init`. backend=false is how Validate reaches a
// configuration that declares a remote backend without any credentials.
func (c *Config) initArgs(backend bool) []string {
	args := []string{"tofu", "init", "-input=false", "-no-color"}
	if !backend {
		args = append(args, "-backend=false")
	}
	return args
}

// backendArgs renders the hoisted backend settings. They are dropped on an
// init that skips the backend, where tofu rejects them.
func (c *Config) backendArgs() []string {
	var args []string
	for i, name := range c.BackendNames {
		args = append(args, "-backend-config="+name+"="+c.BackendValues[i])
	}
	for i := range c.BackendFiles {
		args = append(args, "-backend-config="+backendFilePath(i))
	}
	return args
}

// varArgs renders the hoisted variables. Secret variables are absent by
// design: they travel as TF_VAR_<name> environment secrets so their values
// never reach argv.
func (c *Config) varArgs() []string {
	var args []string
	for i, name := range c.VarNames {
		args = append(args, "-var", name+"="+c.VarValues[i])
	}
	for i := range c.VarFiles {
		args = append(args, "-var-file="+varFilePath(i))
	}
	return args
}

// statePath is where the local backend keeps state, relative to the root
// module. A selected workspace moves it under terraform.tfstate.d.
func (c *Config) statePath() string {
	if c.Workspace == "" || c.Workspace == "default" {
		return stateFileName
	}
	return path.Join(workspaceStateDir, c.Workspace, stateFileName)
}

// hasState reports whether there is any state to work from: one supplied via
// WithState, or one the source tree already carries.
func (c *Config) hasState(ctx context.Context) (bool, error) {
	if c.State != nil {
		return true, nil
	}
	matches, err := c.Source.Glob(ctx, c.statePath())
	if err != nil {
		return false, fmt.Errorf("look for %s in the root module: %s", c.statePath(), errText(err))
	}
	return len(matches) > 0, nil
}

// hasBackend reports whether state lives somewhere other than a local file:
// either the caller supplied backend settings, or the configuration itself
// declares a `backend` or `cloud` block.
//
// The configuration is inspected rather than assumed, because a repo that
// declares its backend in-tree needs no WithBackendConfig at all.
func (c *Config) hasBackend(ctx context.Context) (bool, error) {
	if len(c.BackendNames) > 0 || len(c.BackendFiles) > 0 {
		return true, nil
	}
	matches, err := c.Source.Glob(ctx, "**/*.tf")
	if err != nil {
		return false, fmt.Errorf("search the root module for *.tf: %s", errText(err))
	}
	for _, name := range matches {
		// Sub-modules cannot declare a backend; only the root module's own
		// files decide where state lives.
		if strings.Contains(name, "/") {
			continue
		}
		contents, err := c.Source.File(name).Contents(ctx)
		if err != nil {
			return false, fmt.Errorf("read %s: %s", name, errText(err))
		}
		if declaresBackend(contents) {
			return true, nil
		}
	}
	return false, nil
}

// declaresBackend reports whether a root-module file opens a `backend` or
// `cloud` block. It is a scan, not a parse: the answer only decides whether
// state is expected to come back as a file, and tofu itself is the authority
// on the configuration's validity.
func declaresBackend(contents string) bool {
	for line := range strings.SplitSeq(contents, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		if strings.HasPrefix(line, "backend ") || strings.HasPrefix(line, "backend\"") {
			return true
		}
		if strings.HasPrefix(line, "cloud ") || strings.HasPrefix(line, "cloud{") {
			return true
		}
	}
	return false
}

func varFilePath(i int) string {
	return fmt.Sprintf("%s/vars-%d.tfvars", varFileDir, i)
}

func backendFilePath(i int) string {
	return fmt.Sprintf("%s/backend-%d.hcl", backendFileDir, i)
}

func targetArgs(targets []string) []string {
	var args []string
	for _, t := range targets {
		args = append(args, "-target="+t)
	}
	return args
}

// checkPairName rejects a name that would change which flag tofu sees. Every
// hoisted setting renders as `name=value`, so an empty name or one containing
// `=` would silently set something other than what the caller asked for.
func checkPairName(fn string, what string, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%s: %s is required", fn, what)
	}
	if strings.Contains(name, "=") {
		return fmt.Errorf("%s: %s %q must not contain %q", fn, what, name, "=")
	}
	return nil
}

// expectSuccess turns a non-zero exit into an error carrying tofu's own
// diagnostics.
func expectSuccess(ctx context.Context, exec *dagger.Container, label string) error {
	code, err := exec.ExitCode(ctx)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("%s failed (exit %d):\n%s", label, code, combinedOutput(ctx, exec))
	}
	return nil
}

// errText renders an SDK error for embedding in a message. Wrapping with %w
// is pointless at a module boundary: Dagger unwraps to the inner GraphQL
// error on the way out, so the wrapping context is lost.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
