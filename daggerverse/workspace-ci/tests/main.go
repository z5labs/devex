// Tests for the workspace-ci module.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/sync/errgroup"

	"dagger/tests/internal/dagger"
)

type Tests struct{}

// leg is one entry of a plan, as a consumer would parse it.
type leg struct {
	Name       string `json:"name"`
	Module     string `json:"module"`
	Filter     string `json:"filter"`
	Hash       string `json:"hash"`
	Timeout    int    `json:"timeout"`
	JobTimeout int    `json:"jobTimeout"`
}

// report is what Plan emits with diagnostics on.
type report struct {
	Plan            []leg    `json:"plan"`
	AffectedModules []string `json:"affectedModules"`
	LoadedModules   []string `json:"loadedModules"`
	Full            bool     `json:"full"`
	Memoized        []leg    `json:"memoized"`
	MemoTrusted     bool     `json:"memoTrusted"`
	KnownGood       int      `json:"knownGood"`
}

// explain plans the change that the named commit introduced and returns the
// planner's own account of it.
func explain(ctx context.Context, ci *dagger.WorkspaceCi, fx fixture, commit string, knownGood string) (report, error) {
	return explainRange(ctx, ci, fx, fx.before(commit), fx.at(commit), knownGood)
}

func explainRange(ctx context.Context, ci *dagger.WorkspaceCi, fx fixture, base, head, knownGood string) (report, error) {
	raw, err := ci.Plan(ctx, base, head, dagger.WorkspaceCiPlanOpts{
		Repo:        fx.dir,
		Diagnostics: true,
		KnownGood:   knownGood,
	})
	if err != nil {
		return report{}, err
	}
	var out report
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return report{}, fmt.Errorf("parse the plan %q: %w", raw, err)
	}
	return out, nil
}

// names returns a plan's leg names, sorted.
func names(legs []leg) []string {
	out := make([]string, 0, len(legs))
	for _, l := range legs {
		out = append(out, l.Name)
	}
	slices.Sort(out)
	return out
}

// wantLegs asserts a plan's exact leg set.
func wantLegs(got report, want ...string) error {
	slices.Sort(want)
	if have := names(got.Plan); !slices.Equal(have, want) {
		return fmt.Errorf("planned %v, want %v", have, want)
	}
	return nil
}

// find returns the named leg.
func find(got report, name string) (leg, error) {
	for _, l := range got.Plan {
		if l.Name == name {
			return l, nil
		}
	}
	return leg{}, fmt.Errorf("no leg named %q in %v", name, names(got.Plan))
}

// PlanSelectsAffectedModuleChecks proves the core promise: a change to one module
// plans that module's checks and every check that legitimately depends on it, and
// nothing else.
//
// The fixture's b depends on a, so touching a must reach both; c and dirty depend
// on neither and must be absent. The root module is always there — its checks
// answer questions about the workspace as a whole.
func (t *Tests) PlanSelectsAffectedModuleChecks(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	got, err := explain(ctx, dag.WorkspaceCi(), fx, cTouchA, "")
	if err != nil {
		return err
	}
	if got.Full {
		return fmt.Errorf("a change to one module ran everything: %v", names(got.Plan))
	}
	if err := wantLegs(got, ".:root-ok", "mods/a:ok", "mods/b:ok"); err != nil {
		return err
	}
	a, err := find(got, "mods/a:ok")
	if err != nil {
		return err
	}
	if a.Module != fxA || a.Filter != "a:ok" {
		return fmt.Errorf("leg %+v is not routed at its own module with its own check pattern", a)
	}
	return nil
}

// PlanIgnoresPathsInNoSourceContext proves a change to a file no module ships —
// a module's own README, declared out via dagger.json "include" — selects nothing.
// Only the root module's checks, which always run, remain.
func (t *Tests) PlanIgnoresPathsInNoSourceContext(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	got, err := explain(ctx, dag.WorkspaceCi(), fx, cTouchCProse, "")
	if err != nil {
		return err
	}
	if got.Full {
		return fmt.Errorf("a change to prose ran everything: %v", names(got.Plan))
	}
	return wantLegs(got, ".:root-ok")
}

