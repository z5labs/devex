package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/opentofu/internal/dagger"

	"github.com/dagger/dagger/util/parallel"
)

// Ci is a chained builder for a standardized OpenTofu CI pipeline. Construct
// via Config.Ci(); enable stages via the With* methods; call Check to run the
// enabled stages, or Run to run them and keep the plan artifacts.
//
// The enabled stages run in parallel and their errors are aggregated, so one
// call reports everything that is wrong with a configuration rather than the
// first thing tofu happened to trip over.
//
// It hangs off Config rather than off Opentofu — a divergence from
// Zig.Ci(source) and Kicad.Ci(source). Every stage beyond fmt needs the
// variables, credentials and backend settings bound to a Config, and
// re-declaring them here would duplicate nine modifiers.
type Ci struct {
	// +private
	Config *Config

	// +private
	FmtEnabled bool
	// +private
	ValidateEnabled bool

	// +private
	PlanEnabled bool
	// +private
	PlanFailOnChanges bool
}

// Ci returns a new pipeline builder bound to this configuration. Everything
// already set on the Config — variables, credentials, backend settings,
// workspace, state — applies to every stage the pipeline runs.
func (c *Config) Ci() *Ci {
	return &Ci{Config: c}
}

// WithFmt enables the `tofu fmt -check -diff -recursive` stage.
func (ci *Ci) WithFmt() *Ci {
	ci.FmtEnabled = true
	return ci
}

// WithValidate enables the `tofu validate` stage. It initialises with
// -backend=false, so a configuration declaring a remote backend is checked
// without any credentials.
func (ci *Ci) WithValidate() *Ci {
	ci.ValidateEnabled = true
	return ci
}

// WithPlan enables the plan stage, making Check run `tofu plan` against the
// configured state or backend. Unlike fmt and validate, this one reaches the
// providers: it needs whatever credentials the configuration's providers
// require, supplied through the Config's WithSecretVariable.
//
// Pass failOnChanges to turn the pipeline into a drift detector — a non-empty
// plan against live infrastructure fails the check. Left false, the stage only
// gates on the plan *succeeding*, which is the right shape for a pull-request
// gate where pending changes are the whole point of the change.
func (ci *Ci) WithPlan(
	// Fail the check when the plan is non-empty.
	// +default=false
	failOnChanges bool,
) *Ci {
	ci.PlanEnabled = true
	ci.PlanFailOnChanges = failOnChanges
	return ci
}

// Check runs the enabled stages in parallel via
// github.com/dagger/dagger/util/parallel and returns the aggregated error.
//
// Every enabled stage runs even when an earlier one has already failed, and
// every failure reaches the caller: an unformatted *and* invalid configuration
// reports both, rather than hiding the validation error behind the formatting
// one until the next round trip.
//
// A pipeline with no stages enabled is an error rather than a pass. Checking
// nothing and reporting success is the purest false green there is — see issue
// #161, where a Check that skipped the one stage that could fail reported a
// configuration as sound when it was not.
//
// +check
// +cache="session"
func (ci *Ci) Check(ctx context.Context) error {
	if !ci.FmtEnabled && !ci.ValidateEnabled && !ci.PlanEnabled {
		return fmt.Errorf(
			"Ci.Check: no stages enabled — call WithFmt, WithValidate or WithPlan; " +
				"a pipeline that checks nothing would report success unconditionally")
	}
	stages := ci.stages()
	if ci.PlanEnabled {
		stages = append(stages, ciStage{"plan", func(ctx context.Context) error {
			_, err := ci.runPlan(ctx)
			return err
		}})
	}
	return runStages(ctx, stages)
}

// Run performs the same stages as Check and returns the plan artifacts —
// plan.tfplan, plan.json, plan.txt and changes, exactly what Config.Plan
// emits — for downstream consumption: a review gate that renders the plan, an
// Apply that consumes the saved plan, an artifact attached to a pull request.
//
// It plans whether or not WithPlan was called, because it must produce the
// directory it returns; WithPlan(failOnChanges: true) additionally makes a
// non-empty plan fail the run. The plan is run once, not once per role: when
// WithPlan enabled it as a check stage too, that single run is both.
//
// Everything runs in one parallel round, so the returned artifacts come from a
// pipeline where every stage passed. A failing stage yields the aggregated
// error and a nil directory.
//
// +check
// +cache="session"
func (ci *Ci) Run(ctx context.Context) (*dagger.Directory, error) {
	var out *dagger.Directory
	stages := append(ci.stages(), ciStage{"plan", func(ctx context.Context) error {
		dir, err := ci.runPlan(ctx)
		if err != nil {
			return err
		}
		// Written by this job alone, and read only after every job has
		// finished, so the assignment needs no synchronisation of its own.
		out = dir
		return nil
	}})
	if err := runStages(ctx, stages); err != nil {
		return nil, err
	}
	return out, nil
}

// ciStage is one named unit of work in the pipeline. Check and Run assemble
// their own stage lists because they want different things from the plan:
// Check discards its artifacts, Run returns them.
type ciStage struct {
	name string
	fn   func(context.Context) error
}

// stages returns the enabled stages that never produce an artifact.
func (ci *Ci) stages() []ciStage {
	var stages []ciStage
	if ci.FmtEnabled {
		stages = append(stages, ciStage{"fmt", ci.runFmt})
	}
	if ci.ValidateEnabled {
		stages = append(stages, ciStage{"validate", ci.runValidate})
	}
	return stages
}

// runStages runs every stage concurrently and returns their aggregated error.
// The aggregation is the point: a parallel run left without WithFailFast waits
// for every job and joins the failures, so a caller sees each broken stage in
// one round trip.
func runStages(ctx context.Context, stages []ciStage) error {
	jobs := parallel.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	for _, stage := range stages {
		jobs = jobs.WithJob(stage.name, stage.fn)
	}
	return jobs.Run(ctx)
}

func (ci *Ci) runFmt(ctx context.Context) error {
	// Fmt returns the diff as its value and carries it in the error when the
	// configuration needs rewriting. A check only has a use for the failure.
	_, err := ci.Config.Fmt(ctx)
	return err
}

func (ci *Ci) runValidate(ctx context.Context) error {
	return ci.Config.Validate(ctx)
}

// runPlan plans the configuration and, when WithPlan(failOnChanges: true) was
// called, rejects a non-empty plan.
//
// The `changes` file is read back off the plan directory rather than the plan
// being re-run: Config.Plan already resolved the run when it built that
// directory, so this is a read of a finished plan, not a second one.
func (ci *Ci) runPlan(ctx context.Context) (*dagger.Directory, error) {
	dir, err := ci.Config.Plan(ctx, false, nil)
	if err != nil {
		return nil, err
	}
	if !ci.PlanFailOnChanges {
		return dir, nil
	}
	changes, err := dir.File(changesFileName).Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("read %s from the plan: %s", changesFileName, errText(err))
	}
	if strings.TrimSpace(changes) != changesPresent {
		return dir, nil
	}
	text, err := dir.File(planTextFileName).Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("read %s from the plan: %s", planTextFileName, errText(err))
	}
	return nil, fmt.Errorf(
		"tofu plan: the configuration has pending changes and WithPlan(failOnChanges: true) "+
			"requires an empty plan:\n%s", text)
}
