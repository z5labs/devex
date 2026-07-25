// Package main implements the test module for the opentofu Dagger module.
// Each test is exposed as a standalone dagger function so it can be invoked
// individually during TDD; All wires them up for parallel execution under
// `dagger call all`.
//
// The fixtures under fixtures/ are hermetic: they use hashicorp/random and
// hashicorp/local only, so nothing needs a cloud credential. The random
// provider's resources exist purely in state, which is what makes the
// state round-trip assertions meaningful — there is no out-of-band object to
// drift away underneath them.
package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/tests/internal/dagger"

	par "github.com/dagger/dagger/util/parallel"
)

type Tests struct{}

// All runs every opentofu-module test in parallel.
//
// parallel caps how many tests run concurrently inside this suite. Defaults to
// 0 (unbounded fan-out) — each `dagger check` job runs on its own GH Actions
// runner, so in-runner parallelism is bounded by the VM's CPU/memory, not by
// the scheduler. Pass any positive integer to opt into a specific cap.
//
// +check
// +cache="session"
func (t *Tests) All(
	ctx context.Context,
	// +default=0
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}

	jobs = jobs.WithJob("ContainerHasTofu", t.ContainerHasTofu)
	jobs = jobs.WithJob("ContainerHasGitAndCaCertificates", t.ContainerHasGitAndCaCertificates)
	jobs = jobs.WithJob("VersionReportsRelease", t.VersionReportsRelease)
	jobs = jobs.WithJob("VersionAcceptsMinimalSuffix", t.VersionAcceptsMinimalSuffix)

	jobs = jobs.WithJob("StateAndBackendConfigAreRejected", t.StateAndBackendConfigAreRejected)
	jobs = jobs.WithJob("StateAndBackendConfigFileAreRejected", t.StateAndBackendConfigFileAreRejected)
	jobs = jobs.WithJob("DestroyWithoutStateOrBackendIsRejected", t.DestroyWithoutStateOrBackendIsRejected)
	jobs = jobs.WithJob("WithVarRejectsNameContainingEquals", t.WithVarRejectsNameContainingEquals)
	jobs = jobs.WithJob("ApplyRejectsTargetsWithSavedPlan", t.ApplyRejectsTargetsWithSavedPlan)

	jobs = jobs.WithJob("FmtAcceptsFormattedConfiguration", t.FmtAcceptsFormattedConfiguration)
	jobs = jobs.WithJob("FmtReportsUnformattedConfiguration", t.FmtReportsUnformattedConfiguration)

	jobs = jobs.WithJob("ValidateAcceptsValidConfiguration", t.ValidateAcceptsValidConfiguration)
	jobs = jobs.WithJob("ValidateRejectsInvalidConfiguration", t.ValidateRejectsInvalidConfiguration)
	jobs = jobs.WithJob("ValidateWorksWithoutBackendCredentials", t.ValidateWorksWithoutBackendCredentials)

	jobs = jobs.WithJob("InitProducesLockFile", t.InitProducesLockFile)
	jobs = jobs.WithJob("PlanReportsChanges", t.PlanReportsChanges)
	jobs = jobs.WithJob("PlanTargetsLimitScope", t.PlanTargetsLimitScope)
	jobs = jobs.WithJob("PlanDestroyReportsDeletions", t.PlanDestroyReportsDeletions)
	jobs = jobs.WithJob("PlanAgainstAppliedStateReportsNoChanges", t.PlanAgainstAppliedStateReportsNoChanges)

	jobs = jobs.WithJob("ApplyProducesStateAndOutputs", t.ApplyProducesStateAndOutputs)
	jobs = jobs.WithJob("ApplyConsumesSavedPlan", t.ApplyConsumesSavedPlan)
	jobs = jobs.WithJob("ApplyFailsOnProviderError", t.ApplyFailsOnProviderError)
	jobs = jobs.WithJob("DestroyEmptiesState", t.DestroyEmptiesState)
	jobs = jobs.WithJob("OutputsReturnsJson", t.OutputsReturnsJson)
	jobs = jobs.WithJob("ShowRendersState", t.ShowRendersState)

	jobs = jobs.WithJob("WithVarOverridesDefault", t.WithVarOverridesDefault)
	jobs = jobs.WithJob("WithVarFileSuppliesVariables", t.WithVarFileSuppliesVariables)
	jobs = jobs.WithJob("WithEnvVariableIsVisibleToTofu", t.WithEnvVariableIsVisibleToTofu)
	jobs = jobs.WithJob("SecretVarReachesTofuWithoutLeaking", t.SecretVarReachesTofuWithoutLeaking)
	jobs = jobs.WithJob("SecretVariableBindsEnvironmentSecret", t.SecretVariableBindsEnvironmentSecret)

	jobs = jobs.WithJob("WithWorkspaceIsolatesState", t.WithWorkspaceIsolatesState)
	jobs = jobs.WithJob("WithoutPluginCacheStillApplies", t.WithoutPluginCacheStillApplies)

	jobs = jobs.WithJob("PlanShouldNotBeCached", t.PlanShouldNotBeCached)
	jobs = jobs.WithJob("ApplyShouldNotBeCached", t.ApplyShouldNotBeCached)

	return jobs.Run(ctx)
}

// ---------------------------------------------------------------- toolchain