// PlanAttributesDeletedPathsToTheirModule proves a deleted file is attributed to
// its module rather than dropped. No source context can contain a path that no
// longer exists, so a deletion is indistinguishable from a file declared out —
// which is why deletions are attributed to their module instead.
func (t *Tests) PlanAttributesDeletedPathsToTheirModule(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	got, err := explain(ctx, dag.WorkspaceCi(), fx, cDeleteCFile, "")
	if err != nil {
		return err
	}
	if got.Full {
		return fmt.Errorf("a deletion ran everything: %v", names(got.Plan))
	}
	return wantLegs(got, ".:root-ok", "mods/c:ok")
}

// PlanRunsEverythingOnGlobalPathChange proves a change to the paths that govern
// how CI runs at all runs everything — and does it the cheap way: one leg per
// module, with no module loaded to produce the plan.
func (t *Tests) PlanRunsEverythingOnGlobalPathChange(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	got, err := explain(ctx, dag.WorkspaceCi(), fx, cTouchFlow, "")
	if err != nil {
		return err
	}
	if !got.Full {
		return fmt.Errorf("a workflow change did not run everything: %v", names(got.Plan))
	}
	if err := wantLegs(got, fxRoot, fxGlobal, fxA, fxB, fxC, fxDirty); err != nil {
		return err
	}
	for _, l := range got.Plan {
		if l.Filter != "" {
			return fmt.Errorf("leg %q carries the check filter %q; the run-everything path must emit one leg per module", l.Name, l.Filter)
		}
	}
	if len(got.LoadedModules) != 0 {
		return fmt.Errorf("the run-everything path loaded %v; it must load no module at all", got.LoadedModules)
	}
	return nil
}

// PlanRunsEverythingOnAnUnusableDiffRange proves the fail-safe: a base that cannot
// be diffed — a new branch, whose before-SHA GitHub sends as all zeros — runs
// everything rather than nothing.
//
// The all-zeros SHA is a sentinel and not a revision, so it keeps that meaning
// however the other side is spelled: it is rejected before the repository is
// consulted, and a symbolic head cannot turn it into a range worth diffing.
func (t *Tests) PlanRunsEverythingOnAnUnusableDiffRange(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	zero := strings.Repeat("0", 40)
	for _, form := range []struct{ base, head string }{
		{zero, fx.at(cTouchRoot)},
		{zero, fxHeadBranch},
		{zero, "HEAD"},
		{fxBaseBranch, zero},
		{"", fxHeadBranch},
	} {
		got, err := explainRange(ctx, dag.WorkspaceCi(), fx, form.base, form.head, "")
		if err != nil {
			return fmt.Errorf("--base=%q --head=%q: %w", form.base, form.head, err)
		}
		if !got.Full {
			return fmt.Errorf("--base=%q --head=%q did not run everything: %v", form.base, form.head, names(got.Plan))
		}
		if err := wantLegs(got, fxRoot, fxGlobal, fxA, fxB, fxC, fxDirty); err != nil {
			return fmt.Errorf("--base=%q --head=%q: %w", form.base, form.head, err)
		}
	}
	return nil
}

