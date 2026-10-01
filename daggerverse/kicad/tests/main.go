// Package main implements the test module for the kicad Dagger module. Each
// test is exposed as a standalone dagger function so it can be invoked
// individually during TDD; All wires them up for parallel execution under
// `dagger call all`.
//
// The fixtures under fixtures/ are hand-authored, self-contained KiCad
// projects: symbols and footprints are embedded in the .kicad_sch/.kicad_pcb
// files, so nothing resolves against a system symbol or footprint library and
// the tests stay hermetic.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dagger/tests/internal/dagger"

	par "github.com/dagger/dagger/util/parallel"
)

type Tests struct{}

// All runs every kicad-module test in parallel.
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

	jobs = jobs.WithJob("ContainerHasKicadCli", t.ContainerHasKicadCli)
	jobs = jobs.WithJob("VersionReportsKicadRelease", t.VersionReportsKicadRelease)
	jobs = jobs.WithJob("PcbAutoDiscoversSingleBoard", t.PcbAutoDiscoversSingleBoard)
	jobs = jobs.WithJob("PcbRejectsAmbiguousAutoDiscovery", t.PcbRejectsAmbiguousAutoDiscovery)
	jobs = jobs.WithJob("PcbRejectsMissingBoard", t.PcbRejectsMissingBoard)
	jobs = jobs.WithJob("PcbRejectsExplicitPathNotFound", t.PcbRejectsExplicitPathNotFound)
	jobs = jobs.WithJob("SchAutoDiscoversSingleSchematic", t.SchAutoDiscoversSingleSchematic)

	jobs = jobs.WithJob("ErcCleanProjectPasses", t.ErcCleanProjectPasses)
	jobs = jobs.WithJob("ErcReportsViolations", t.ErcReportsViolations)
	jobs = jobs.WithJob("DrcCleanProjectPasses", t.DrcCleanProjectPasses)
	jobs = jobs.WithJob("DrcReportsViolations", t.DrcReportsViolations)
	jobs = jobs.WithJob("DrcSchematicParityDetectsMismatch", t.DrcSchematicParityDetectsMismatch)

	jobs = jobs.WithJob("GerbersProduceOneFilePerLayer", t.GerbersProduceOneFilePerLayer)
	jobs = jobs.WithJob("GerbersDefaultExportsAllLayers", t.GerbersDefaultExportsAllLayers)
	jobs = jobs.WithJob("DrillProducesExcellonFiles", t.DrillProducesExcellonFiles)
	jobs = jobs.WithJob("PcbPdfPerLayerProducesFilePerLayer", t.PcbPdfPerLayerProducesFilePerLayer)
	jobs = jobs.WithJob("SchSvgProducesOneFilePerSheet", t.SchSvgProducesOneFilePerSheet)
	jobs = jobs.WithJob("PcbPdfIsSingleMultipageFile", t.PcbPdfIsSingleMultipageFile)
	jobs = jobs.WithJob("PcbSvgProducesSingleSvg", t.PcbSvgProducesSingleSvg)
	jobs = jobs.WithJob("SchPdfProducesPdf", t.SchPdfProducesPdf)
	jobs = jobs.WithJob("PosDefaultsToAsciiBothSides", t.PosDefaultsToAsciiBothSides)
	jobs = jobs.WithJob("BomDefaultFieldsProduceCsvHeader", t.BomDefaultFieldsProduceCsvHeader)
	jobs = jobs.WithJob("NetlistDefaultsToKicadSexpr", t.NetlistDefaultsToKicadSexpr)
	jobs = jobs.WithJob("Ipc2581ProducesXml", t.Ipc2581ProducesXml)
	jobs = jobs.WithJob("StepBoardOnlyProducesStepFile", t.StepBoardOnlyProducesStepFile)

	jobs = jobs.WithJob("WithVarSubstitutesTextVariable", t.WithVarSubstitutesTextVariable)
	jobs = jobs.WithJob("WithVarRejectsNameContainingEquals", t.WithVarRejectsNameContainingEquals)
	jobs = jobs.WithJob("RejectsOutputNameWithPathSeparator", t.RejectsOutputNameWithPathSeparator)
	jobs = jobs.WithJob("DrillRejectsInvalidFormat", t.DrillRejectsInvalidFormat)
	jobs = jobs.WithJob("NetlistRejectsInvalidFormat", t.NetlistRejectsInvalidFormat)
	jobs = jobs.WithJob("JobsetRunProducesDeclaredOutputs", t.JobsetRunProducesDeclaredOutputs)
	jobs = jobs.WithJob("JobsetRejectsMissingFile", t.JobsetRejectsMissingFile)

	jobs = jobs.WithJob("WithDrawingSheetAppliesCustomSheet", t.WithDrawingSheetAppliesCustomSheet)
	jobs = jobs.WithJob("WithVariantSelectsDesignVariant", t.WithVariantSelectsDesignVariant)
	jobs = jobs.WithJob("WithVariantRejectsUnknownVariant", t.WithVariantRejectsUnknownVariant)
	jobs = jobs.WithJob("WithVariantIgnoredByChecks", t.WithVariantIgnoredByChecks)

	jobs = jobs.WithJob("CiCheckRunsErcAndDrc", t.CiCheckRunsErcAndDrc)
	jobs = jobs.WithJob("CiCheckFailsOnViolations", t.CiCheckFailsOnViolations)
	jobs = jobs.WithJob("CiRunProducesFabricationOutputs", t.CiRunProducesFabricationOutputs)
	jobs = jobs.WithJob("CiRunShortCircuitsOnFailingCheck", t.CiRunShortCircuitsOnFailingCheck)

	jobs = jobs.WithJob("ThreeDExportRequiresFullImage", t.ThreeDExportRequiresFullImage)
	jobs = jobs.WithJob("StepWithComponentModelsIncludesModels", t.StepWithComponentModelsIncludesModels)
	jobs = jobs.WithJob("GlbBoardOnlyProducesGlb", t.GlbBoardOnlyProducesGlb)
	jobs = jobs.WithJob("VrmlBoardOnlyProducesVrml", t.VrmlBoardOnlyProducesVrml)
	jobs = jobs.WithJob("PcbDxfProducesDxf", t.PcbDxfProducesDxf)
	jobs = jobs.WithJob("PcbPsProducesPostscript", t.PcbPsProducesPostscript)
	jobs = jobs.WithJob("StatsProducesReport", t.StatsProducesReport)
	jobs = jobs.WithJob("GencadProducesGencad", t.GencadProducesGencad)
	jobs = jobs.WithJob("Ipcd356ProducesNetlist", t.Ipcd356ProducesNetlist)
	jobs = jobs.WithJob("OdbProducesArchive", t.OdbProducesArchive)
	jobs = jobs.WithJob("RenderProducesPng", t.RenderProducesPng)
	jobs = jobs.WithJob("ImportRejectsUnknownFormat", t.ImportRejectsUnknownFormat)
	jobs = jobs.WithJob("PcbUpgradeProducesBoard", t.PcbUpgradeProducesBoard)
	jobs = jobs.WithJob("SchDxfProducesFilePerSheet", t.SchDxfProducesFilePerSheet)
	jobs = jobs.WithJob("SchPsProducesFilePerSheet", t.SchPsProducesFilePerSheet)
	jobs = jobs.WithJob("FpSvgExportsFootprint", t.FpSvgExportsFootprint)
	jobs = jobs.WithJob("FpUpgradeResavesLibrary", t.FpUpgradeResavesLibrary)
	jobs = jobs.WithJob("SymSvgExportsSymbol", t.SymSvgExportsSymbol)
	jobs = jobs.WithJob("SymUpgradeResavesLibrary", t.SymUpgradeResavesLibrary)

	return jobs.Run(ctx)
}

