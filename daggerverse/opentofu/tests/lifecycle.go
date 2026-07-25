package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"dagger/tests/internal/dagger"
)

// ------------------------------------------------------------------- init

// InitProducesLockFile asserts Init emits the dependency lock file — the
// portable artifact of an init, and the one a repo commits — and that it does
// not hand back the .terraform provider directory, whose entries are symlinks
// into a cache volume that does not exist outside the container.
func (t *Tests) InitProducesLockFile(ctx context.Context) error {
	entries, err := opentofu().Config(fixture("basic")).Init().Entries(ctx)
	if err != nil {
		return fmt.Errorf("Init: %w", err)
	}
	if !slices.Contains(entries, lockFileName) {
		return fmt.Errorf("expected %s in the initialised module, got %v", lockFileName, entries)
	}
	for _, e := range entries {
		if strings.TrimSuffix(e, "/") == ".terraform" {
			return fmt.Errorf("expected .terraform to be stripped from the result, got %v", entries)
		}
	}
	return nil
}

// ------------------------------------------------------------------- lock

// LockCoversRequestedPlatforms asserts a multi-platform lock produces a lock
// file that records the provider with a full set of hashes, and that it covers
// at least what a single-platform lock does.
//
// The assertion is structural rather than per-platform because the lock file
// itself carries no platform labels: `hashes` is one flat list. What proves
// each requested platform is genuinely resolved is
// LockRejectsUnavailablePlatform, where naming a platform the provider does not
// publish fails the run.
func (t *Tests) LockCoversRequestedPlatforms(ctx context.Context) error {
	multi, err := lockHashes(ctx, opentofu().
		Config(fixture("basic")).
		Lock(dagger.OpentofuConfigLockOpts{
			Platforms: []string{"linux_amd64", "darwin_arm64", "windows_amd64"},
		}))
	if err != nil {
		return fmt.Errorf("Lock for three platforms: %w", err)
	}
	if len(multi) == 0 {
		return fmt.Errorf("expected the lock file to record hashes, got none")
	}

	single, err := lockHashes(ctx, opentofu().
		Config(fixture("basic")).
		Lock(dagger.OpentofuConfigLockOpts{Platforms: []string{"linux_amd64"}}))
	if err != nil {
		return fmt.Errorf("Lock for one platform: %w", err)
	}
	for _, hash := range single {
		if !slices.Contains(multi, hash) {
			return fmt.Errorf("expected the multi-platform lock to cover %s, got %v", hash, multi)
		}
	}
	return nil
}

// LockRejectsUnavailablePlatform asserts every requested platform is actually
// fetched: one the provider does not publish fails the lock, naming it. This
// is what gives LockCoversRequestedPlatforms its teeth.
//
// openbsd_s390x is a safe stand-in for "will never exist" — Go has no OpenBSD
// port for s390x, so no provider can publish a package for it.
func (t *Tests) LockRejectsUnavailablePlatform(ctx context.Context) error {
	_, err := opentofu().
		Config(fixture("basic")).
		Lock(dagger.OpentofuConfigLockOpts{
			Platforms: []string{"linux_amd64", "openbsd_s390x"},
		}).
		Sync(ctx)
	return expectErrorContains(err, "tofu providers lock", "openbsd_s390x")
}

// LockWithoutPlatformsProducesUsableLockFile asserts the default — no
// platforms named — still writes a lock file for the platform tofu runs on,
// and one tofu itself accepts: the returned tree initialises against it.
func (t *Tests) LockWithoutPlatformsProducesUsableLockFile(ctx context.Context) error {
	locked := opentofu().Config(fixture("basic")).Lock()

	hashes, err := lockHashes(ctx, locked)
	if err != nil {
		return fmt.Errorf("Lock: %w", err)
	}
	if len(hashes) == 0 {
		return fmt.Errorf("expected the lock file to record hashes, got none")
	}

	raw, err := locked.File(lockFileName).Contents(ctx)
	if err != nil {
		return fmt.Errorf("read %s: %w", lockFileName, err)
	}
	if !strings.Contains(raw, "hashicorp/random") {
		return fmt.Errorf("expected the lock file to record the fixture's provider, got:\n%s", raw)
	}
	if err := opentofu().Config(locked).Validate(ctx); err != nil {
		return fmt.Errorf("initialise against the generated lock file: %w", err)
	}
	return nil
}

