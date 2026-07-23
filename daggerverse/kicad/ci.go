package main

import (
	"context"

	"dagger/kicad/internal/dagger"

	"github.com/dagger/dagger/util/parallel"
)

// bomDefaultFields is the BOM column set Ci exports, kept in sync with
// Sch.Bom's own default so a Ci-produced BOM matches a hand-called one.
const bomDefaultFields = "Reference,Value,Footprint,QUANTITY,DNP"

// Fabrication output kinds Run knows how to produce. WithFabricationOutputs
// enables the full set; Run switches on each in produce.
const (
	outputGerbers = "gerbers"
	outputDrill   = "drill"
	outputPos     = "pos"
	outputBom     = "bom"
)

// Ci is a chained builder for a standardized KiCad CI pipeline. Construct via
// Kicad.Ci(source); enable check stages and output sets via the With* methods;
// call Run to execute checks-then-outputs, or Check to run only the parallel
// checks.
//
// Stage 1 runs the enabled design-rule checks in parallel (Erc, Drc); errors
// are aggregated. Stage 2 produces the enabled outputs as a single directory
// and Run returns it. Downstream consumers compose that directory into their
// own pipelines (archive, upload to a fab house, attach to a release, ...).
//
// It composes the Project/Pcb/Sch primitives without adding capability of its
// own: every stage is a call the caller could make by hand, bundled into one
// declarative pipeline so a hardware repo's CI is a single `dagger call`.
type Ci struct {
	// +private
	Project *Project

	// +private
	ErcEnabled bool
	// +private
	DrcEnabled bool
	// +private
	DrcSchematicParity bool

	// +private
	Outputs []string
}

// Ci returns a new pipeline builder bound to the supplied project source. The
// board and schematic are auto-discovered per stage, exactly as a bare
// Project(source).Pcb()/Sch() call would.
func (k *Kicad) Ci(source *dagger.Directory) *Ci {
	return &Ci{Project: k.Project(source)}
}

// WithErc enables the Electrical Rule Check stage (Sch.Erc at severity error).
func (c *Ci) WithErc() *Ci {
	c.ErcEnabled = true
	return c
}

// WithDrc enables the Design Rule Check stage (Pcb.Drc at severity error).
// Pass schematicParity to also check the board against the schematic
// (footprints, nets, values) — a class of defect plain DRC never looks for.
func (c *Ci) WithDrc(
	// +default=false
	schematicParity bool,
) *Ci {
	c.DrcEnabled = true
	c.DrcSchematicParity = schematicParity
	return c
}

// WithFabricationOutputs enables the fabrication package: Gerbers, drill files,
// the pick-and-place position file and the BOM. Run merges them into one
// directory (gerbers/ and drill/ subdirectories, pos.pos and bom.csv at root).
func (c *Ci) WithFabricationOutputs() *Ci {
	c.Outputs = append(c.Outputs, outputGerbers, outputDrill, outputPos, outputBom)
	return c
}

// Check runs the enabled check stages (Erc, Drc) in parallel via
// github.com/dagger/dagger/util/parallel and returns the aggregated error. Use
// when callers want to run the checks independently of the outputs (for
// example a PR gate that never needs the fabrication package).
//
// +check
// +cache="session"
func (c *Ci) Check(ctx context.Context) error {
	jobs := parallel.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if c.ErcEnabled {
		jobs = jobs.WithJob("erc", c.runErc)
	}
	if c.DrcEnabled {
		jobs = jobs.WithJob("drc", c.runDrc)
	}
	return jobs.Run(ctx)
}

// Run executes the pipeline: stage 1 (Check) → stage 2 (outputs). Returns the
// enabled outputs merged into one directory. On stage-1 failure, returns the
// aggregated error from Check and a nil directory (stage 2 is skipped), so a
// failing check short-circuits before any export work.
//
// +check
// +cache="session"
func (c *Ci) Run(ctx context.Context) (*dagger.Directory, error) {
	if err := c.Check(ctx); err != nil {
		return nil, err
	}
	return c.produce(ctx)
}

func (c *Ci) runErc(ctx context.Context) error {
	return c.Project.Sch("").Erc(ctx, "error")
}

func (c *Ci) runDrc(ctx context.Context) error {
	return c.Project.Pcb("").Drc(ctx, "error", c.DrcSchematicParity, false)
}

// produce runs the enabled output stages and merges their results into a
// single directory. Each stage is the same call the caller could make by hand
// against the auto-discovered board or schematic; gerbers and drill land in
// their own subdirectories to keep their board-name-prefixed files from being
// mistaken for one another.
func (c *Ci) produce(ctx context.Context) (*dagger.Directory, error) {
	out := dag.Directory()
	pcb := c.Project.Pcb("")
	sch := c.Project.Sch("")
	for _, kind := range c.Outputs {
		switch kind {
		case outputGerbers:
			d, err := pcb.Gerbers(ctx, nil, 6, false)
			if err != nil {
				return nil, err
			}
			out = out.WithDirectory("gerbers", d)
		case outputDrill:
			d, err := pcb.Drill(ctx, "excellon", "mm", "absolute", false, false)
			if err != nil {
				return nil, err
			}
			out = out.WithDirectory("drill", d)
		case outputPos:
			f, err := pcb.Pos(ctx, "both", "ascii", "in", false, "pos.pos")
			if err != nil {
				return nil, err
			}
			out = out.WithFile("pos.pos", f)
		case outputBom:
			f, err := sch.Bom(ctx, bomDefaultFields, "", "Reference", false, "bom.csv")
			if err != nil {
				return nil, err
			}
			out = out.WithFile("bom.csv", f)
		}
	}
	return out, nil
}