// ---------------------------------------------------------------- toolchain

// ContainerHasKicadCli asserts the base image exposes kicad-cli on PATH, so
// the escape hatch documented on Container() actually works.
func (t *Tests) ContainerHasKicadCli(ctx context.Context) error {
	out, err := dag.Kicad().Container().
		WithExec([]string{"which", "kicad-cli"}).
		Stdout(ctx)
	if err != nil {
		return fmt.Errorf("which kicad-cli: %w", err)
	}
	if !strings.Contains(out, "kicad-cli") {
		return fmt.Errorf("expected a kicad-cli path, got %q", out)
	}
	return nil
}

// VersionReportsKicadRelease asserts Version reports the release the pinned
// image ships, i.e. a 10.x version for the default 10.0 tag.
func (t *Tests) VersionReportsKicadRelease(ctx context.Context) error {
	out, err := dag.Kicad().Version(ctx)
	if err != nil {
		return fmt.Errorf("Version: %w", err)
	}
	if !strings.HasPrefix(out, "10.") {
		return fmt.Errorf("expected a 10.x KiCad version, got %q", out)
	}
	return nil
}

// PcbAutoDiscoversSingleBoard asserts an empty path finds the project's only
// board — the produced drill file is named after it, so a wrong pick would
// show up in the output name.
func (t *Tests) PcbAutoDiscoversSingleBoard(ctx context.Context) error {
	entries, err := dag.Kicad().Project(fixture("blinky")).Pcb().Drill().Entries(ctx)
	if err != nil {
		return fmt.Errorf("Drill on auto-discovered board: %w", err)
	}
	if !contains(entries, "blinky.drl") {
		return fmt.Errorf("expected blinky.drl from the auto-discovered board, got %v", entries)
	}
	return nil
}

// PcbRejectsAmbiguousAutoDiscovery asserts a project with two boards and no
// board named after the project file errors, naming both candidates.
func (t *Tests) PcbRejectsAmbiguousAutoDiscovery(ctx context.Context) error {
	_, err := dag.Kicad().Project(fixture("two-boards")).Pcb().Drill().Sync(ctx)
	if err == nil {
		return fmt.Errorf("expected an ambiguity error for two boards, got nil")
	}
	for _, want := range []string{"left.kicad_pcb", "right.kicad_pcb"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to name %s, got: %v", want, err)
		}
	}
	return nil
}

// PcbRejectsMissingBoard asserts a project with no board at all errors,
// rather than letting kicad-cli fail on an empty argument.
func (t *Tests) PcbRejectsMissingBoard(ctx context.Context) error {
	_, err := dag.Kicad().Project(fixture("no-board")).Pcb().Drill().Sync(ctx)
	if err == nil {
		return fmt.Errorf("expected a not-found error for a project with no board, got nil")
	}
	if !strings.Contains(err.Error(), "no *.kicad_pcb found in project") {
		return fmt.Errorf("expected a no-board error, got: %v", err)
	}
	return nil
}

// PcbRejectsExplicitPathNotFound asserts an explicit path that is not in the
// tree is reported as such, naming the path.
func (t *Tests) PcbRejectsExplicitPathNotFound(ctx context.Context) error {
	_, err := dag.Kicad().Project(fixture("blinky")).
		Pcb(dagger.KicadProjectPcbOpts{Path: "nope.kicad_pcb"}).
		Drill().Sync(ctx)
	if err == nil {
		return fmt.Errorf("expected a not-found error for an explicit bad path, got nil")
	}
	if !strings.Contains(err.Error(), `"nope.kicad_pcb" not found in project`) {
		return fmt.Errorf("expected a path-not-found error, got: %v", err)
	}
	return nil
}

// SchAutoDiscoversSingleSchematic asserts an empty path finds the project's
// schematic; the netlist records the source sheet, so it names what was
// picked.
func (t *Tests) SchAutoDiscoversSingleSchematic(ctx context.Context) error {
	out, err := dag.Kicad().Project(fixture("blinky")).Sch().Netlist().Contents(ctx)
	if err != nil {
		return fmt.Errorf("Netlist on auto-discovered schematic: %w", err)
	}
	if !strings.Contains(out, "blinky.kicad_sch") {
		return fmt.Errorf("expected the netlist to name blinky.kicad_sch, got:\n%s", head(out))
	}
	return nil
}

// ------------------------------------------------------------------- checks

// ErcCleanProjectPasses asserts a clean schematic returns nil.
func (t *Tests) ErcCleanProjectPasses(ctx context.Context) error {
	if err := dag.Kicad().Project(fixture("blinky")).Sch().Erc(ctx); err != nil {
		return fmt.Errorf("expected a clean ERC for blinky, got: %w", err)
	}
	return nil
}

// ErcReportsViolations asserts a schematic with a dangling pin fails and that
// the violation list — not just a count — makes it into the error.
func (t *Tests) ErcReportsViolations(ctx context.Context) error {
	err := dag.Kicad().Project(fixture("violations")).Sch().Erc(ctx)
	if err == nil {
		return fmt.Errorf("expected an ERC failure for the violations fixture, got nil")
	}
	if !strings.Contains(err.Error(), "pin_not_connected") {
		return fmt.Errorf("expected the ERC report in the error, got: %v", err)
	}
	return nil
}