// PlanAcceptsSymbolicRevisions proves a range named the way a person names one —
// a branch, a tag, HEAD-relative, or a mixture of those and a SHA — plans exactly
// what the equivalent pair of SHAs plans. CI passes SHAs because that is what the
// event payload carries; anyone running Plan by hand has `main` and `HEAD`.
//
// The range is the single commit that touches mods/a, so the expected plan is a
// strict subset of the workspace. That matters: a revision that does not resolve
// falls back to running everything, which an assertion against a full plan could
// not tell from success.
func (t *Tests) PlanAcceptsSymbolicRevisions(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	ci := dag.WorkspaceCi()
	bySHA, err := explain(ctx, ci, fx, cTouchA, "")
	if err != nil {
		return err
	}
	if err := wantLegs(bySHA, ".:root-ok", "mods/a:ok", "mods/b:ok"); err != nil {
		return fmt.Errorf("by SHA: %w", err)
	}

	for _, form := range []struct{ base, head string }{
		{fxBaseBranch, fxHeadBranch},
		{fxBaseTag, fxHeadBranch},
		{fx.rev(cInitial), fx.rev(cTouchA)},
		{fxBaseBranch, fx.at(cTouchA)},
		{fx.at(cInitial), fxHeadBranch},
	} {
		got, err := explainRange(ctx, ci, fx, form.base, form.head, "")
		if err != nil {
			return fmt.Errorf("--base=%s --head=%s: %w", form.base, form.head, err)
		}
		if got.Full {
			return fmt.Errorf("--base=%s --head=%s ran everything, so the revisions did not resolve", form.base, form.head)
		}
		if have, want := names(got.Plan), names(bySHA.Plan); !slices.Equal(have, want) {
			return fmt.Errorf("--base=%s --head=%s planned %v, want the SHA range's %v", form.base, form.head, have, want)
		}
	}

	// The literal `HEAD` the issue's example uses. Its range is the commit that
	// touches the root module, which legitimately runs everything, so this asserts
	// agreement with the SHA form rather than a subset — the cases above are what
	// prove resolution happened at all.
	head, err := explainRange(ctx, ci, fx, fx.rev(cTouchFlow), fx.rev(cTouchRoot), "")
	if err != nil {
		return fmt.Errorf("--base=HEAD~1 --head=HEAD: %w", err)
	}
	byTipSHA, err := explain(ctx, ci, fx, cTouchRoot, "")
	if err != nil {
		return err
	}
	if have, want := names(head.Plan), names(byTipSHA.Plan); !slices.Equal(have, want) {
		return fmt.Errorf("--base=HEAD~1 --head=HEAD planned %v, want the SHA range's %v", have, want)
	}
	return nil
}

// PlanRunsEverythingOnAnUnresolvableRevision proves a revision that names no
// commit — a typo, a branch that was deleted, a shallow clone that never fetched
// the base — runs everything rather than erroring or, worse, planning nothing.
//
// It is the same fail-safe an all-zeros base takes, and it is why resolution
// failure is deliberately not promoted to an error: a name the repository cannot
// resolve must cost a run its time, never its coverage.
func (t *Tests) PlanRunsEverythingOnAnUnresolvableRevision(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	for _, form := range []struct{ base, head string }{
		{"no-such-branch", fxHeadBranch},
		{fxBaseBranch, "no-such-branch"},
	} {
		got, err := explainRange(ctx, dag.WorkspaceCi(), fx, form.base, form.head, "")
		if err != nil {
			return fmt.Errorf("--base=%s --head=%s errored instead of running everything: %w", form.base, form.head, err)
		}
		if !got.Full {
			return fmt.Errorf("--base=%s --head=%s did not run everything: %v", form.base, form.head, names(got.Plan))
		}
		if err := wantLegs(got, fxRoot, fxGlobal, fxA, fxB, fxC, fxDirty); err != nil {
			return fmt.Errorf("--base=%s --head=%s: %w", form.base, form.head, err)
		}
	}
	return nil
}

// PlanErrorsOnWorkspaceWithNoModules proves a workspace it cannot read is an
// error rather than an empty plan. An empty matrix skips the run job and passes the
// gate having run nothing, which is the one failure mode worth failing closed for.
func (t *Tests) PlanErrorsOnWorkspaceWithNoModules(ctx context.Context) error {
	empty := dag.Directory().WithNewFile("README.md", "no modules here\n")
	_, err := dag.WorkspaceCi().Plan(ctx, "", "", dagger.WorkspaceCiPlanOpts{Repo: empty})
	if err == nil {
		return fmt.Errorf("a workspace with no modules produced a plan")
	}
	if !strings.Contains(err.Error(), "no dagger.json") {
		return fmt.Errorf("a workspace with no modules failed for the wrong reason: %v", err)
	}
	return nil
}

