package main

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"dagger/tests/internal/dagger"
)

// The tests here bind fixtures/nested, a tree whose root module in roots/app
// uses ../../modules/pet — a module outside the root module's own directory.

const (
	nestedRoot      = "roots/app"
	nestedPetModule = "module.pet.random_pet.name"
)

// nested binds the nested fixture with roots/app as the root module.
func nested() *dagger.OpentofuConfig {
	return opentofu().Config(fixture("nested"), dagger.OpentofuConfigOpts{Path: nestedRoot})
}

// PathValidatesRootModuleOutsideModules asserts a root module reaching a module
// outside its own directory validates once the whole tree is bound.
func (t *Tests) PathValidatesRootModuleOutsideModules(ctx context.Context) error {
	if err := nested().Validate(ctx); err != nil {
		return fmt.Errorf("Validate with path %q: %w", nestedRoot, err)
	}
	return nil
}

// PathPlansRootModuleOutsideModules asserts Plan runs in the root module and
// hands back plan files describing the module it uses.
func (t *Tests) PathPlansRootModuleOutsideModules(ctx context.Context) error {
	plan, err := pin(ctx, nested().Plan())
	if err != nil {
		return fmt.Errorf("Plan with path %q: %w", nestedRoot, err)
	}
	entries, err := plan.Entries(ctx)
	if err != nil {
		return fmt.Errorf("read plan directory: %w", err)
	}
	for _, name := range []string{planFileName, planJSONName, planTextName, changesName} {
		if !slices.Contains(entries, name) {
			return fmt.Errorf("expected %s in the plan directory, got %v", name, entries)
		}
	}
	changes, err := plan.File(changesName).Contents(ctx)
	if err != nil {
		return fmt.Errorf("read %s: %w", changesName, err)
	}
	if strings.TrimSpace(changes) != "changes" {
		return fmt.Errorf("expected changes against an empty state, got %q", changes)
	}
	doc, err := decodePlan(ctx, plan)
	if err != nil {
		return err
	}
	if !slices.Contains(doc.addresses(), nestedPetModule) {
		return fmt.Errorf("expected the plan to create %s, got %v", nestedPetModule, doc.addresses())
	}
	return nil
}

// PathAppliesAndDestroysRootModuleOutsideModules asserts the file-carried state
// round trip works from a root module below source: Apply hands back the state
// tofu wrote in the root module, and Destroy reads it back from WithState.
func (t *Tests) PathAppliesAndDestroysRootModuleOutsideModules(ctx context.Context) error {
	applied, err := pin(ctx, nested().Apply())
	if err != nil {
		return fmt.Errorf("Apply with path %q: %w", nestedRoot, err)
	}
	state, err := decodeState(ctx, applied.File(stateFileName))
	if err != nil {
		return err
	}
	if !slices.Contains(state.addresses(), nestedPetModule) {
		return fmt.Errorf("expected the applied state to track %s, got %v", nestedPetModule, state.addresses())
	}
	outputs, err := decodeOutputs(ctx, applied.File(outputsName))
	if err != nil {
		return err
	}
	if name, _ := outputs["name"].Value.(string); !strings.HasPrefix(name, "nested-") {
		return fmt.Errorf("expected the name output to come from the pet module, got %v", outputs["name"].Value)
	}

	destroyed, err := pin(ctx, nested().WithState(applied.File(stateFileName)).Destroy())
	if err != nil {
		return fmt.Errorf("Destroy with path %q: %w", nestedRoot, err)
	}
	after, err := decodeState(ctx, destroyed.File(stateFileName))
	if err != nil {
		return err
	}
	if got := after.addresses(); len(got) != 0 {
		return fmt.Errorf("expected an emptied state, still tracking %v", got)
	}
	return nil
}

// PathReturnsRootModuleFromInit asserts Init hands back the root module, not
// the tree around it: the lock file sits at the top of the result, and the
// sibling modules/ directory is not in it.
func (t *Tests) PathReturnsRootModuleFromInit(ctx context.Context) error {
	entries, err := nested().Init().Entries(ctx)
	if err != nil {
		return fmt.Errorf("Init with path %q: %w", nestedRoot, err)
	}
	if !slices.Contains(entries, lockFileName) || !slices.Contains(entries, "main.tf") {
		return fmt.Errorf("expected the root module with %s at its top, got %v", lockFileName, entries)
	}
	for _, e := range entries {
		if e == "modules/" || e == "roots/" {
			return fmt.Errorf("expected only the root module back, got the surrounding tree: %v", entries)
		}
	}
	return nil
}

// RootModuleBoundAloneCannotReachOutsideModules asserts what path exists to
// fix: binding roots/app on its own leaves ../../modules/pet outside what tofu
// was handed, so init cannot find it.
func (t *Tests) RootModuleBoundAloneCannotReachOutsideModules(ctx context.Context) error {
	err := opentofu().Config(fixture("nested/" + nestedRoot)).Validate(ctx)
	return expectErrorContains(err, "tofu init", "Unreadable module directory", `module "pet"`)
}

// PathMissingFromSourceIsRejected asserts a path that is not a directory in
// source — absent, or naming a file — is refused with an error naming it.
func (t *Tests) PathMissingFromSourceIsRejected(ctx context.Context) error {
	for _, p := range []string{"roots/missing", nestedRoot + "/main.tf"} {
		err := opentofu().Config(fixture("nested"), dagger.OpentofuConfigOpts{Path: p}).Validate(ctx)
		if err := expectErrorContains(err, fmt.Sprintf("%q", p), "not a directory in source"); err != nil {
			return fmt.Errorf("path %q: %w", p, err)
		}
	}
	return nil
}

// PathLeavingSourceIsRejected asserts a path climbing out of source is refused
// with an error naming it, however it is spelled, and so is an absolute one.
func (t *Tests) PathLeavingSourceIsRejected(ctx context.Context) error {
	for _, p := range []string{"..", "../basic", "roots/../../basic"} {
		err := opentofu().Config(fixture("nested"), dagger.OpentofuConfigOpts{Path: p}).Validate(ctx)
		if err := expectErrorContains(err, fmt.Sprintf("%q", p), "leaves source"); err != nil {
			return fmt.Errorf("path %q: %w", p, err)
		}
	}
	err := opentofu().Config(fixture("nested"), dagger.OpentofuConfigOpts{Path: "/src"}).Validate(ctx)
	if err := expectErrorContains(err, `"/src"`, "must be relative"); err != nil {
		return fmt.Errorf("path %q: %w", "/src", err)
	}
	return nil
}