// DrcCleanProjectPasses asserts a clean board returns nil.
func (t *Tests) DrcCleanProjectPasses(ctx context.Context) error {
	if err := dag.Kicad().Project(fixture("blinky")).Pcb().Drc(ctx); err != nil {
		return fmt.Errorf("expected a clean DRC for blinky, got: %w", err)
	}
	return nil
}

// DrcReportsViolations asserts a board with overlapping footprints fails and
// that the violation list makes it into the error.
func (t *Tests) DrcReportsViolations(ctx context.Context) error {
	err := dag.Kicad().Project(fixture("violations")).Pcb().Drc(ctx)
	if err == nil {
		return fmt.Errorf("expected a DRC failure for the violations fixture, got nil")
	}
	if !strings.Contains(err.Error(), "DRC violations") {
		return fmt.Errorf("expected the DRC report in the error, got: %v", err)
	}
	return nil
}

// DrcSchematicParityDetectsMismatch asserts schematicParity surfaces a board
// whose pad nets disagree with the schematic — a class of defect plain DRC
// never looks for.
func (t *Tests) DrcSchematicParityDetectsMismatch(ctx context.Context) error {
	err := dag.Kicad().Project(fixture("violations")).Pcb().
		Drc(ctx, dagger.KicadPcbDrcOpts{SchematicParity: true})
	if err == nil {
		return fmt.Errorf("expected a parity failure for the violations fixture, got nil")
	}
	if !strings.Contains(err.Error(), "net_conflict") {
		return fmt.Errorf("expected a net_conflict parity violation, got: %v", err)
	}
	return nil
}

// ------------------------------------------------------------------ exports

// GerbersProduceOneFilePerLayer asserts an explicit layer list plots exactly
// those layers (plus the job file that ties them together).
func (t *Tests) GerbersProduceOneFilePerLayer(ctx context.Context) error {
	entries, err := dag.Kicad().Project(fixture("blinky")).Pcb().
		Gerbers(dagger.KicadPcbGerbersOpts{Layers: []string{"F.Cu", "B.Cu"}}).
		Entries(ctx)
	if err != nil {
		return fmt.Errorf("Gerbers: %w", err)
	}
	for _, want := range []string{"blinky-F_Cu.gtl", "blinky-B_Cu.gbl"} {
		if !contains(entries, want) {
			return fmt.Errorf("expected %s in the gerber output, got %v", want, entries)
		}
	}
	if contains(entries, "blinky-F_Silkscreen.gto") {
		return fmt.Errorf("expected only the requested layers, got %v", entries)
	}
	return nil
}

// GerbersDefaultExportsAllLayers asserts an empty layer list plots every
// layer the board defines rather than silently plotting none.
func (t *Tests) GerbersDefaultExportsAllLayers(ctx context.Context) error {
	entries, err := dag.Kicad().Project(fixture("blinky")).Pcb().Gerbers().Entries(ctx)
	if err != nil {
		return fmt.Errorf("Gerbers: %w", err)
	}
	for _, want := range []string{
		"blinky-F_Cu.gtl", "blinky-B_Cu.gbl", "blinky-F_Silkscreen.gto",
		"blinky-Edge_Cuts.gm1", "blinky-job.gbrjob",
	} {
		if !contains(entries, want) {
			return fmt.Errorf("expected %s in the default gerber output, got %v", want, entries)
		}
	}
	return nil
}

// DrillProducesExcellonFiles asserts the default drill export writes an
// Excellon file for the board.
func (t *Tests) DrillProducesExcellonFiles(ctx context.Context) error {
	entries, err := dag.Kicad().Project(fixture("blinky")).Pcb().Drill().Entries(ctx)
	if err != nil {
		return fmt.Errorf("Drill: %w", err)
	}
	if !contains(entries, "blinky.drl") {
		return fmt.Errorf("expected blinky.drl, got %v", entries)
	}
	return nil
}

// PcbPdfPerLayerProducesFilePerLayer asserts --mode-separate lands one PDF
// per requested layer in the returned directory.
func (t *Tests) PcbPdfPerLayerProducesFilePerLayer(ctx context.Context) error {
	entries, err := dag.Kicad().Project(fixture("blinky")).Pcb().
		PdfPerLayer([]string{"F.Cu", "B.Cu"}).Entries(ctx)
	if err != nil {
		return fmt.Errorf("PdfPerLayer: %w", err)
	}
	for _, want := range []string{"blinky-F_Cu.pdf", "blinky-B_Cu.pdf"} {
		if !contains(entries, want) {
			return fmt.Errorf("expected %s, got %v", want, entries)
		}
	}
	return nil
}

// SchSvgProducesOneFilePerSheet asserts a hierarchical schematic plots one
// SVG per sheet, which also proves the root sheet — not a sub-sheet — was the
// one auto-discovered.
func (t *Tests) SchSvgProducesOneFilePerSheet(ctx context.Context) error {
	entries, err := dag.Kicad().Project(fixture("hierarchical")).Sch().Svg().Entries(ctx)
	if err != nil {
		return fmt.Errorf("Svg: %w", err)
	}
	for _, want := range []string{"hierarchical.svg", "hierarchical-sub.svg"} {
		if !contains(entries, want) {
			return fmt.Errorf("expected %s, got %v", want, entries)
		}
	}
	return nil
}

// PcbPdfIsSingleMultipageFile asserts --mode-single produces one real PDF.
// The assertion goes through Export + os.ReadFile rather than Contents()
// because Contents mangles non-UTF-8 bytes.
func (t *Tests) PcbPdfIsSingleMultipageFile(ctx context.Context) error {
	f := dag.Kicad().Project(fixture("blinky")).Pcb().
		Pdf([]string{"F.Cu", "B.Cu", "Edge.Cuts"})
	return assertMagic(ctx, f, "board.pdf", []byte("%PDF"))
}

// PcbSvgProducesSingleSvg asserts --mode-single produces one SVG document.
func (t *Tests) PcbSvgProducesSingleSvg(ctx context.Context) error {
	out, err := dag.Kicad().Project(fixture("blinky")).Pcb().
		Svg([]string{"F.Cu", "Edge.Cuts"}).Contents(ctx)
	if err != nil {
		return fmt.Errorf("Svg: %w", err)
	}
	if !strings.Contains(out, "<svg") {
		return fmt.Errorf("expected an SVG document, got:\n%s", head(out))
	}
	return nil
}

