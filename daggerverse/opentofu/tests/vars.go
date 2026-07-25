package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"dagger/tests/internal/dagger"
)

// -------------------------------------------------------------- variables

// WithVarOverridesDefault asserts a hoisted variable reaches tofu and beats
// the declaration's own default.
func (t *Tests) WithVarOverridesDefault(ctx context.Context) error {
	return expectPlannedPrefix(ctx,
		opentofu().Config(fixture("basic")).WithVar("prefix", "from-var"),
		"from-var")
}

// WithVarFileSuppliesVariables asserts a .tfvars file supplies variables, and
// that it is reachable from where the module stages it — outside the root
// module, so it cannot collide with a file the configuration owns.
func (t *Tests) WithVarFileSuppliesVariables(ctx context.Context) error {
	return expectPlannedPrefix(ctx,
		opentofu().
			Config(fixture("basic")).
			WithVarFile(newFile("prod.tfvars", "prefix = \"from-var-file\"\n")),
		"from-var-file")
}

// WithEnvVariableIsVisibleToTofu asserts the plain environment escape hatch
// reaches the tofu process — here through TF_VAR_, the mechanism the secret
// variants ride on.
func (t *Tests) WithEnvVariableIsVisibleToTofu(ctx context.Context) error {
	return expectPlannedPrefix(ctx,
		opentofu().Config(fixture("basic")).WithEnvVariable("TF_VAR_prefix", "from-env"),
		"from-env")
}

// ---------------------------------------------------------------- secrets

// SecretVarReachesTofuWithoutLeaking asserts WithSecretVar delivers the value
// to tofu and that the plaintext appears in none of the artifacts the module
// hands back.
//
// The value is 64 hex characters of freshly generated randomness, so any
// occurrence anywhere in plan.txt, plan.json or apply.log is a real leak
// rather than a coincidence.
func (t *Tests) SecretVarReachesTofuWithoutLeaking(ctx context.Context) error {
	token, secret, err := testSecret(ctx, "secret-var")
	if err != nil {
		return err
	}
	cfg := opentofu().Config(fixture("secret-var")).WithSecretVar("token", secret)

	plan, err := pin(ctx, cfg.Plan())
	if err != nil {
		return fmt.Errorf("Plan with a secret variable: %w", err)
	}
	for _, name := range []string{planTextName, planJSONName} {
		contents, err := plan.File(name).Contents(ctx)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if strings.Contains(contents, token) {
			return fmt.Errorf("the secret variable's plaintext leaked into %s", name)
		}
	}

	result, err := pin(ctx, cfg.Apply())
	if err != nil {
		return fmt.Errorf("Apply with a secret variable: %w", err)
	}
	log, err := result.File(applyLogName).Contents(ctx)
	if err != nil {
		return fmt.Errorf("read %s: %w", applyLogName, err)
	}
	if strings.Contains(log, token) {
		return fmt.Errorf("the secret variable's plaintext leaked into %s", applyLogName)
	}

	return expectTokenLength(ctx, result, len(token))
}

// SecretVariableBindsEnvironmentSecret asserts the generic secret-environment
// modifier — the one provider credentials travel on — reaches tofu. It is
// exercised through TF_VAR_ because that is the only environment variable a
// hermetic, credential-free fixture can observe.
func (t *Tests) SecretVariableBindsEnvironmentSecret(ctx context.Context) error {
	token, secret, err := testSecret(ctx, "secret-env")
	if err != nil {
		return err
	}
	result, err := pin(ctx, opentofu().
		Config(fixture("secret-var")).
		WithSecretVariable("TF_VAR_token", secret).
		Apply())
	if err != nil {
		return fmt.Errorf("Apply with a secret environment variable: %w", err)
	}
	return expectTokenLength(ctx, result, len(token))
}

// -------------------------------------------------------------- workspaces

// WithWorkspaceIsolatesState asserts a selected workspace round-trips: the
// state Apply emits comes from the workspace's own state path, and feeding it
// back to a Config on the same workspace leaves nothing to do.
func (t *Tests) WithWorkspaceIsolatesState(ctx context.Context) error {
	const workspace = "staging"

	result, err := pin(ctx, opentofu().
		Config(fixture("basic")).
		WithWorkspace(workspace).
		Apply())
	if err != nil {
		return fmt.Errorf("Apply on workspace %q: %w", workspace, err)
	}
	state, err := decodeState(ctx, result.File(stateFileName))
	if err != nil {
		return err
	}
	want := []string{"random_integer.port", "random_pet.name"}
	if got := state.addresses(); !slices.Equal(got, want) {
		return fmt.Errorf("expected workspace state tracking %v, got %v", want, got)
	}

	plan, err := pin(ctx, opentofu().
		Config(fixture("basic")).
		WithWorkspace(workspace).
		WithState(result.File(stateFileName)).
		Plan())
	if err != nil {
		return fmt.Errorf("Plan on workspace %q: %w", workspace, err)
	}
	changes, err := plan.File(changesName).Contents(ctx)
	if err != nil {
		return fmt.Errorf("read %s: %w", changesName, err)
	}
	if changes != "none" {
		text, _ := plan.File(planTextName).Contents(ctx)
		return fmt.Errorf("expected the workspace's own state to be picked up, got changes=%q:\n%s", changes, text)
	}
	return nil
}

// ------------------------------------------------------------ plugin cache