// ------------------------------------------------------------------- plan

// PlanReportsChanges asserts a plan against an empty state emits all four
// artifacts, flags the run as having changes, and describes both resources
// the fixture declares.
func (t *Tests) PlanReportsChanges(ctx context.Context) error {
	plan, err := pin(ctx, opentofu().Config(fixture("basic")).Plan())
	if err != nil {
		return fmt.Errorf("Plan: %w", err)
	}

	entries, err := plan.Entries(ctx)
	if err != nil {
		return fmt.Errorf("read the plan directory: %w", err)
	}
	for _, want := range []string{planFileName, planJSONName, planTextName, changesName} {
		if !slices.Contains(entries, want) {
			return fmt.Errorf("expected %s in the plan directory, got %v", want, entries)
		}
	}

	changes, err := plan.File(changesName).Contents(ctx)
	if err != nil {
		return fmt.Errorf("read %s: %w", changesName, err)
	}
	if changes != "changes" {
		return fmt.Errorf("expected changes=%q for a plan against an empty state, got %q", "changes", changes)
	}

	decoded, err := decodePlan(ctx, plan)
	if err != nil {
		return err
	}
	addresses := decoded.addresses()
	want := []string{"random_integer.port", "random_pet.name"}
	if !slices.Equal(addresses, want) {
		return fmt.Errorf("expected planned changes for %v, got %v", want, addresses)
	}
	for _, rc := range decoded.ResourceChanges {
		if !slices.Equal(rc.Change.Actions, []string{"create"}) {
			return fmt.Errorf("expected %s to be created, got actions %v", rc.Address, rc.Change.Actions)
		}
	}

	text, err := plan.File(planTextName).Contents(ctx)
	if err != nil {
		return fmt.Errorf("read %s: %w", planTextName, err)
	}
	if !strings.Contains(text, "random_pet.name") {
		return fmt.Errorf("expected the rendered plan to mention random_pet.name, got:\n%s", text)
	}

	size, err := plan.File(planFileName).Size(ctx)
	if err != nil {
		return fmt.Errorf("stat %s: %w", planFileName, err)
	}
	if size == 0 {
		return fmt.Errorf("expected a non-empty saved plan")
	}
	return nil
}

// PlanTargetsLimitScope asserts -target narrows the plan to the named
// resource, leaving the fixture's other resource untouched.
func (t *Tests) PlanTargetsLimitScope(ctx context.Context) error {
	plan, err := pin(ctx, opentofu().
		Config(fixture("basic")).
		Plan(dagger.OpentofuConfigPlanOpts{Targets: []string{"random_pet.name"}}))
	if err != nil {
		return fmt.Errorf("Plan with targets: %w", err)
	}
	decoded, err := decodePlan(ctx, plan)
	if err != nil {
		return err
	}
	want := []string{"random_pet.name"}
	if got := decoded.addresses(); !slices.Equal(got, want) {
		return fmt.Errorf("expected a targeted plan covering %v, got %v", want, got)
	}
	return nil
}

