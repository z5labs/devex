package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"dagger/tests/internal/dagger"
)

// PlanLoadsOnlyAffectedModules proves the performance property the whole design
// rests on, by counting rather than timing: producing a plan for a narrow change
// loads the affected modules and nothing else.
func (t *Tests) PlanLoadsOnlyAffectedModules(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	got, err := explain(ctx, dag.WorkspaceCi(), fx, cTouchA, "")
	if err != nil {
		return err
	}
	want := []string{fxRoot, fxA, fxB}
	slices.Sort(want)
	loaded := slices.Clone(got.LoadedModules)
	slices.Sort(loaded)
	if !slices.Equal(loaded, want) {
		return fmt.Errorf("loaded %v, want %v", loaded, want)
	}
	for _, unaffected := range []string{fxC, fxDirty, fxGlobal} {
		if slices.Contains(loaded, unaffected) {
			return fmt.Errorf("loaded %q, which the change could not reach", unaffected)
		}
	}
	return nil
}

// AffectedModulesReportsWhatChangeReached proves the attribution half of the plan
// is available on its own, for a caller that wants to know what a change reached
// without paying to enumerate checks.
func (t *Tests) AffectedModulesReportsWhatChangeReached(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	raw, err := dag.WorkspaceCi().AffectedModules(ctx, fx.before(cTouchA), fx.at(cTouchA), dagger.WorkspaceCiAffectedModulesOpts{
		Repo: fx.dir,
	})
	if err != nil {
		return err
	}
	var got []string
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		return fmt.Errorf("parse %q: %w", raw, err)
	}
	want := []string{fxRoot, fxA, fxB}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		return fmt.Errorf("reached %v, want %v", got, want)
	}
	return nil
}

// PlanEmitsGithubActionsMatrix proves the one non-canonical format carries the
// same legs on a single line, which is what a GITHUB_OUTPUT assignment and fromJSON
// need.
func (t *Tests) PlanEmitsGithubActionsMatrix(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	raw, err := dag.WorkspaceCi().Plan(ctx, fx.before(cTouchA), fx.at(cTouchA), dagger.WorkspaceCiPlanOpts{
		Repo:   fx.dir,
		Format: dagger.WorkspaceCiFormatGithubActions,
	})
	if err != nil {
		return err
	}
	if strings.Contains(strings.TrimSpace(raw), "\n") {
		return fmt.Errorf("the github-actions plan spans more than one line:\n%s", raw)
	}
	var legs []leg
	if err := json.Unmarshal([]byte(raw), &legs); err != nil {
		return fmt.Errorf("a github-actions matrix must be a JSON array: %w (%q)", err, raw)
	}
	if len(legs) == 0 {
		return fmt.Errorf("the github-actions matrix is empty: %q", raw)
	}
	for _, l := range legs {
		if l.Name == "" || l.Module == "" {
			return fmt.Errorf("matrix entry %+v is missing the fields a job needs", l)
		}
		if l.JobTimeout != l.Timeout+4 {
			return fmt.Errorf("matrix entry %+v has a job budget that is not its step budget plus headroom", l)
		}
	}
	return nil
}

// PlanAppliesTimeoutOverrides proves the timeout table: an override keyed by a
// leg's name beats one keyed by its module, both beat the default, and the job
// budget always follows the step budget.
func (t *Tests) PlanAppliesTimeoutOverrides(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	ci := dag.WorkspaceCi(dagger.WorkspaceCiOpts{
		Timeouts:       `{"mods/a:ok": 11, "mods/b": 9}`,
		DefaultTimeout: 7,
	})
	got, err := explain(ctx, ci, fx, cTouchA, "")
	if err != nil {
		return err
	}
	for name, want := range map[string]int{"mods/a:ok": 11, "mods/b:ok": 9, ".:root-ok": 7} {
		l, err := find(got, name)
		if err != nil {
			return err
		}
		if l.Timeout != want {
			return fmt.Errorf("leg %q has a step budget of %d minutes, want %d", name, l.Timeout, want)
		}
		if l.JobTimeout != want+4 {
			return fmt.Errorf("leg %q has a job budget of %d minutes, want %d", name, l.JobTimeout, want+4)
		}
	}
	return nil
}

