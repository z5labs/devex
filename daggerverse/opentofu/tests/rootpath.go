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
// outside its own directory validates once the whole tree is bound — and that
// it is the root module path names being validated: roots/broken, beside it,
// fails. Were path ignored, both would validate as the fixture's top
// directory, which holds no configuration at all.
func (t *Tests) PathValidatesRootModuleOutsideModules(ctx context.Context) error {
	if err := nested().Validate(ctx); err != nil {
		return fmt.Errorf("Validate with path %q: %w", nestedRoot, err)
	}
	err := opentofu().Config(fixture("nested"), dagger.OpentofuConfigOpts{Path: "roots/broken"}).Validate(ctx)
	if err := expectErrorContains(err, "tofu validate", "prefix"); err != nil {
		return fmt.Errorf("Validate with path %q: %w", "roots/broken", err)
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
	// An emptied state alone would also follow from tofu never seeing the
	// supplied state at all; the log says it destroyed what that state held.
	return expectDestroyed(ctx, destroyed, 1)
}

// PathFindsStateInRootModule asserts a state file the source already carries
// is looked for in the root module, not at the top of source: Destroy would
// otherwise refuse, finding neither state nor a backend.
func (t *Tests) PathFindsStateInRootModule(ctx context.Context) error {
	applied, err := pin(ctx, nested().Apply())
	if err != nil {
		return fmt.Errorf("Apply with path %q: %w", nestedRoot, err)
	}
	src := fixture("nested").WithFile(nestedRoot+"/"+stateFileName, applied.File(stateFileName))
	destroyed, err := pin(ctx, opentofu().Config(src, dagger.OpentofuConfigOpts{Path: nestedRoot}).Destroy())
	if err != nil {
		return fmt.Errorf("Destroy with the state in %s: %w", nestedRoot, err)
	}
	return expectDestroyed(ctx, destroyed, 1)
}

// PathHonoursBackendDeclaredInRootModule asserts a backend block is looked for
// in the root module: one declared there means state is not handed back as a
// file. Looked for at the top of source, which holds no *.tf, it would be
// missed and the local state returned as if it were file-carried.
func (t *Tests) PathHonoursBackendDeclaredInRootModule(ctx context.Context) error {
	src := fixture("nested").WithNewFile(nestedRoot+"/backend.tf", "terraform {\n  backend \"local\" {}\n}\n")
	applied, err := pin(ctx, opentofu().Config(src, dagger.OpentofuConfigOpts{Path: nestedRoot}).Apply())
	if err != nil {
		return fmt.Errorf("Apply with a backend declared in %s: %w", nestedRoot, err)
	}
	entries, err := applied.Entries(ctx)
	if err != nil {
		return fmt.Errorf("read the apply directory: %w", err)
	}
	if slices.Contains(entries, stateFileName) {
		return fmt.Errorf("expected no %s with a backend declared in the root module, got %v", stateFileName, entries)
	}
	return nil
}

// PathFmtAndFormatCoverOnlyRootModule asserts Fmt checks and Format rewrites
// the root module and nothing around it: an unformatted file in the root
// module is reported and rewritten, one in modules/ is neither, and Format
// hands back the root module's directory alone.
func (t *Tests) PathFmtAndFormatCoverOnlyRootModule(ctx context.Context) error {
	const unformatted = "locals {\n    a   =   1\n}\n"
	outside := fixture("nested").WithNewFile("modules/pet/extra.tf", unformatted)
	if _, err := opentofu().Config(outside, dagger.OpentofuConfigOpts{Path: nestedRoot}).Fmt(ctx); err != nil {
		return fmt.Errorf("expected Fmt to leave modules/ alone, got: %w", err)
	}

	inside := fixture("nested").WithNewFile(nestedRoot+"/extra.tf", unformatted)
	cfg := opentofu().Config(inside, dagger.OpentofuConfigOpts{Path: nestedRoot})
	_, err := cfg.Fmt(ctx)
	if err := expectErrorContains(err, "not formatted", "extra.tf"); err != nil {
		return err
	}
	formatted, err := pin(ctx, cfg.Format())
	if err != nil {
		return fmt.Errorf("Format with path %q: %w", nestedRoot, err)
	}
	if err := expectRootModuleOnly(ctx, formatted, "extra.tf"); err != nil {
		return fmt.Errorf("Format: %w", err)
	}
	after, err := formatted.File("extra.tf").Contents(ctx)
	if err != nil {
		return fmt.Errorf("read the formatted extra.tf: %w", err)
	}
	if after == unformatted {
		return fmt.Errorf("expected Format to rewrite extra.tf, got it back unchanged:\n%s", after)
	}
	return nil
}

// PathReturnsRootModuleFromLock asserts Lock hands back the root module
// carrying the lock file, as Init does.
func (t *Tests) PathReturnsRootModuleFromLock(ctx context.Context) error {
	locked, err := pin(ctx, nested().Lock())
	if err != nil {
		return fmt.Errorf("Lock with path %q: %w", nestedRoot, err)
	}
	if err := expectRootModuleOnly(ctx, locked, lockFileName); err != nil {
		return fmt.Errorf("Lock: %w", err)
	}
	return nil
}

// PathReturnsRootModuleFromInit asserts Init hands back the root module, not
// the tree around it: the lock file sits at the top of the result, and the
// sibling modules/ directory is not in it.
func (t *Tests) PathReturnsRootModuleFromInit(ctx context.Context) error {
	if err := expectRootModuleOnly(ctx, nested().Init(), lockFileName); err != nil {
		return fmt.Errorf("Init with path %q: %w", nestedRoot, err)
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

// PathThroughSymlinkIsRejected asserts a path passing through a symlink is
// refused, whether the link escapes source or stays inside it: inside the
// container it resolves against a different filesystem than it did in source.
func (t *Tests) PathThroughSymlinkIsRejected(ctx context.Context) error {
	src := fixture("nested").
		WithSymlink("../../..", "roots/escape").
		WithSymlink("app", "roots/alias")
	for _, p := range []string{"roots/escape", "roots/alias", "roots/alias/."} {
		err := opentofu().Config(src, dagger.OpentofuConfigOpts{Path: p}).Validate(ctx)
		if err := expectErrorContains(err, fmt.Sprintf("%q", p), "symlink"); err != nil {
			return fmt.Errorf("path %q: %w", p, err)
		}
	}
	return nil
}

// expectRootModuleOnly asserts dir is the nested fixture's root module, with
// the named file at its top, and none of the tree around it.
func expectRootModuleOnly(ctx context.Context, dir *dagger.Directory, want string) error {
	entries, err := dir.Entries(ctx)
	if err != nil {
		return err
	}
	if !slices.Contains(entries, want) || !slices.Contains(entries, "main.tf") {
		return fmt.Errorf("expected the root module with %s at its top, got %v", want, entries)
	}
	for _, e := range entries {
		if e == "modules/" || e == "roots/" {
			return fmt.Errorf("expected only the root module back, got the surrounding tree: %v", entries)
		}
	}
	return nil
}

// expectDestroyed asserts a Destroy's log reports exactly n resources
// destroyed, which is what shows it read the state it was meant to.
func expectDestroyed(ctx context.Context, result *dagger.Directory, n int) error {
	log, err := result.File("destroy.log").Contents(ctx)
	if err != nil {
		return fmt.Errorf("read destroy.log: %w", err)
	}
	if want := fmt.Sprintf("Resources: %d destroyed", n); !strings.Contains(log, want) {
		return fmt.Errorf("expected the destroy log to report %q, got:\n%s", want, log)
	}
	return nil
}
