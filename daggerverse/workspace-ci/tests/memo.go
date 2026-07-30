package main

import (
	"context"
	"encoding/json"
	"fmt"
)

// PlanDropsKnownGoodLeg proves the point of memoization: a leg whose whole input
// closure hashes to a value some earlier run already passed on is dropped, and
// only that leg is.
func (t *Tests) PlanDropsKnownGoodLeg(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	first, err := explain(ctx, dag.WorkspaceCi(), fx, cTouchA, "")
	if err != nil {
		return err
	}
	target, err := find(first, "mods/a:ok")
	if err != nil {
		return err
	}
	if target.Hash == "" {
		return fmt.Errorf("leg %q has no input hash, so nothing could ever be recorded for it", target.Name)
	}

	second, err := explain(ctx, dag.WorkspaceCi(), fx, cTouchA, jsonArray(target.Hash))
	if err != nil {
		return err
	}
	if !second.MemoTrusted {
		return fmt.Errorf("recorded passes were refused for a change that touches one module")
	}
	if err := wantLegs(second, ".:root-ok", "mods/b:ok"); err != nil {
		return err
	}
	if len(second.Memoized) != 1 || second.Memoized[0].Name != target.Name {
		return fmt.Errorf("expected %q to be reported as already passed, got %v", target.Name, names(second.Memoized))
	}
	return nil
}

// PlanRefusesRecordedPassesWhenGlobalInputChanged proves the trust boundary. Pass
// records are written by the same CI run that produced them, so a change that
// could alter the recording machinery must retire every recorded pass — even the
// ones whose hashes still match.
func (t *Tests) PlanRefusesRecordedPassesWhenGlobalInputChanged(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	first, err := explain(ctx, dag.WorkspaceCi(), fx, cTouchFlow, "")
	if err != nil {
		return err
	}
	target, err := find(first, fxA)
	if err != nil {
		return err
	}
	if target.Hash == "" {
		return fmt.Errorf("leg %q has no input hash, so this proves nothing", target.Name)
	}

	second, err := explain(ctx, dag.WorkspaceCi(), fx, cTouchFlow, jsonArray(target.Hash))
	if err != nil {
		return err
	}
	if second.MemoTrusted {
		return fmt.Errorf("recorded passes were honoured after a global input changed")
	}
	if len(second.Memoized) != 0 {
		return fmt.Errorf("a recorded pass retired %v after a global input changed", names(second.Memoized))
	}
	if _, err := find(second, fxA); err != nil {
		return fmt.Errorf("the leg a recorded pass matched was dropped anyway: %w", err)
	}
	return nil
}

// PlanAlwaysRunsUnhashableLeg proves the fail-safe half of memoization: a module
// with an input that has no object id at HEAD — an untracked file, or a dirty
// working tree — is never memoized, because a hash built from what git can see
// would not describe what the check reads.
func (t *Tests) PlanAlwaysRunsUnhashableLeg(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	got, err := explain(ctx, dag.WorkspaceCi(), fx, cTouchFlow, "")
	if err != nil {
		return err
	}
	dirty, err := find(got, fxDirty)
	if err != nil {
		return err
	}
	if dirty.Hash != "" {
		return fmt.Errorf("leg %q was hashed to %q despite an untracked input", dirty.Name, dirty.Hash)
	}
	clean, err := find(got, fxA)
	if err != nil {
		return err
	}
	if clean.Hash == "" {
		return fmt.Errorf("no leg in the plan was hashable, so an empty hash proves nothing")
	}
	// An empty hash must match no recorded pass, however the store is spelled.
	again, err := explain(ctx, dag.WorkspaceCi(), fx, cTouchFlow, jsonArray("", clean.Hash))
	if err != nil {
		return err
	}
	if _, err := find(again, fxDirty); err != nil {
		return fmt.Errorf("an unhashable leg was memoized away: %w", err)
	}
	return nil
}

// PlanGlobalInputsAreRootDependencyClosure proves that the global inputs folded
// into every leg's hash are the root module's whole dependency closure and not
// just its own source context.
//
// The fixture's root module depends on mods/global, which nothing else depends
// on. Changing only that module must move the input hash of an unrelated module's
// leg — otherwise moving the CI engine itself into a dependency (which is exactly
// what adopting this module does) would leave it outside the trust boundary of
// the hashes it computes.
func (t *Tests) PlanGlobalInputsAreRootDependencyClosure(ctx context.Context) error {
	base, err := hashOfLeg(ctx, "", fxC)
	if err != nil {
		return err
	}
	same, err := hashOfLeg(ctx, "", fxC)
	if err != nil {
		return err
	}
	if same != base {
		return fmt.Errorf("the same tree hashed %q to %q then %q", fxC, base, same)
	}
	moved, err := hashOfLeg(ctx, "variant", fxC)
	if err != nil {
		return err
	}
	if moved == base {
		return fmt.Errorf("changing a module the root module depends on left %q hashed at %q", fxC, base)
	}
	return nil
}

// hashOfLeg plans the fixture variant's whole workspace and returns one leg's
// input hash.
func hashOfLeg(ctx context.Context, variant, name string) (string, error) {
	fx, err := newFixture(ctx, variant)
	if err != nil {
		return "", err
	}
	got, err := explain(ctx, dag.WorkspaceCi(), fx, cTouchFlow, "")
	if err != nil {
		return "", err
	}
	found, err := find(got, name)
	if err != nil {
		return "", err
	}
	if found.Hash == "" {
		return "", fmt.Errorf("leg %q has no input hash to compare", name)
	}
	return found.Hash, nil
}

// jsonArray renders hashes the way the knownGood argument expects them.
func jsonArray(hashes ...string) string {
	out, err := json.Marshal(hashes)
	if err != nil {
		return "[]"
	}
	return string(out)
}