// SchPdfProducesPdf asserts the schematic PDF export produces a real PDF.
func (t *Tests) SchPdfProducesPdf(ctx context.Context) error {
	f := dag.Kicad().Project(fixture("blinky")).Sch().Pdf()
	return assertMagic(ctx, f, "schematic.pdf", []byte("%PDF"))
}

// PosDefaultsToAsciiBothSides asserts the default position file is the ascii
// format covering both sides, and lists the board's footprints.
func (t *Tests) PosDefaultsToAsciiBothSides(ctx context.Context) error {
	out, err := dag.Kicad().Project(fixture("blinky")).Pcb().Pos().Contents(ctx)
	if err != nil {
		return fmt.Errorf("Pos: %w", err)
	}
	if !strings.Contains(out, "## Side : All") {
		return fmt.Errorf("expected a both-sides ascii position file, got:\n%s", head(out))
	}
	for _, want := range []string{"R1", "D1"} {
		if !strings.Contains(out, want) {
			return fmt.Errorf("expected %s in the position file, got:\n%s", want, head(out))
		}
	}
	return nil
}

// BomDefaultFieldsProduceCsvHeader asserts the default field list produces
// the matching CSV header and one row per component.
func (t *Tests) BomDefaultFieldsProduceCsvHeader(ctx context.Context) error {
	out, err := dag.Kicad().Project(fixture("blinky")).Sch().Bom().Contents(ctx)
	if err != nil {
		return fmt.Errorf("Bom: %w", err)
	}
	if !strings.HasPrefix(out, `"Reference","Value","Footprint","QUANTITY","DNP"`) {
		return fmt.Errorf("expected the default BOM header, got:\n%s", head(out))
	}
	if !strings.Contains(out, `"R1"`) || !strings.Contains(out, `"D1"`) {
		return fmt.Errorf("expected R1 and D1 rows, got:\n%s", head(out))
	}
	return nil
}