// PlanDestroyReportsDeletions asserts -destroy plans the teardown of what the
// supplied state tracks, rather than the creation of what the configuration
// declares.
func (t *Tests) PlanDestroyReportsDeletions(ctx context.Context) error {
	state, err := applyBasic(ctx)
	if err != nil {
		return err
	}
	plan, err := pin(ctx, opentofu().
		Config(fixture("basic")).
		WithState(state).
		Plan(dagger.OpentofuConfigPlanOpts{Destroy: true}))
	if err != nil {
		return fmt.Errorf("Plan(destroy): %w", err)
	}
	decoded, err := decodePlan(ctx, plan)
	if err != nil {
		return err
	}
	if len(decoded.ResourceChanges) == 0 {
		return fmt.Errorf("expected a destroy plan to cover the applied resources, got none")
	}
	for _, rc := range decoded.ResourceChanges {
		if !slices.Equal(rc.Change.Actions, []string{"delete"}) {
			return fmt.Errorf("expected %s to be deleted, got actions %v", rc.Address, rc.Change.Actions)
		}
	}
	return nil
}

// PlanAgainstAppliedStateReportsNoChanges asserts the state Apply emits is
// re-consumable: feeding it back to a fresh Config leaves nothing to do.
//
// This is the round trip that makes file-carried state usable at all — the
// caller persists terraform.tfstate between runs, and tofu has to recognise
// its own work in it.
func (t *Tests) PlanAgainstAppliedStateReportsNoChanges(ctx context.Context) error {
	state, err := applyBasic(ctx)
	if err != nil {
		return err
	}
	plan, err := pin(ctx, opentofu().
		Config(fixture("basic")).
		WithState(state).
		Plan())
	if err != nil {
		return fmt.Errorf("Plan against applied state: %w", err)
	}
	changes, err := plan.File(changesName).Contents(ctx)
	if err != nil {
		return fmt.Errorf("read %s: %w", changesName, err)
	}
	if changes != "none" {
		text, _ := plan.File(planTextName).Contents(ctx)
		return fmt.Errorf("expected changes=%q against applied state, got %q:\n%s", "none", changes, text)
	}
	return nil
}

// ------------------------------------------------------------------ apply

// ApplyProducesStateAndOutputs asserts an apply in file-carried mode returns
// the three artifacts it documents, that the state tracks both resources, and
// that the output values are the ones the configuration declares.
func (t *Tests) ApplyProducesStateAndOutputs(ctx context.Context) error {
	result, err := pin(ctx, opentofu().Config(fixture("basic")).Apply())
	if err != nil {
		return fmt.Errorf("Apply: %w", err)
	}

	entries, err := result.Entries(ctx)
	if err != nil {
		return fmt.Errorf("read the apply directory: %w", err)
	}
	for _, want := range []string{stateFileName, outputsName, applyLogName} {
		if !slices.Contains(entries, want) {
			return fmt.Errorf("expected %s in the apply directory, got %v", want, entries)
		}
	}

	state, err := decodeState(ctx, result.File(stateFileName))
	if err != nil {
		return err
	}
	want := []string{"random_integer.port", "random_pet.name"}
	if got := state.addresses(); !slices.Equal(got, want) {
		return fmt.Errorf("expected state tracking %v, got %v", want, got)
	}

	outputs, err := decodeOutputs(ctx, result.File(outputsName))
	if err != nil {
		return err
	}
	name, ok := outputs["name"]
	if !ok {
		return fmt.Errorf("expected a name output, got %v", outputs)
	}
	if !strings.HasPrefix(fmt.Sprint(name.Value), "devex-") {
		return fmt.Errorf("expected the name output to carry the default prefix, got %v", name.Value)
	}
	if _, ok := outputs["port"]; !ok {
		return fmt.Errorf("expected a port output, got %v", outputs)
	}

	log, err := result.File(applyLogName).Contents(ctx)
	if err != nil {
		return fmt.Errorf("read %s: %w", applyLogName, err)
	}
	if !strings.Contains(log, "Apply complete!") {
		return fmt.Errorf("expected the apply log to record completion, got:\n%s", log)
	}
	return nil
}

