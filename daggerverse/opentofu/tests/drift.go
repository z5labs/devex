package main

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"dagger/tests/internal/dagger"
)

// The refresh-only tests run against the drift fixture, whose local_file is
// the one object in the suite that lives outside state. The file Apply writes
// stays in Apply's container, so each test re-creates it in the source it
// plans against: with managedContent it is the object as applied; with
// anything else it is the object changed outside tofu.
const (
	managedFileName = "managed.txt"
	managedContent  = "managed by tofu\n"
)

// ---------------------------------------------------------- refresh-only plan

// PlanRefreshOnlyAfterApplyReportsNone asserts a refresh-only plan against the
// state an apply just produced, with the object it created untouched, has
// nothing to report.
func (t *Tests) PlanRefreshOnlyAfterApplyReportsNone(ctx context.Context) error {
	state, err := applyDrift(ctx)
	if err != nil {
		return err
	}
	return expectChanges(ctx, "a refresh-only plan right after an apply",
		driftSource(managedContent).WithState(state).
			Plan(dagger.OpentofuConfigPlanOpts{RefreshOnly: true}),
		"none")
}

// PlanRefreshOnlyReportsDriftWithoutChangingState asserts a refresh-only plan
// sees an object changed outside tofu, emits the same four artifacts any plan
// does, and leaves the state it was given as it was — which is what separates
// it from Refresh, whose drift report is the state it already rewrote.
func (t *Tests) PlanRefreshOnlyReportsDriftWithoutChangingState(ctx context.Context) error {
	state, err := applyDrift(ctx)
	if err != nil {
		return err
	}
	before, err := state.Contents(ctx)
	if err != nil {
		return fmt.Errorf("read the applied state: %w", err)
	}

	cfg := driftSource("changed outside tofu\n").WithState(state)
	plan, err := pin(ctx, cfg.Plan(dagger.OpentofuConfigPlanOpts{RefreshOnly: true}))
	if err != nil {
		return fmt.Errorf("Plan(refreshOnly): %w", err)
	}
	entries, err := plan.Entries(ctx)
	if err != nil {
		return fmt.Errorf("read the plan directory: %w", err)
	}
	for _, want := range []string{planFileName, planJSONName, planTextName, changesName} {
		if !slices.Contains(entries, want) {
			return fmt.Errorf("expected %s in the refresh-only plan directory, got %v", want, entries)
		}
	}
	changes, err := plan.File(changesName).Contents(ctx)
	if err != nil {
		return fmt.Errorf("read %s: %w", changesName, err)
	}
	text, err := plan.File(planTextName).Contents(ctx)
	if err != nil {
		return fmt.Errorf("read %s: %w", planTextName, err)
	}
	if changes != "changes" {
		return fmt.Errorf("expected changes=%q after the object changed outside tofu, got %q:\n%s",
			"changes", changes, text)
	}
	if !strings.Contains(text, "local_file.managed") {
		return fmt.Errorf("expected the refresh-only plan to name local_file.managed, got:\n%s", text)
	}

	after, err := state.Contents(ctx)
	if err != nil {
		return fmt.Errorf("re-read the applied state: %w", err)
	}
	if after != before {
		return fmt.Errorf("expected the state to be unchanged by a refresh-only plan")
	}
	// The drift was not written anywhere a later check reads from: planning
	// again against the same state still finds it.
	return expectChanges(ctx, "a second refresh-only plan against the same state",
		cfg.Plan(dagger.OpentofuConfigPlanOpts{RefreshOnly: true}), "changes")
}

// PlanRefreshOnlyIgnoresConfigurationChanges asserts a refresh-only plan
// reports nothing when only the configuration moved, while an ordinary plan
// over the same inputs proposes the change — drift is measured against what
// was applied, not against today's configuration.
func (t *Tests) PlanRefreshOnlyIgnoresConfigurationChanges(ctx context.Context) error {
	state, err := applyDrift(ctx)
	if err != nil {
		return err
	}
	cfg := driftSource(managedContent).
		WithState(state).
		WithVar("content", "a configuration change not yet applied\n")
	if err := expectChanges(ctx, "a refresh-only plan after a configuration change",
		cfg.Plan(dagger.OpentofuConfigPlanOpts{RefreshOnly: true}), "none"); err != nil {
		return err
	}
	return expectChanges(ctx, "an ordinary plan after a configuration change", cfg.Plan(), "changes")
}

// PlanRefreshOnlyWithDestroyIsRejected asserts the module refuses the pair
// itself, naming both options, rather than leaving tofu to fail on it.
func (t *Tests) PlanRefreshOnlyWithDestroyIsRejected(ctx context.Context) error {
	_, err := opentofu().
		Config(fixture("basic")).
		Plan(dagger.OpentofuConfigPlanOpts{Destroy: true, RefreshOnly: true}).
		Sync(ctx)
	return expectErrorContains(err, "destroy", "refreshOnly")
}

// ------------------------------------------------------- refresh-only Ci gate

// CiCheckWithRefreshOnlyPlanDetectsDrift asserts a refresh-only drift gate
// fails on an object changed outside tofu.
func (t *Tests) CiCheckWithRefreshOnlyPlanDetectsDrift(ctx context.Context) error {
	state, err := applyDrift(ctx)
	if err != nil {
		return err
	}
	err = driftSource("changed outside tofu\n").
		WithState(state).
		Ci().
		WithPlan(dagger.OpentofuCiWithPlanOpts{FailOnChanges: true, RefreshOnly: true}).
		Check(ctx)
	return expectErrorContains(err, "changed outside tofu", "local_file.managed")
}

// CiCheckWithRefreshOnlyPlanIgnoresConfigurationChanges asserts the same gate
// passes when only the configuration moved — the case an ordinary drift gate
// cannot tell apart from drift.
func (t *Tests) CiCheckWithRefreshOnlyPlanIgnoresConfigurationChanges(ctx context.Context) error {
	state, err := applyDrift(ctx)
	if err != nil {
		return err
	}
	err = driftSource(managedContent).
		WithState(state).
		WithVar("content", "a configuration change not yet applied\n").
		Ci().
		WithPlan(dagger.OpentofuCiWithPlanOpts{FailOnChanges: true, RefreshOnly: true}).
		Check(ctx)
	if err != nil {
		return fmt.Errorf("expected a refresh-only drift gate to ignore a configuration change, got: %w", err)
	}
	return nil
}

// ------------------------------------------------------------------ helpers

// applyDrift applies the drift fixture from an empty state and returns the
// state it produced.
func applyDrift(ctx context.Context) (*dagger.File, error) {
	result, err := pin(ctx, opentofu().Config(fixture("drift")).Apply())
	if err != nil {
		return nil, fmt.Errorf("Apply the drift fixture: %w", err)
	}
	return result.File(stateFileName), nil
}

// driftSource is the drift fixture with managed.txt holding content.
func driftSource(content string) *dagger.OpentofuConfig {
	return opentofu().Config(fixture("drift").WithNewFile(managedFileName, content))
}

// expectChanges asserts a plan's changes verdict, quoting the rendered plan
// when it is not the one wanted.
func expectChanges(ctx context.Context, what string, dir *dagger.Directory, want string) error {
	plan, err := pin(ctx, dir)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	changes, err := plan.File(changesName).Contents(ctx)
	if err != nil {
		return fmt.Errorf("%s: read %s: %w", what, changesName, err)
	}
	if changes != want {
		text, _ := plan.File(planTextName).Contents(ctx)
		return fmt.Errorf("%s: expected changes=%q, got %q:\n%s", what, want, changes, text)
	}
	return nil
}
