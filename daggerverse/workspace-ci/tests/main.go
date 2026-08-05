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
func (t *Tests) PlanRunsEverythingOnAnUnusableDiffRange(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	zero := strings.Repeat("0", 40)
	got, err := explainRange(ctx, dag.WorkspaceCi(), fx, zero, fx.at(cTouchRoot), "")
	if err != nil {
		return err
	}
	if !got.Full {
		return fmt.Errorf("an unusable diff range did not run everything: %v", names(got.Plan))
	}
	return wantLegs(got, fxRoot, fxGlobal, fxA, fxB, fxC, fxDirty)
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
		"plan-errors-on-workspace-with-no-modules":               t.PlanErrorsOnWorkspaceWithNoModules,
		"plan-drops-known-good-leg":                              t.PlanDropsKnownGoodLeg,
		"plan-refuses-recorded-passes-when-global-input-changed": t.PlanRefusesRecordedPassesWhenGlobalInputChanged,
		"plan-always-runs-unhashable-leg":                        t.PlanAlwaysRunsUnhashableLeg,
		"plan-global-inputs-are-root-dependency-closure":         t.PlanGlobalInputsAreRootDependencyClosure,
		"plan-loads-only-affected-modules":                       t.PlanLoadsOnlyAffectedModules,
		"affected-modules-reports-what-change-reached":           t.AffectedModulesReportsWhatChangeReached,
		"plan-emits-github-actions-matrix":                       t.PlanEmitsGithubActionsMatrix,
		"plan-emits-jenkins-parallel-stages":                     t.PlanEmitsJenkinsParallelStages,
		"plan-applies-timeout-overrides":                         t.PlanAppliesTimeoutOverrides,
		"plan-splits-named-modules-on-the-run-everything-path":   t.PlanSplitsNamedModulesOnTheRunEverythingPath,
		"new-rejects-malformed-timeouts":                         t.NewRejectsMalformedTimeouts,
		"new-rejects-memo-token-without-repo":                    t.NewRejectsMemoTokenWithoutRepo,
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
