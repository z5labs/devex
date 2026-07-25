package main

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"dagger/tests/internal/dagger"
)

// ------------------------------------------------------------------- check

// CiCheckPassesOnCleanConfiguration asserts a pipeline whose every stage
// succeeds reports success. It is the positive half of the false-green pair:
// without it, a Check that failed unconditionally would satisfy every
// assertion below.
func (t *Tests) CiCheckPassesOnCleanConfiguration(ctx context.Context) error {
	err := opentofu().
		Config(fixture("basic")).
		Ci().
		WithFmt().
		WithValidate().
		Check(ctx)
	if err != nil {
		return fmt.Errorf("Ci.Check on a clean configuration: %w", err)
	}
	return nil
}

// CiCheckReportsUnformattedConfiguration asserts the fmt stage gates the
// pipeline: a configuration that validates but is not formatted still fails.
func (t *Tests) CiCheckReportsUnformattedConfiguration(ctx context.Context) error {
	err := opentofu().
		Config(fixture("unformatted")).
		Ci().
		WithFmt().
		WithValidate().
		Check(ctx)
	return expectErrorContains(err, "not formatted", "main.tf")
}

// CiCheckReportsInvalidConfiguration is the false-green regression from #161
// in this module's terms: the invalid fixture is canonically formatted, so the
// fmt stage passes on it. A Check that reported success on the strength of
// that one green stage would call an unusable configuration sound. Enabling
// the validate stage must surface tofu's own diagnostic instead.
func (t *Tests) CiCheckReportsInvalidConfiguration(ctx context.Context) error {
	err := opentofu().
		Config(fixture("invalid")).
		Ci().
		WithFmt().
		WithValidate().
		Check(ctx)
	return expectErrorContains(err, "tofu validate", "random_pet")
}

// CiCheckWithoutValidateSkipsIt is the counterpart to
// CiCheckReportsInvalidConfiguration and pins the opt-in semantics: a stage
// that was never enabled never runs. The invalid fixture is fmt-clean, so an
// fmt-only Check passes on it — the pipeline reports on exactly what the
// caller asked it to check, and nothing else.
func (t *Tests) CiCheckWithoutValidateSkipsIt(ctx context.Context) error {
	if err := opentofu().Config(fixture("invalid")).Ci().WithFmt().Check(ctx); err != nil {
		return fmt.Errorf("expected an fmt-only Check to pass on the fmt-clean invalid fixture, got: %w", err)
	}
	return nil
}

// CiCheckAggregatesStageFailures asserts the stages run in parallel and their
// errors are aggregated rather than short-circuiting on the first. The ci-bad
// fixture is unformatted *and* invalid, so both stages fail; requiring both
// diagnostics in one message proves neither was skipped once the other had
// already gone red.
func (t *Tests) CiCheckAggregatesStageFailures(ctx context.Context) error {
	err := opentofu().
		Config(fixture("ci-bad")).
		Ci().
		WithFmt().
		WithValidate().
		Check(ctx)
	return expectErrorContains(err, "not formatted", "tofu validate")
}

// CiCheckWithoutStagesIsRejected asserts an empty pipeline is an error rather
// than a pass. A Check that inspects nothing and returns nil is a green that
// means nothing at all.
func (t *Tests) CiCheckWithoutStagesIsRejected(ctx context.Context) error {
	err := opentofu().Config(fixture("basic")).Ci().Check(ctx)
	return expectErrorContains(err, "no stages enabled", "WithFmt")
}

// -------------------------------------------------------------------- plan

// CiCheckWithPlanDetectsDrift asserts WithPlan(failOnChanges: true) turns the
// pipeline into a drift detector: planning the basic fixture against an empty
// state has two resources to create, and a non-empty plan fails the check.
func (t *Tests) CiCheckWithPlanDetectsDrift(ctx context.Context) error {
	err := opentofu().
		Config(fixture("basic")).
		Ci().
		WithPlan(dagger.OpentofuCiWithPlanOpts{FailOnChanges: true}).
		Check(ctx)
	return expectErrorContains(err, "pending changes", "random_pet.name")
}

// CiCheckWithPlanPassesOnAppliedState is the other half of the drift gate:
// the same pipeline against the state a previous apply produced has nothing
// left to do, so the check is green.
//
// The two together are what makes the gate meaningful — a drift detector that
// only ever fails is indistinguishable from a broken one.
func (t *Tests) CiCheckWithPlanPassesOnAppliedState(ctx context.Context) error {
	state, err := applyBasic(ctx)
	if err != nil {
		return err
	}
	err = opentofu().
		Config(fixture("basic")).
		WithState(state).
		Ci().
		WithFmt().
		WithValidate().
		WithPlan(dagger.OpentofuCiWithPlanOpts{FailOnChanges: true}).
		Check(ctx)
	if err != nil {
		return fmt.Errorf("Ci.Check with a drift gate against applied state: %w", err)
	}
	return nil
}

// CiCheckWithPlanAllowsChangesByDefault asserts the plan stage without
// failOnChanges gates on the plan *succeeding*, not on it being empty — the
// shape a pull-request gate needs, where pending changes are the whole point
// of the change under review.
func (t *Tests) CiCheckWithPlanAllowsChangesByDefault(ctx context.Context) error {
	err := opentofu().
		Config(fixture("basic")).
		Ci().
		WithFmt().
		WithPlan().
		Check(ctx)
	if err != nil {
		return fmt.Errorf("expected a plan stage without failOnChanges to accept a non-empty plan, got: %w", err)
	}
	return nil
}

// --------------------------------------------------------------------- run

// CiRunProducesPlanArtifacts asserts Run hands back the plan artifacts for
// downstream consumption — the same four files Config.Plan emits — after the
// enabled checks have passed.
func (t *Tests) CiRunProducesPlanArtifacts(ctx context.Context) error {
	result, err := pin(ctx, opentofu().
		Config(fixture("basic")).
		Ci().
		WithFmt().
		WithValidate().
		Run())
	if err != nil {
		return fmt.Errorf("Ci.Run: %w", err)
	}

	entries, err := result.Entries(ctx)
	if err != nil {
		return fmt.Errorf("read the run directory: %w", err)
	}
	for _, want := range []string{planFileName, planJSONName, planTextName, changesName} {
		if !slices.Contains(entries, want) {
			return fmt.Errorf("expected %s in the run directory, got %v", want, entries)
		}
	}

	changes, err := result.File(changesName).Contents(ctx)
	if err != nil {
		return fmt.Errorf("read %s: %w", changesName, err)
	}
	if changes != "changes" {
		return fmt.Errorf("expected changes=%q for a plan against an empty state, got %q", "changes", changes)
	}

	text, err := result.File(planTextName).Contents(ctx)
	if err != nil {
		return fmt.Errorf("read %s: %w", planTextName, err)
	}
	if !strings.Contains(text, "random_pet.name") {
		return fmt.Errorf("expected the rendered plan to mention random_pet.name, got:\n%s", text)
	}
	return nil
}

// CiRunFailsOnFailedCheck asserts a failing stage costs the caller the
// artifacts: Run returns the aggregated error and no directory, so a broken
// configuration cannot hand a plan to whatever consumes one downstream.
func (t *Tests) CiRunFailsOnFailedCheck(ctx context.Context) error {
	_, err := opentofu().
		Config(fixture("ci-bad")).
		Ci().
		WithFmt().
		WithValidate().
		Run().
		Sync(ctx)
	return expectErrorContains(err, "not formatted", "tofu validate")
}