// All runs every test.
//
// The concurrency cap keeps a burst of fixture uploads and module loads from
// crowding out the engine; the tests themselves are independent.
//
// +check
func (t *Tests) All(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(6)
	for name, run := range map[string]func(context.Context) error{
		"plan-selects-affected-module-checks":                    t.PlanSelectsAffectedModuleChecks,
		"plan-ignores-paths-in-no-source-context":                t.PlanIgnoresPathsInNoSourceContext,
		"plan-attributes-deleted-paths-to-their-module":          t.PlanAttributesDeletedPathsToTheirModule,
		"plan-runs-everything-on-global-path-change":             t.PlanRunsEverythingOnGlobalPathChange,
		"plan-runs-everything-on-an-unusable-diff-range":         t.PlanRunsEverythingOnAnUnusableDiffRange,
		"plan-accepts-symbolic-revisions":                        t.PlanAcceptsSymbolicRevisions,
		"plan-runs-everything-on-an-unresolvable-revision":       t.PlanRunsEverythingOnAnUnresolvableRevision,
		"plan-errors-on-workspace-with-no-modules":               t.PlanErrorsOnWorkspaceWithNoModules,
		"plan-drops-known-good-leg":                              t.PlanDropsKnownGoodLeg,
		"plan-refuses-recorded-passes-when-global-input-changed": t.PlanRefusesRecordedPassesWhenGlobalInputChanged,
		"plan-always-runs-unhashable-leg":                        t.PlanAlwaysRunsUnhashableLeg,
		"plan-global-inputs-are-root-dependency-closure":         t.PlanGlobalInputsAreRootDependencyClosure,
		"plan-loads-only-affected-modules":                       t.PlanLoadsOnlyAffectedModules,
		"affected-modules-reports-what-change-reached":           t.AffectedModulesReportsWhatChangeReached,
		"plan-emits-github-actions-matrix":                       t.PlanEmitsGithubActionsMatrix,
		"plan-emits-jenkins-parallel-stages":                     t.PlanEmitsJenkinsParallelStages,
		"plan-records-passes-from-jenkins-branches":              t.PlanRecordsPassesFromJenkinsBranches,
		"plan-refuses-record-command-for-data-formats":           t.PlanRefusesRecordCommandForDataFormats,
		"plan-applies-timeout-overrides":                         t.PlanAppliesTimeoutOverrides,
		"plan-splits-named-modules-on-the-run-everything-path":   t.PlanSplitsNamedModulesOnTheRunEverythingPath,
		"new-rejects-malformed-timeouts":                         t.NewRejectsMalformedTimeouts,
		"new-rejects-memo-token-without-repo":                    t.NewRejectsMemoTokenWithoutRepo,
		"new-rejects-an-unknown-memo-store":                      t.NewRejectsAnUnknownMemoStore,
		"record-pass-refuses-an-untrusted-ref":                   t.RecordPassRefusesAnUntrustedRef,
		"record-pass-reaches-the-store-from-trusted-ref":         t.RecordPassReachesTheStoreFromTrustedRef,
		"record-pass-never-fails-the-passing-check":              t.RecordPassNeverFailsThePassingCheck,
		"record-pass-skips-an-unhashable-leg":                    t.RecordPassSkipsAnUnhashableLeg,
		"record-pass-says-the-actions-cache-is-unwritable":       t.RecordPassSaysTheActionsCacheIsUnwritable,
		"record-pass-skips-with-no-store-configured":             t.RecordPassSkipsWithNoStoreConfigured,
		"record-pass-needs-the-runs-ref":                         t.RecordPassNeedsTheRunsRef,
		"memo-store-self-test-passes":                            t.MemoStoreSelfTestPasses,
		"selection-self-test-passes":                             t.SelectionSelfTestPasses,
	} {
		g.Go(func() error {
			if err := run(ctx); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			return nil
		})
	}
	return g.Wait()
}