// ContainerHasTofu asserts the assembled container exposes the tofu binary on
// PATH, so the escape hatch documented on Container() actually works.
func (t *Tests) ContainerHasTofu(ctx context.Context) error {
	out, err := dag.Opentofu().Container().
		WithExec([]string{"which", "tofu"}).
		Stdout(ctx)
	if err != nil {
		return fmt.Errorf("which tofu: %w", err)
	}
	if !strings.Contains(out, "/tofu") {
		return fmt.Errorf("expected a tofu path, got %q", out)
	}
	return nil
}

// ContainerHasGitAndCaCertificates asserts the two things the -minimal image
// omits and every non-trivial configuration needs: git, for module sources,
// and a CA bundle, for the provider registry.
func (t *Tests) ContainerHasGitAndCaCertificates(ctx context.Context) error {
	ctr := dag.Opentofu().Container()
	if _, err := ctr.WithExec([]string{"git", "--version"}).Stdout(ctx); err != nil {
		return fmt.Errorf("git --version: %w", err)
	}
	if _, err := ctr.WithExec([]string{"test", "-s", "/etc/ssl/certs/ca-certificates.crt"}).Sync(ctx); err != nil {
		return fmt.Errorf("CA bundle missing from the assembled container: %w", err)
	}
	return nil
}

// VersionReportsRelease asserts Version reports the release New was asked for.
func (t *Tests) VersionReportsRelease(ctx context.Context) error {
	out, err := dag.Opentofu(dagger.OpentofuOpts{Version: pinnedVersion}).Version(ctx)
	if err != nil {
		return fmt.Errorf("Version: %w", err)
	}
	if !strings.Contains(out, pinnedVersion) {
		return fmt.Errorf("expected version %q, got %q", pinnedVersion, out)
	}
	return nil
}

// VersionAcceptsMinimalSuffix asserts a caller who spells out the -minimal
// suffix lands on the same image as one who does not — the suffix is appended
// only when absent.
func (t *Tests) VersionAcceptsMinimalSuffix(ctx context.Context) error {
	bare, err := dag.Opentofu(dagger.OpentofuOpts{Version: pinnedVersion}).Version(ctx)
	if err != nil {
		return fmt.Errorf("Version(%q): %w", pinnedVersion, err)
	}
	suffixed, err := dag.Opentofu(dagger.OpentofuOpts{Version: pinnedVersion + "-minimal"}).Version(ctx)
	if err != nil {
		return fmt.Errorf("Version(%q): %w", pinnedVersion+"-minimal", err)
	}
	if bare != suffixed {
		return fmt.Errorf("expected %q and %q to select the same image, got %q and %q",
			pinnedVersion, pinnedVersion+"-minimal", bare, suffixed)
	}
	return nil
}

// ------------------------------------------------------------------- helpers

const (
	// pinnedVersion is the OpenTofu release the version assertions expect. It
	// tracks the module's own default.
	pinnedVersion = "1.12.5"

	// planFileName, stateFileName and the artifact names below mirror what the
	// module documents it emits. Naming them here keeps a rename honest: a
	// module that quietly changed one of them would fail every test that reads
	// the artifact, not just the one that asserted on the name.
	planFileName  = "plan.tfplan"
	stateFileName = "terraform.tfstate"
	planJSONName  = "plan.json"
	planTextName  = "plan.txt"
	changesName   = "changes"
	outputsName   = "outputs.json"
	applyLogName  = "apply.log"
)

// opentofu constructs the module under test. Everything pins the same release
// so an image bump is a one-line change here rather than a sweep.
func opentofu() *dagger.Opentofu {
	return dag.Opentofu(dagger.OpentofuOpts{Version: pinnedVersion})
}

// fixture returns the named hand-authored root module under fixtures/.
func fixture(name string) *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/" + name)
}

// newFile stages an ad-hoc file for the tests that need one input file
// without a fixture directory behind it.
func newFile(name string, contents string) *dagger.File {
	return dag.Directory().WithNewFile(name, contents).File(name)
}

// emptyState is a syntactically valid, resource-free tofu state — enough for
// the state-mode rejection tests, which never get as far as reading it.
func emptyState() *dagger.File {
	return newFile(stateFileName, `{"version":4,"terraform_version":"`+pinnedVersion+`","serial":1,"lineage":"","outputs":{},"resources":[]}`)
}

// pin resolves a lazily-returned directory once and hands back a handle to
// that exact result.
//
// Every stateful function on Config is +cache="never", so each selection off
// the returned directory would otherwise re-invoke it — two reads of one
// Apply's output would be two different applies, with two different random
// values inside them. Resolving to an ID first pins the result.
func pin(ctx context.Context, dir *dagger.Directory) (*dagger.Directory, error) {
	id, err := dir.ID(ctx)
	if err != nil {
		return nil, err
	}
	return dag.LoadDirectoryFromID(dagger.DirectoryID(id)), nil
}

// expectErrorContains asserts a call failed and that its message carries every
// listed fragment. Module errors lose their %w wrapping at the Dagger
// boundary, so the fragments are matched against the flattened text.
func expectErrorContains(err error, fragments ...string) error {
	if err == nil {
		return fmt.Errorf("expected an error mentioning %s, got none", strings.Join(quoteAll(fragments), " and "))
	}
	for _, fragment := range fragments {
		if !strings.Contains(err.Error(), fragment) {
			return fmt.Errorf("expected the error to mention %q, got: %v", fragment, err)
		}
	}
	return nil
}

func quoteAll(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = fmt.Sprintf("%q", v)
	}
	return out
}