// PlanSplitsNamedModulesOnTheRunEverythingPath proves the escape hatch for a
// module whose checks must not share a leg: named modules are enumerated even when
// everything runs, and every other module still gets one coarse leg and is still
// never loaded.
func (t *Tests) PlanSplitsNamedModulesOnTheRunEverythingPath(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	ci := dag.WorkspaceCi(dagger.WorkspaceCiOpts{SplitModules: []string{fxA}})
	got, err := explain(ctx, ci, fx, cTouchFlow, "")
	if err != nil {
		return err
	}
	if !got.Full {
		return fmt.Errorf("a workflow change did not run everything: %v", names(got.Plan))
	}
	if err := wantLegs(got, fxRoot, fxGlobal, "mods/a:ok", fxB, fxC, fxDirty); err != nil {
		return err
	}
	split, err := find(got, "mods/a:ok")
	if err != nil {
		return err
	}
	if split.Filter != "a:ok" {
		return fmt.Errorf("split leg %+v does not carry its own check pattern", split)
	}
	coarse, err := find(got, fxB)
	if err != nil {
		return err
	}
	if coarse.Filter != "" {
		return fmt.Errorf("leg %q carries the filter %q; only the named modules are split", coarse.Name, coarse.Filter)
	}
	if !slices.Equal(got.LoadedModules, []string{fxA}) {
		return fmt.Errorf("loaded %v; splitting one module must load exactly that module", got.LoadedModules)
	}
	return nil
}

// NewRejectsMalformedTimeouts proves a timeout table that cannot be read is an
// error. A typo'd key already fails quietly — the default applies — so the one
// thing left to catch loudly is a table nothing could be read from.
func (t *Tests) NewRejectsMalformedTimeouts(ctx context.Context) error {
	_, err := dag.WorkspaceCi(dagger.WorkspaceCiOpts{Timeouts: `{"mods/a:ok": `}).
		Plan(ctx, "", "", dagger.WorkspaceCiPlanOpts{Repo: dag.Directory()})
	if err == nil {
		return fmt.Errorf("a malformed timeout table was accepted")
	}
	if !strings.Contains(err.Error(), "parse timeouts") {
		return fmt.Errorf("a malformed timeout table failed for the wrong reason: %v", err)
	}
	return nil
}

// NewRejectsMemoTokenWithoutRepo proves a credential with nothing to scope it is
// an error rather than a store that silently reads nothing — which would look
// exactly like a workspace with no recorded passes.
func (t *Tests) NewRejectsMemoTokenWithoutRepo(ctx context.Context) error {
	token, err := randomSecret()
	if err != nil {
		return err
	}
	_, err = dag.WorkspaceCi(dagger.WorkspaceCiOpts{MemoToken: token}).
		Plan(ctx, "", "", dagger.WorkspaceCiPlanOpts{Repo: dag.Directory()})
	if err == nil {
		return fmt.Errorf("a memo token with no repository was accepted")
	}
	if !strings.Contains(err.Error(), "memoRepo") {
		return fmt.Errorf("a memo token with no repository failed for the wrong reason: %v", err)
	}
	return nil
}

// SelectionSelfTestPasses runs the module's own check the way a consumer's CI
// would, so a regression in the pure selection and hashing rules fails here too
// rather than only where it is installed.
func (t *Tests) SelectionSelfTestPasses(ctx context.Context) error {
	return dag.WorkspaceCi().SelectionSelfTest(ctx)
}

// randomSecret mints a throwaway credential at run time, so no test ever carries
// a literal one.
func randomSecret() (*dagger.Secret, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	return dag.SetSecret("workspace-ci-tests-memo-token", hex.EncodeToString(b[:])), nil
}