// NetlistDefaultsToKicadSexpr asserts the default netlist format is KiCad's
// own s-expression export, carrying the nets the schematic declares.
func (t *Tests) NetlistDefaultsToKicadSexpr(ctx context.Context) error {
	out, err := dag.Kicad().Project(fixture("blinky")).Sch().Netlist().Contents(ctx)
	if err != nil {
		return fmt.Errorf("Netlist: %w", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "(export") {
		return fmt.Errorf("expected a kicadsexpr netlist, got:\n%s", head(out))
	}
	for _, want := range []string{`(name "VCC")`, `(name "GND")`} {
		if !strings.Contains(out, want) {
			return fmt.Errorf("expected %s in the netlist, got:\n%s", want, head(out))
		}
	}
	return nil
}

// Ipc2581ProducesXml asserts the IPC-2581 export produces an XML document at
// the requested revision.
func (t *Tests) Ipc2581ProducesXml(ctx context.Context) error {
	out, err := dag.Kicad().Project(fixture("blinky")).Pcb().Ipc2581().Contents(ctx)
	if err != nil {
		return fmt.Errorf("Ipc2581: %w", err)
	}
	if !strings.HasPrefix(out, "<?xml") {
		return fmt.Errorf("expected an XML document, got:\n%s", head(out))
	}
	if !strings.Contains(out, "IPC-2581") {
		return fmt.Errorf("expected an IPC-2581 document, got:\n%s", head(out))
	}
	return nil
}

// StepBoardOnlyProducesStepFile asserts the board-only STEP export produces a
// real ISO-10303-21 file. Asserted via Export + os.ReadFile, not Contents.
func (t *Tests) StepBoardOnlyProducesStepFile(ctx context.Context) error {
	f := dag.Kicad().Project(fixture("blinky")).Pcb().
		Step(dagger.KicadPcbStepOpts{BoardOnly: true})
	return assertMagic(ctx, f, "board.step", []byte("ISO-10303-21;"))
}

// ------------------------------------------------------- validation, jobset

// WithVarSubstitutesTextVariable asserts WithVar overrides the value the
// project file declares. The blinky board carries a `${LEDCOLOR}` silkscreen
// text; the IPC-2581 export records resolved text verbatim, so it shows which
// value won.
func (t *Tests) WithVarSubstitutesTextVariable(ctx context.Context) error {
	project := dag.Kicad().Project(fixture("blinky"))

	base, err := project.Pcb().Ipc2581().Contents(ctx)
	if err != nil {
		return fmt.Errorf("Ipc2581 without WithVar: %w", err)
	}
	if !strings.Contains(base, `name="TEXT" value="green"`) {
		return fmt.Errorf("expected the project's default LEDCOLOR=green, got:\n%s", head(base))
	}

	out, err := project.WithVar("LEDCOLOR", "red").Pcb().Ipc2581().Contents(ctx)
	if err != nil {
		return fmt.Errorf("Ipc2581 with WithVar: %w", err)
	}
	if !strings.Contains(out, `name="TEXT" value="red"`) {
		return fmt.Errorf("expected LEDCOLOR to be overridden to red, got:\n%s", head(out))
	}
	return nil
}

// WithVarRejectsNameContainingEquals asserts a name that would corrupt
// kicad-cli's `name=value` encoding is rejected. WithVar is a builder with no
// error return, so the error has to surface on the exec that uses it.
func (t *Tests) WithVarRejectsNameContainingEquals(ctx context.Context) error {
	err := dag.Kicad().Project(fixture("blinky")).
		WithVar("BAD=NAME", "x").Sch().Erc(ctx)
	if err == nil {
		return fmt.Errorf("expected an error for a variable name containing '=', got nil")
	}
	if !strings.Contains(err.Error(), "must not contain") {
		return fmt.Errorf("expected the WithVar validation error, got: %v", err)
	}

	err = dag.Kicad().Project(fixture("blinky")).
		WithVar("", "x").Sch().Erc(ctx)
	if err == nil {
		return fmt.Errorf("expected an error for an empty variable name, got nil")
	}
	if !strings.Contains(err.Error(), "variable name is required") {
		return fmt.Errorf("expected the empty-name validation error, got: %v", err)
	}
	return nil
}

// RejectsOutputNameWithPathSeparator asserts an artifact name that would walk
// out of the module-owned output directory is rejected up front.
func (t *Tests) RejectsOutputNameWithPathSeparator(ctx context.Context) error {
	_, err := dag.Kicad().Project(fixture("blinky")).Sch().
		Bom(dagger.KicadSchBomOpts{OutputName: "sub/bom.csv"}).Sync(ctx)
	if err == nil {
		return fmt.Errorf("expected an error for an outputName containing '/', got nil")
	}
	if !strings.Contains(err.Error(), "must be a file name, not a path") {
		return fmt.Errorf("expected the outputName validation error, got: %v", err)
	}
	return nil
}

// DrillRejectsInvalidFormat asserts an out-of-range enum is rejected with the
// legal set spelled out, rather than passed through to kicad-cli.
func (t *Tests) DrillRejectsInvalidFormat(ctx context.Context) error {
	_, err := dag.Kicad().Project(fixture("blinky")).Pcb().
		Drill(dagger.KicadPcbDrillOpts{Format: "postscript"}).Sync(ctx)
	if err == nil {
		return fmt.Errorf("expected an error for an invalid drill format, got nil")
	}
	if !strings.Contains(err.Error(), "must be one of excellon, gerber") {
		return fmt.Errorf("expected the legal format set in the error, got: %v", err)
	}
	return nil
}

// NetlistRejectsInvalidFormat asserts the netlist format enum is validated
// the same way, listing every format kicad-cli accepts.
func (t *Tests) NetlistRejectsInvalidFormat(ctx context.Context) error {
	_, err := dag.Kicad().Project(fixture("blinky")).Sch().
		Netlist(dagger.KicadSchNetlistOpts{Format: "verilog"}).Sync(ctx)
	if err == nil {
		return fmt.Errorf("expected an error for an invalid netlist format, got nil")
	}
	if !strings.Contains(err.Error(), "kicadsexpr") {
		return fmt.Errorf("expected the legal format set in the error, got: %v", err)
	}
	return nil
}

// JobsetRunProducesDeclaredOutputs asserts a jobset runs and its declared
// output folder comes back populated.
func (t *Tests) JobsetRunProducesDeclaredOutputs(ctx context.Context) error {
	out := dag.Kicad().Project(fixture("jobset")).Jobset("jobset.kicad_jobset")
	entries, err := out.Directory("fab").Entries(ctx)
	if err != nil {
		return fmt.Errorf("Jobset: %w", err)
	}
	for _, want := range []string{"jobset-F_Cu.gtl", "jobset-job.gbrjob"} {
		if !contains(entries, want) {
			return fmt.Errorf("expected %s in the jobset output, got %v", want, entries)
		}
	}
	return nil
}

// JobsetRejectsMissingFile asserts a jobset path that is not in the tree is
// reported as such, naming the path.
func (t *Tests) JobsetRejectsMissingFile(ctx context.Context) error {
	_, err := dag.Kicad().Project(fixture("jobset")).Jobset("nope.kicad_jobset").Sync(ctx)
	if err == nil {
		return fmt.Errorf("expected a not-found error for a missing jobset, got nil")
	}
	if !strings.Contains(err.Error(), `"nope.kicad_jobset" not found in project`) {
		return fmt.Errorf("expected a jobset-not-found error, got: %v", err)
	}
	return nil
}

// ------------------------------------------------ drawing sheet, variants

// WithDrawingSheetAppliesCustomSheet asserts the custom drawing sheet's
// title-block text lands in an export produced with it and is absent without
// it. The board's SVG plot renders worksheet text as literal <text> elements
// (unlike the PDF plot, which strokes it to geometry), so the marker string is
// greppable in the exported document rather than only in rendered pixels.
func (t *Tests) WithDrawingSheetAppliesCustomSheet(ctx context.Context) error {
	const marker = "DEVEX-DRAWING-SHEET-158"
	project := dag.Kicad().Project(fixture("blinky"))
	layers := []string{"F.Cu", "Edge.Cuts"}

	with, err := project.
		WithDrawingSheet(fixtureFile("drawing-sheet/custom.kicad_wks")).
		Pcb().Svg(layers).Contents(ctx)
	if err != nil {
		return fmt.Errorf("Svg with the custom drawing sheet: %w", err)
	}
	if !strings.Contains(with, marker) {
		return fmt.Errorf("expected the custom sheet's title-block text %q in the export, got:\n%s", marker, head(with))
	}

	without, err := project.Pcb().Svg(layers).Contents(ctx)
	if err != nil {
		return fmt.Errorf("Svg without the custom drawing sheet: %w", err)
	}
	if strings.Contains(without, marker) {
		return fmt.Errorf("expected %q to be absent without the custom sheet, but it was present", marker)
	}
	return nil
}

// WithVariantSelectsDesignVariant asserts two assembly variants of the same
// project produce different BOMs. The variants fixture overrides R1's value per
// variant (1k vs 10k), which the default BOM's Value column records verbatim,
// so the selected variant is observable in exported text rather than geometry.
func (t *Tests) WithVariantSelectsDesignVariant(ctx context.Context) error {
	project := dag.Kicad().Project(fixture("variants"))

	std, err := project.WithVariant("StandardBuild").Sch().Bom().Contents(ctx)
	if err != nil {
		return fmt.Errorf("Bom for StandardBuild: %w", err)
	}
	high, err := project.WithVariant("HighValueBuild").Sch().Bom().Contents(ctx)
	if err != nil {
		return fmt.Errorf("Bom for HighValueBuild: %w", err)
	}
	if std == high {
		return fmt.Errorf("expected the two variants to produce different BOMs, got identical output:\n%s", head(std))
	}
	if !strings.Contains(std, `"R1","1k"`) {
		return fmt.Errorf("expected R1=1k in the StandardBuild BOM, got:\n%s", head(std))
	}
	if !strings.Contains(high, `"R1","10k"`) {
		return fmt.Errorf("expected R1=10k in the HighValueBuild BOM, got:\n%s", head(high))
	}
	return nil
}

// WithVariantRejectsUnknownVariant asserts an undeclared variant name produces
// a clear error naming the variants the project does declare. kicad-cli
// silently falls back to the default variant for an unknown name, so this is
// the module's own validation rather than a passed-through kicad-cli error.
func (t *Tests) WithVariantRejectsUnknownVariant(ctx context.Context) error {
	_, err := dag.Kicad().Project(fixture("variants")).
		WithVariant("NoSuchVariant").Sch().Bom().Sync(ctx)
	if err == nil {
		return fmt.Errorf("expected an error for an undeclared variant, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unknown variant") {
		return fmt.Errorf("expected a clear unknown-variant error, got: %v", err)
	}
	for _, want := range []string{"StandardBuild", "HighValueBuild"} {
		if !strings.Contains(msg, want) {
			return fmt.Errorf("expected the error to list the declared variant %s, got: %v", want, err)
		}
	}
	return nil
}

// WithVariantIgnoredByChecks asserts a variant-bearing project still passes ERC
// and DRC with a variant selected. kicad-cli rejects --variant on sch erc and
// pcb drc, so the module drops the flag there; a clean pass proves it was
// dropped rather than passed through, which would fail as a usage error.
func (t *Tests) WithVariantIgnoredByChecks(ctx context.Context) error {
	project := dag.Kicad().Project(fixture("variants")).WithVariant("HighValueBuild")
	if err := project.Sch().Erc(ctx); err != nil {
		return fmt.Errorf("expected Erc to drop the selected variant and pass, got: %w", err)
	}
	if err := project.Pcb().Drc(ctx); err != nil {
		return fmt.Errorf("expected Drc to drop the selected variant and pass, got: %w", err)
	}
	return nil
}

// ----------------------------------------------------------------------- ci

// CiCheckRunsErcAndDrc asserts the chained pipeline runs both enabled checks
// against a clean project and returns nil. blinky passes both ERC and DRC on
// its own, so a nil return proves the fan-out ran the enabled stages and
// aggregated no error.
func (t *Tests) CiCheckRunsErcAndDrc(ctx context.Context) error {
	err := dag.Kicad().Ci(fixture("blinky")).
		WithErc().
		WithDrc().
		Check(ctx)
	if err != nil {
		return fmt.Errorf("expected a clean Ci.Check for blinky, got: %w", err)
	}
	return nil
}

// CiCheckFailsOnViolations runs Check against the violations fixture with both
// ERC and DRC enabled and asserts the parallel fan-out aggregated BOTH job
// failures rather than short-circuiting on the first. ERC fails with a
// pin_not_connected violation and DRC fails with its "DRC violations" report;
// requiring both signatures proves both jobs ran and both errors propagated.
func (t *Tests) CiCheckFailsOnViolations(ctx context.Context) error {
	err := dag.Kicad().Ci(fixture("violations")).
		WithErc().
		WithDrc().
		Check(ctx)
	if err == nil {
		return fmt.Errorf("expected a Ci.Check failure for the violations fixture, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "pin_not_connected") {
		return fmt.Errorf("expected the ERC violation in the aggregated error, got: %s", msg)
	}
	if !strings.Contains(msg, "DRC violations") {
		return fmt.Errorf("expected the DRC report in the aggregated error, got: %s", msg)
	}
	return nil
}

// CiRunProducesFabricationOutputs runs the full pipeline against the clean
// blinky project — checks then outputs — and asserts Run returns one directory
// holding the whole fabrication package: gerbers/ and drill/ subdirectories,
// plus pos.pos and bom.csv at the root.
func (t *Tests) CiRunProducesFabricationOutputs(ctx context.Context) error {
	out := dag.Kicad().Ci(fixture("blinky")).
		WithErc().
		WithDrc().
		WithFabricationOutputs().
		Run()

	root, err := out.Entries(ctx)
	if err != nil {
		return fmt.Errorf("Ci.Run: %w", err)
	}
	// Directory.Entries lists subdirectories with a trailing slash.
	for _, want := range []string{"gerbers/", "drill/", "pos.pos", "bom.csv"} {
		if !contains(root, want) {
			return fmt.Errorf("expected %s at the root of the fabrication package, got %v", want, root)
		}
	}

	gerbers, err := out.Directory("gerbers").Entries(ctx)
	if err != nil {
		return fmt.Errorf("Ci.Run gerbers/: %w", err)
	}
	if !contains(gerbers, "blinky-F_Cu.gtl") {
		return fmt.Errorf("expected blinky-F_Cu.gtl under gerbers/, got %v", gerbers)
	}

	drill, err := out.Directory("drill").Entries(ctx)
	if err != nil {
		return fmt.Errorf("Ci.Run drill/: %w", err)
	}
	if !contains(drill, "blinky.drl") {
		return fmt.Errorf("expected blinky.drl under drill/, got %v", drill)
	}
	return nil
}

// CiRunShortCircuitsOnFailingCheck asserts a failing check stops the pipeline
// before any output work: Run against the violations fixture with ERC enabled
// and fabrication outputs requested must return the aggregated check error and
// no directory. The error carries the ERC report, proving the failure came
// from stage 1 rather than from an export.
func (t *Tests) CiRunShortCircuitsOnFailingCheck(ctx context.Context) error {
	_, err := dag.Kicad().Ci(fixture("violations")).
		WithErc().
		WithFabricationOutputs().
		Run().
		Sync(ctx)
	if err == nil {
		return fmt.Errorf("expected Ci.Run to short-circuit on the failing check, got nil")
	}
	if !strings.Contains(err.Error(), "pin_not_connected") {
		return fmt.Errorf("expected the failing-check ERC report in the error, got: %v", err)
	}
	return nil
}

// ------------------------------------------- 3D exports, image variant

// ThreeDExportRequiresFullImage asserts a with-models 3D export on the slim
// image fails with an error naming the -full tag, rather than silently emitting
// a board-only model. Glb stands in for the whole step-family here; every one
// of them routes through the same require3DModels guard.
func (t *Tests) ThreeDExportRequiresFullImage(ctx context.Context) error {
	_, err := dag.Kicad().Project(fixture("blinky")).Pcb().Glb().Sync(ctx)
	if err == nil {
		return fmt.Errorf("expected a with-models Glb on the slim image to fail, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"component 3D models", "10.0-full"} {
		if !strings.Contains(msg, want) {
			return fmt.Errorf("expected the guard error to mention %q, got: %v", want, err)
		}
	}
	return nil
}

// StepWithComponentModelsIncludesModels asserts a with-models STEP on the -full
// image differs from the boardOnly output. The blinky R1 footprint references a
// component 3D model that only the -full image bundles, so the populated
// assembly carries geometry the bare board does not — proving the with-models
// path actually resolved and embedded the model rather than falling back to
// board geometry.
func (t *Tests) StepWithComponentModelsIncludesModels(ctx context.Context) error {
	pcb := dag.Kicad(dagger.KicadOpts{Full: true}).Project(fixture("blinky")).Pcb()

	withModels, err := exportBytes(ctx, pcb.Step(), "with-models.step")
	if err != nil {
		return fmt.Errorf("with-models Step: %w", err)
	}
	boardOnly, err := exportBytes(ctx, pcb.Step(dagger.KicadPcbStepOpts{BoardOnly: true}), "board-only.step")
	if err != nil {
		return fmt.Errorf("board-only Step: %w", err)
	}
	if bytes.Equal(withModels, boardOnly) {
		return fmt.Errorf("expected the with-models STEP to differ from the board-only STEP, got identical output")
	}
	if len(withModels) <= len(boardOnly) {
		return fmt.Errorf("expected the with-models STEP (%d bytes) to be larger than the board-only STEP (%d bytes)",
			len(withModels), len(boardOnly))
	}
	if !bytes.HasPrefix(withModels, []byte("ISO-10303-21;")) {
		return fmt.Errorf("expected the with-models output to be a STEP file, got %q", firstBytes(withModels, 13))
	}
	return nil
}

// GlbBoardOnlyProducesGlb asserts the board-only GLB export produces a real
// binary glTF, whose files open with the "glTF" magic.
func (t *Tests) GlbBoardOnlyProducesGlb(ctx context.Context) error {
	f := dag.Kicad().Project(fixture("blinky")).Pcb().Glb(dagger.KicadPcbGlbOpts{BoardOnly: true})
	return assertMagic(ctx, f, "board.glb", []byte("glTF"))
}

// VrmlBoardOnlyProducesVrml asserts the board-only VRML export produces a VRML
// v2.0 document. VRML has no kicad-cli board-only flag, so boardOnly here only
// skips the -full guard; on the slim image the output is board geometry alone.
func (t *Tests) VrmlBoardOnlyProducesVrml(ctx context.Context) error {
	out, err := dag.Kicad().Project(fixture("blinky")).Pcb().
		Vrml(dagger.KicadPcbVrmlOpts{BoardOnly: true}).Contents(ctx)
	if err != nil {
		return fmt.Errorf("Vrml: %w", err)
	}
	if !strings.HasPrefix(out, "#VRML") {
		return fmt.Errorf("expected a VRML document, got:\n%s", head(out))
	}
	return nil
}

// ------------------------------------------- long-tail 2D and utility

// PcbDxfProducesDxf asserts the single-file DXF plot produces a DXF drawing,
// whose ASCII form opens with a SECTION record.
func (t *Tests) PcbDxfProducesDxf(ctx context.Context) error {
	out, err := dag.Kicad().Project(fixture("blinky")).Pcb().
		Dxf([]string{"F.Cu", "Edge.Cuts"}).Contents(ctx)
	if err != nil {
		return fmt.Errorf("Dxf: %w", err)
	}
	if !strings.Contains(out, "SECTION") {
		return fmt.Errorf("expected a DXF drawing, got:\n%s", head(out))
	}
	return nil
}

// PcbPsProducesPostscript asserts the single-file PostScript plot produces a
// document opening with the "%!PS" magic.
func (t *Tests) PcbPsProducesPostscript(ctx context.Context) error {
	f := dag.Kicad().Project(fixture("blinky")).Pcb().Ps([]string{"F.Cu", "Edge.Cuts"})
	return assertMagic(ctx, f, "board.ps", []byte("%!PS"))
}

// StatsProducesReport asserts the board statistics report is produced and reads
// as a human-readable report.
func (t *Tests) StatsProducesReport(ctx context.Context) error {
	out, err := dag.Kicad().Project(fixture("blinky")).Pcb().Stats().Contents(ctx)
	if err != nil {
		return fmt.Errorf("Stats: %w", err)
	}
	if !strings.Contains(out, "PCB statistics") {
		return fmt.Errorf("expected a board statistics report, got:\n%s", head(out))
	}
	return nil
}

// GencadProducesGencad asserts the GenCAD export produces a GenCAD file, which
// opens with a $HEADER section naming the format.
func (t *Tests) GencadProducesGencad(ctx context.Context) error {
	out, err := dag.Kicad().Project(fixture("blinky")).Pcb().Gencad().Contents(ctx)
	if err != nil {
		return fmt.Errorf("Gencad: %w", err)
	}
	if !strings.Contains(out, "$HEADER") || !strings.Contains(out, "GENCAD") {
		return fmt.Errorf("expected a GenCAD document, got:\n%s", head(out))
	}
	return nil
}

// Ipcd356ProducesNetlist asserts the IPC-D-356 export produces a bare-board
// test netlist, whose records carry the format's CODE/UNITS parameters.
func (t *Tests) Ipcd356ProducesNetlist(ctx context.Context) error {
	out, err := dag.Kicad().Project(fixture("blinky")).Pcb().Ipcd356().Contents(ctx)
	if err != nil {
		return fmt.Errorf("Ipcd356: %w", err)
	}
	if !strings.Contains(out, "UNITS") {
		return fmt.Errorf("expected an IPC-D-356 netlist, got:\n%s", head(out))
	}
	return nil
}

// OdbProducesArchive asserts the ODB++ export produces a zip archive, which
// opens with the "PK" local-file-header magic.
func (t *Tests) OdbProducesArchive(ctx context.Context) error {
	f := dag.Kicad().Project(fixture("blinky")).Pcb().Odb()
	return assertMagic(ctx, f, "odb.zip", []byte("PK\x03\x04"))
}

// RenderProducesPng asserts the 3D render produces a PNG image, which opens
// with the PNG signature. On the slim image this is a bare-board render, which
// is a valid PNG all the same.
func (t *Tests) RenderProducesPng(ctx context.Context) error {
	f := dag.Kicad().Project(fixture("blinky")).Pcb().Render()
	return assertMagic(ctx, f, "render.png", []byte("\x89PNG\r\n\x1a\n"))
}

// ImportRejectsUnknownFormat asserts the import format enum is validated,
// listing every format kicad-cli accepts, rather than passed through. Import
// converts a foreign board and needs no real fixture to prove the validation.
func (t *Tests) ImportRejectsUnknownFormat(ctx context.Context) error {
	_, err := dag.Kicad().Project(fixture("blinky")).Pcb().
		Import("foreign.brd", dagger.KicadPcbImportOpts{Format: "verilog"}).Sync(ctx)
	if err == nil {
		return fmt.Errorf("expected an error for an invalid import format, got nil")
	}
	if !strings.Contains(err.Error(), "must be one of") || !strings.Contains(err.Error(), "eagle") {
		return fmt.Errorf("expected the legal format set in the error, got: %v", err)
	}
	return nil
}

// PcbUpgradeProducesBoard asserts the in-place board upgrade returns a resaved
// .kicad_pcb. kicad-cli's upgrade has no output flag and rewrites the file in
// place, so a returned board proves the writable-copy path worked.
func (t *Tests) PcbUpgradeProducesBoard(ctx context.Context) error {
	out, err := dag.Kicad().Project(fixture("blinky")).Pcb().
		Upgrade(dagger.KicadPcbUpgradeOpts{Force: true}).Contents(ctx)
	if err != nil {
		return fmt.Errorf("Upgrade: %w", err)
	}
	if !strings.Contains(out, "(kicad_pcb") {
		return fmt.Errorf("expected an upgraded .kicad_pcb, got:\n%s", head(out))
	}
	return nil
}

// SchDxfProducesFilePerSheet asserts the schematic DXF plot lands one file per
// sheet in the returned directory.
func (t *Tests) SchDxfProducesFilePerSheet(ctx context.Context) error {
	entries, err := dag.Kicad().Project(fixture("blinky")).Sch().Dxf().Entries(ctx)
	if err != nil {
		return fmt.Errorf("Sch.Dxf: %w", err)
	}
	if !contains(entries, "blinky.dxf") {
		return fmt.Errorf("expected blinky.dxf, got %v", entries)
	}
	return nil
}

// SchPsProducesFilePerSheet asserts the schematic PostScript plot lands one
// file per sheet in the returned directory.
func (t *Tests) SchPsProducesFilePerSheet(ctx context.Context) error {
	entries, err := dag.Kicad().Project(fixture("blinky")).Sch().Ps().Entries(ctx)
	if err != nil {
		return fmt.Errorf("Sch.Ps: %w", err)
	}
	if !contains(entries, "blinky.ps") {
		return fmt.Errorf("expected blinky.ps, got %v", entries)
	}
	return nil
}

// --------------------------------------------- fp and sym libraries

// FpSvgExportsFootprint asserts the footprint-library SVG export lands one SVG
// per footprint, named after the footprint, in the returned directory.
func (t *Tests) FpSvgExportsFootprint(ctx context.Context) error {
	entries, err := dag.Kicad().Fp(fixtureDir("fplib/test.pretty")).Svg().Entries(ctx)
	if err != nil {
		return fmt.Errorf("Fp.Svg: %w", err)
	}
	if !contains(entries, "R_0805.svg") {
		return fmt.Errorf("expected R_0805.svg, got %v", entries)
	}
	return nil
}

// FpUpgradeResavesLibrary asserts the footprint-library upgrade returns the
// resaved .pretty directory with its .kicad_mod file in place.
func (t *Tests) FpUpgradeResavesLibrary(ctx context.Context) error {
	entries, err := dag.Kicad().Fp(fixtureDir("fplib/test.pretty")).
		Upgrade(dagger.KicadFpUpgradeOpts{Force: true}).Entries(ctx)
	if err != nil {
		return fmt.Errorf("Fp.Upgrade: %w", err)
	}
	if !contains(entries, "R_0805.kicad_mod") {
		return fmt.Errorf("expected R_0805.kicad_mod, got %v", entries)
	}
	return nil
}

// SymSvgExportsSymbol asserts the symbol-library SVG export lands one SVG per
// symbol unit in the returned directory.
func (t *Tests) SymSvgExportsSymbol(ctx context.Context) error {
	entries, err := dag.Kicad().Sym(fixtureFile("symlib/test.kicad_sym")).Svg().Entries(ctx)
	if err != nil {
		return fmt.Errorf("Sym.Svg: %w", err)
	}
	if !contains(entries, "R_unit1.svg") {
		return fmt.Errorf("expected R_unit1.svg, got %v", entries)
	}
	return nil
}

// SymUpgradeResavesLibrary asserts the symbol-library upgrade returns the
// resaved .kicad_sym file.
func (t *Tests) SymUpgradeResavesLibrary(ctx context.Context) error {
	out, err := dag.Kicad().Sym(fixtureFile("symlib/test.kicad_sym")).
		Upgrade(dagger.KicadSymUpgradeOpts{Force: true}).Contents(ctx)
	if err != nil {
		return fmt.Errorf("Sym.Upgrade: %w", err)
	}
	if !strings.Contains(out, "kicad_symbol_lib") {
		return fmt.Errorf("expected an upgraded .kicad_sym, got:\n%s", head(out))
	}
	return nil
}

// ------------------------------------------------------------------ helpers

// fixture returns the named hand-authored KiCad project under fixtures/.
func fixture(name string) *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/" + name)
}

// fixtureFile returns a single file under fixtures/ by project-relative path,
// for the inputs — like a drawing sheet or a symbol library — that a function
// takes as a lone *dagger.File rather than a project directory.
func fixtureFile(path string) *dagger.File {
	return dag.CurrentModule().Source().File("fixtures/" + path)
}

// fixtureDir returns a subdirectory under fixtures/ by project-relative path,
// for the library inputs (a .pretty footprint library) that are a directory
// but not a whole KiCad project.
func fixtureDir(path string) *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/" + path)
}

// exportBytes exports a file artifact and returns its raw bytes, for the
// assertions that compare whole files rather than just their leading magic.
func exportBytes(ctx context.Context, f *dagger.File, name string) ([]byte, error) {
	dir, err := os.MkdirTemp(".", "kicad-")
	if err != nil {
		return nil, fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, name)
	if _, err := f.Export(ctx, path); err != nil {
		return nil, fmt.Errorf("export %s: %w", name, err)
	}
	return os.ReadFile(path)
}

// assertMagic exports a binary artifact and compares its leading bytes.
// File.Contents() mangles non-UTF-8 data, so binary formats are asserted
// through the filesystem instead.
func assertMagic(ctx context.Context, f *dagger.File, name string, want []byte) error {
	dir, err := os.MkdirTemp(".", "kicad-")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, name)
	if _, err := f.Export(ctx, path); err != nil {
		return fmt.Errorf("export %s: %w", name, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	if !bytes.HasPrefix(got, want) {
		return fmt.Errorf("expected %s to start with %q, got %q", name, want, firstBytes(got, len(want)))
	}
	return nil
}

func contains(entries []string, want string) bool {
	for _, e := range entries {
		if e == want {
			return true
		}
	}
	return false
}

func firstBytes(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[:n]
}

// head trims a long artifact down to something readable in a failure message.
func head(s string) string {
	const limit = 512
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "\n..."
}