// ApplyConsumesSavedPlan asserts the plan.tfplan Plan emits is what Apply
// takes, closing the two-step plan-then-apply loop a review gate needs.
func (t *Tests) ApplyConsumesSavedPlan(ctx context.Context) error {
	plan, err := pin(ctx, opentofu().Config(fixture("basic")).Plan())
	if err != nil {
		return fmt.Errorf("Plan: %w", err)
	}
	result, err := pin(ctx, opentofu().
		Config(fixture("basic")).
		Apply(dagger.OpentofuConfigApplyOpts{Plan: plan.File(planFileName)}))
	if err != nil {
		return fmt.Errorf("Apply with a saved plan: %w", err)
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

// ApplyFailsOnProviderError asserts a failed apply is an error carrying
// tofu's own diagnostic, not a directory the caller has to remember to
// inspect.
func (t *Tests) ApplyFailsOnProviderError(ctx context.Context) error {
	_, err := opentofu().Config(fixture("apply-fails")).Apply().Sync(ctx)
	return expectErrorContains(err, "tofu apply failed", "local_file.unwritable")
}

// ----------------------------------------------------------------- destroy

// DestroyEmptiesState asserts a destroy against file-carried state tears down
// everything the state tracked and hands back the emptied state.
func (t *Tests) DestroyEmptiesState(ctx context.Context) error {
	state, err := applyBasic(ctx)
	if err != nil {
		return err
	}
	result, err := pin(ctx, opentofu().
		Config(fixture("basic")).
		WithState(state).
		Destroy())
	if err != nil {
		return fmt.Errorf("Destroy: %w", err)
	}
	after, err := decodeState(ctx, result.File(stateFileName))
	if err != nil {
		return err
	}
	if got := after.addresses(); len(got) != 0 {
		return fmt.Errorf("expected an emptied state, still tracking %v", got)
	}
	log, err := result.File("destroy.log").Contents(ctx)
	if err != nil {
		return fmt.Errorf("read destroy.log: %w", err)
	}
	if !strings.Contains(log, "Destroy complete!") {
		return fmt.Errorf("expected the destroy log to record completion, got:\n%s", log)
	}
	return nil
}

// ------------------------------------------------------------ outputs/show

// OutputsReturnsJson asserts Outputs reads the output values out of the
// supplied state.
func (t *Tests) OutputsReturnsJson(ctx context.Context) error {
	state, err := applyBasic(ctx)
	if err != nil {
		return err
	}
	raw, err := opentofu().Config(fixture("basic")).WithState(state).Outputs(ctx)
	if err != nil {
		return fmt.Errorf("Outputs: %w", err)
	}
	var outputs map[string]outputValue
	if err := json.Unmarshal([]byte(raw), &outputs); err != nil {
		return fmt.Errorf("parse the outputs as JSON (%q): %w", raw, err)
	}
	for _, want := range []string{"name", "port"} {
		if _, ok := outputs[want]; !ok {
			return fmt.Errorf("expected a %s output, got %v", want, outputs)
		}
	}
	return nil
}

// ShowRendersState asserts Show renders the supplied state in tofu's
// human-readable form.
func (t *Tests) ShowRendersState(ctx context.Context) error {
	state, err := applyBasic(ctx)
	if err != nil {
		return err
	}
	out, err := opentofu().Config(fixture("basic")).WithState(state).Show(ctx)
	if err != nil {
		return fmt.Errorf("Show: %w", err)
	}
	for _, want := range []string{"random_pet", "random_integer"} {
		if !strings.Contains(out, want) {
			return fmt.Errorf("expected the rendered state to mention %s, got:\n%s", want, out)
		}
	}
	return nil
}

// ------------------------------------------------------------------ helpers

// applyBasic applies the basic fixture from an empty state and returns the
// state it produced, for the tests that need something to read, re-plan or
// tear down.
func applyBasic(ctx context.Context) (*dagger.File, error) {
	result, err := pin(ctx, opentofu().Config(fixture("basic")).Apply())
	if err != nil {
		return nil, fmt.Errorf("Apply the basic fixture: %w", err)
	}
	return result.File(stateFileName), nil
}

// lockHashes returns the provider hashes recorded in a lock file — the `h1:`
// and `zh:` entries, without the quoting and punctuation the HCL list wraps
// them in. It is a scan, not a parse: the assertions only compare hash sets.
func lockHashes(ctx context.Context, dir *dagger.Directory) ([]string, error) {
	raw, err := dir.File(lockFileName).Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", lockFileName, err)
	}
	var hashes []string
	for line := range strings.SplitSeq(raw, "\n") {
		line = strings.Trim(strings.TrimSpace(line), `",`)
		if strings.HasPrefix(line, "h1:") || strings.HasPrefix(line, "zh:") {
			hashes = append(hashes, line)
		}
	}
	return hashes, nil
}

// planDocument is the slice of `tofu show -json <plan>` the assertions read.
type planDocument struct {
	FormatVersion string `json:"format_version"`
	Timestamp     string `json:"timestamp"`
	Variables     map[string]struct {
		Value any `json:"value"`
	} `json:"variables"`
	ResourceChanges []struct {
		Address string `json:"address"`
		Change  struct {
			Actions []string `json:"actions"`
		} `json:"change"`
	} `json:"resource_changes"`
}

// actions maps each address the plan covers to what it proposes doing to it.
//
// Every resource in the configuration appears, including the ones the plan
// leaves alone — those carry `no-op` — so an assertion about one resource has
// to name the rest rather than expect them to be absent.
func (p planDocument) actions() map[string][]string {
	out := make(map[string][]string, len(p.ResourceChanges))
	for _, rc := range p.ResourceChanges {
		out[rc.Address] = rc.Change.Actions
	}
	return out
}

// addresses returns the planned resource addresses, sorted so assertions do
// not depend on tofu's ordering.
func (p planDocument) addresses() []string {
	out := make([]string, 0, len(p.ResourceChanges))
	for _, rc := range p.ResourceChanges {
		out = append(out, rc.Address)
	}
	slices.Sort(out)
	return out
}

// stateDocument is the slice of a tofu state file the assertions read.
type stateDocument struct {
	Version   int `json:"version"`
	Resources []struct {
		Mode string `json:"mode"`
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"resources"`
}

func (s stateDocument) addresses() []string {
	out := make([]string, 0, len(s.Resources))
	for _, r := range s.Resources {
		out = append(out, r.Type+"."+r.Name)
	}
	slices.Sort(out)
	return out
}

type outputValue struct {
	Value     any  `json:"value"`
	Sensitive bool `json:"sensitive"`
}

func decodePlan(ctx context.Context, plan *dagger.Directory) (planDocument, error) {
	var doc planDocument
	raw, err := plan.File(planJSONName).Contents(ctx)
	if err != nil {
		return doc, fmt.Errorf("read %s: %w", planJSONName, err)
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return doc, fmt.Errorf("parse %s: %w", planJSONName, err)
	}
	return doc, nil
}

func decodeState(ctx context.Context, state *dagger.File) (stateDocument, error) {
	raw, err := state.Contents(ctx)
	if err != nil {
		return stateDocument{}, fmt.Errorf("read %s: %w", stateFileName, err)
	}
	return parseState(raw)
}

// parseState decodes a state document that was read as text — from a file
// here, from an object in a remote backend in the backend tests.
func parseState(raw string) (stateDocument, error) {
	var doc stateDocument
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return doc, fmt.Errorf("parse %s: %w", stateFileName, err)
	}
	return doc, nil
}

func decodeOutputs(ctx context.Context, file *dagger.File) (map[string]outputValue, error) {
	raw, err := file.Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", outputsName, err)
	}
	var outputs map[string]outputValue
	if err := json.Unmarshal([]byte(raw), &outputs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", outputsName, err)
	}
	return outputs, nil
}