// WithoutPluginCacheStillApplies asserts opting out of the shared provider
// cache leaves a working toolchain: init downloads its providers afresh and
// the lifecycle is otherwise unchanged.
func (t *Tests) WithoutPluginCacheStillApplies(ctx context.Context) error {
	result, err := pin(ctx, opentofu().
		Config(fixture("basic")).
		WithoutPluginCache().
		Apply())
	if err != nil {
		return fmt.Errorf("Apply without the plugin cache: %w", err)
	}
	state, err := decodeState(ctx, result.File(stateFileName))
	if err != nil {
		return err
	}
	want := []string{"random_integer.port", "random_pet.name"}
	if got := state.addresses(); !slices.Equal(got, want) {
		return fmt.Errorf("expected state tracking %v, got %v", want, got)
	}
	return nil
}

// ---------------------------------------------------------------- caching

// PlanShouldNotBeCached asserts two consecutive plans of the same
// configuration genuinely re-run tofu.
//
// +cache="never" governs the function result; the WithExec layers underneath
// are still content-addressed, which is why the module puts a per-call nonce
// on the run. A cached second call would hand back the first call's plan
// byte for byte, stamp included.
//
// The stamp tofu records has one-second resolution and nothing else in a plan
// of this fixture varies between runs — HCL's uuid() is unknown at plan time,
// and the random provider's values are too. So the two plans are deliberately
// spaced past a second boundary: a differing stamp then means tofu ran twice,
// and an identical one means the second call never executed.
func (t *Tests) PlanShouldNotBeCached(ctx context.Context) error {
	first, err := planTimestamp(ctx)
	if err != nil {
		return err
	}
	time.Sleep(planStampResolution)
	second, err := planTimestamp(ctx)
	if err != nil {
		return err
	}
	if first == second {
		return fmt.Errorf("expected two plans to re-execute, both stamped %q", first)
	}
	return nil
}

// ApplyShouldNotBeCached asserts two consecutive applies from an empty state
// genuinely re-run tofu: the random provider mints a fresh pet name each
// time, so an identical name would mean the second apply never happened.
func (t *Tests) ApplyShouldNotBeCached(ctx context.Context) error {
	first, err := applyName(ctx)
	if err != nil {
		return err
	}
	second, err := applyName(ctx)
	if err != nil {
		return err
	}
	if first == second {
		return fmt.Errorf("expected two applies to re-execute, both produced %q", first)
	}
	return nil
}

// ------------------------------------------------------------------ helpers

// testSecret mints a fresh 64-hex-character value and binds it as a secret.
// The name is uniquified so two tests running concurrently cannot collide on
// one secret name, and it carries none of the value.
func testSecret(ctx context.Context, label string) (string, *dagger.Secret, error) {
	value, err := dag.Random().Sha256(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("generate a test secret: %w", err)
	}
	id, err := dag.Random().UUIDV4(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("name a test secret: %w", err)
	}
	return value, dag.SetSecret("opentofu-"+label+"-"+id, value), nil
}

// expectPlannedPrefix plans the basic fixture and asserts tofu resolved the
// prefix variable to want. plan.json records the variable values the run
// used, which is the value tofu actually resolved rather than the one the
// caller believes it sent.
func expectPlannedPrefix(ctx context.Context, cfg *dagger.OpentofuConfig, want string) error {
	plan, err := pin(ctx, cfg.Plan())
	if err != nil {
		return fmt.Errorf("Plan: %w", err)
	}
	decoded, err := decodePlan(ctx, plan)
	if err != nil {
		return err
	}
	got, ok := decoded.Variables["prefix"]
	if !ok {
		return fmt.Errorf("expected the plan to record a prefix variable, got %v", decoded.Variables)
	}
	if fmt.Sprint(got.Value) != want {
		return fmt.Errorf("expected prefix=%q, got %v", want, got.Value)
	}
	return nil
}

// expectTokenLength asserts the secret-var fixture saw a token of the
// expected length — the proof the value reached tofu, given the fixture never
// exposes the value itself.
func expectTokenLength(ctx context.Context, result *dagger.Directory, want int) error {
	outputs, err := decodeOutputs(ctx, result.File(outputsName))
	if err != nil {
		return err
	}
	length, ok := outputs["token_length"]
	if !ok {
		return fmt.Errorf("expected a token_length output, got %v", outputs)
	}
	if fmt.Sprint(length.Value) != fmt.Sprint(float64(want)) {
		return fmt.Errorf("expected token_length=%d, got %v", want, length.Value)
	}
	return nil
}

// planStampResolution is how far apart two plans must be for tofu's
// one-second plan stamp to be guaranteed to differ.
const planStampResolution = 1100 * time.Millisecond

// planTimestamp plans the basic fixture and returns the stamp tofu recorded
// in the plan.
func planTimestamp(ctx context.Context) (string, error) {
	plan, err := pin(ctx, opentofu().Config(fixture("basic")).Plan())
	if err != nil {
		return "", fmt.Errorf("Plan the basic fixture: %w", err)
	}
	decoded, err := decodePlan(ctx, plan)
	if err != nil {
		return "", err
	}
	if decoded.Timestamp == "" {
		return "", fmt.Errorf("expected the plan to carry a timestamp, got none")
	}
	return decoded.Timestamp, nil
}

// applyName applies the basic fixture from an empty state and returns the pet
// name the random provider minted.
func applyName(ctx context.Context) (string, error) {
	result, err := pin(ctx, opentofu().Config(fixture("basic")).Apply())
	if err != nil {
		return "", fmt.Errorf("Apply the basic fixture: %w", err)
	}
	outputs, err := decodeOutputs(ctx, result.File(outputsName))
	if err != nil {
		return "", err
	}
	name, ok := outputs["name"]
	if !ok {
		return "", fmt.Errorf("expected a name output, got %v", outputs)
	}
	return fmt.Sprint(name.Value), nil
}
