package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/kicad/internal/dagger"
)

// Pcb is a board selected within a Project. Every method here execs
// kicad-cli, so each carries a session cache directive; selecting the board
// itself is pure config and carries none.
type Pcb struct {
	// +private
	Project *Project
	// +private
	Path string
}

// Drc runs the Design Rule Check and returns a non-nil error listing the
// violations when the board fails, nil when it is clean.
//
// It returns a bare error rather than (report, error) because Dagger drops a
// function's value whenever it also returns a non-nil error: a
// report-returning signature would leave the violation list unreachable on
// exactly the failure path that needs it. `--exit-code-violations` exits 5 on
// violations, which Expect=ReturnTypeAny keeps on the value path so the
// report can be read back and folded into the message.
//
// +cache="session"
func (b *Pcb) Drc(
	ctx context.Context,
	// Violation levels to report: all, error, warning or exclusions.
	// +default="error"
	severity string,
	// Also check the board against the schematic (footprints, nets, values).
	// +default=false
	schematicParity bool,
	// Refill copper zones before checking.
	// +default=false
	refillZones bool,
) error {
	sev, err := severityFlag(severity)
	if err != nil {
		return err
	}
	board, ctr, err := b.resolve(ctx)
	if err != nil {
		return err
	}
	args := []string{"kicad-cli", "pcb", "drc", "--exit-code-violations", sev, "--output", reportPath}
	args = append(args, b.Project.hoisted(cmdFlags{defineVar: true})...)
	if schematicParity {
		args = append(args, "--schematic-parity")
	}
	if refillZones {
		args = append(args, "--refill-zones")
	}
	return runCheck(ctx, ctr, "kicad-cli pcb drc", append(args, board))
}

// Gerbers plots the board's Gerber files and returns them as a directory. An
// empty layers list plots every layer the board defines, plus the .gbrjob
// file that ties them together.
//
// +cache="session"
func (b *Pcb) Gerbers(
	ctx context.Context,
	// Untranslated layer names to plot, e.g. F.Cu, B.Cu. Empty plots all.
	// +optional
	layers []string,
	// Gerber coordinate precision: 5 or 6.
	// +default=6
	precision int,
	// Check and refill zones before plotting.
	// +default=false
	checkZones bool,
) (*dagger.Directory, error) {
	if precision != 5 && precision != 6 {
		return nil, fmt.Errorf("invalid precision %d: must be one of 5, 6", precision)
	}
	board, ctr, err := b.resolve(ctx)
	if err != nil {
		return nil, err
	}
	args := []string{"kicad-cli", "pcb", "export", "gerbers",
		"--output", outputDir + "/", "--precision", fmt.Sprint(precision)}
	args = append(args, b.Project.hoisted(cmdFlags{defineVar: true, variant: true, drawingSheet: true})...)
	if len(layers) > 0 {
		args = append(args, "--layers", strings.Join(layers, ","))
	}
	if checkZones {
		args = append(args, "--check-zones")
	}
	exec, err := runExport(ctx, ctr, "kicad-cli pcb export gerbers", append(args, board))
	if err != nil {
		return nil, err
	}
	return exec.Directory(outputDir), nil
}

// Drill generates the board's drill files and returns them as a directory.
//
// +cache="session"
func (b *Pcb) Drill(
	ctx context.Context,
	// Drill file format: excellon or gerber.
	// +default="excellon"
	format string,
	// Excellon output units: mm or in.
	// +default="mm"
	units string,
	// Drill origin: absolute or plot.
	// +default="absolute"
	origin string,
	// Emit independent files for plated and non-plated holes.
	// +default=false
	separatePlatedHoles bool,
	// Also emit a drill map.
	// +default=false
	generateMap bool,
) (*dagger.Directory, error) {
	if err := oneOf("format", format, "excellon", "gerber"); err != nil {
		return nil, err
	}
	if err := oneOf("units", units, "mm", "in"); err != nil {
		return nil, err
	}
	if err := oneOf("origin", origin, "absolute", "plot"); err != nil {
		return nil, err
	}
	board, ctr, err := b.resolve(ctx)
	if err != nil {
		return nil, err
	}
	args := []string{"kicad-cli", "pcb", "export", "drill",
		"--output", outputDir + "/",
		"--format", format,
		"--excellon-units", units,
		"--drill-origin", origin,
	}
	if separatePlatedHoles {
		args = append(args, "--excellon-separate-th")
	}
	if generateMap {
		args = append(args, "--generate-map")
	}
	exec, err := runExport(ctx, ctr, "kicad-cli pcb export drill", append(args, board))
	if err != nil {
		return nil, err
	}
	return exec.Directory(outputDir), nil
}

// Pos generates the component position (pick-and-place) file.
//
// +cache="session"
func (b *Pcb) Pos(
	ctx context.Context,
	// Board side to include: front, back or both.
	// +default="both"
	side string,
	// Output format: ascii, csv or gerber.
	// +default="ascii"
	format string,
	// Output units for the ascii and csv formats: in or mm.
	// +default="in"
	units string,
	// Include only SMD footprints.
	// +default=false
	smdOnly bool,
	// Name of the produced file. Named outputName, not output, because -o
	// collides with the Dagger CLI's own top-level flag.
	// +default="pos.pos"
	outputName string,
) (*dagger.File, error) {
	if err := oneOf("side", side, "front", "back", "both"); err != nil {
		return nil, err
	}
	if err := oneOf("format", format, "ascii", "csv", "gerber"); err != nil {
		return nil, err
	}
	if err := oneOf("units", units, "in", "mm"); err != nil {
		return nil, err
	}
	if err := checkOutputName(outputName); err != nil {
		return nil, err
	}
	board, ctr, err := b.resolve(ctx)
	if err != nil {
		return nil, err
	}
	out := outputDir + "/" + outputName
	args := []string{"kicad-cli", "pcb", "export", "pos",
		"--output", out, "--side", side, "--format", format, "--units", units}
	args = append(args, b.Project.hoisted(cmdFlags{variant: true})...)
	if smdOnly {
		args = append(args, "--smd-only")
	}
	exec, err := runExport(ctx, ctr, "kicad-cli pcb export pos", append(args, board))
	if err != nil {
		return nil, err
	}
	return exec.File(out), nil
}

// Step exports the board as a STEP model. The default `10.0` image is the
// slim variant, which ships no 3D component models: a with-models export
// (boardOnly=false) fails there naming the -full image, rather than silently
// emitting a board-only model. Pass boardOnly for board geometry alone, or
// select the -full image for populated assemblies.
//
// +cache="session"
func (b *Pcb) Step(
	ctx context.Context,
	// Export the bare board, with no component models.
	// +default=false
	boardOnly bool,
	// Exclude models for components flagged Do Not Populate.
	// +default=false
	excludeDnp bool,
	// +default="board.step"
	outputName string,
) (*dagger.File, error) {
	return b.export3D(ctx, "step", outputName, boardOnly, excludeDnp)
}

// Ipc2581 exports the board in IPC-2581 format — a single XML file carrying
// the fabrication and assembly data that would otherwise be spread across
// Gerbers, drill files and a BOM.
//
// +cache="session"
func (b *Pcb) Ipc2581(
	ctx context.Context,
	// IPC-2581 standard revision: B or C.
	// +default="C"
	version string,
	// Units: mm or in.
	// +default="mm"
	units string,
	// +default="board.xml"
	outputName string,
) (*dagger.File, error) {
	if err := oneOf("version", version, "B", "C"); err != nil {
		return nil, err
	}
	if err := oneOf("units", units, "mm", "in"); err != nil {
		return nil, err
	}
	if err := checkOutputName(outputName); err != nil {
		return nil, err
	}
	board, ctr, err := b.resolve(ctx)
	if err != nil {
		return nil, err
	}
	out := outputDir + "/" + outputName
	args := []string{"kicad-cli", "pcb", "export", "ipc2581",
		"--output", out, "--version", version, "--units", units}
	args = append(args, b.Project.hoisted(cmdFlags{defineVar: true, variant: true, drawingSheet: true})...)
	exec, err := runExport(ctx, ctr, "kicad-cli pcb export ipc2581", append(args, board))
	if err != nil {
		return nil, err
	}
	return exec.File(out), nil
}

// Pdf plots the given layers into a single PDF.
//
// Pdf and PdfPerLayer are split into file- and directory-returning functions
// rather than one function taking a mode flag: a Dagger function has exactly
// one return type, so modelling `--mode-single` vs `--mode-separate` as a
// parameter would force *Directory onto the common single-file case.
//
// +cache="session"
func (b *Pcb) Pdf(
	ctx context.Context,
	// Untranslated layer names to plot, e.g. F.Cu, Edge.Cuts.
	layers []string,
	// +default="board.pdf"
	outputName string,
) (*dagger.File, error) {
	if len(layers) == 0 {
		return nil, fmt.Errorf("layers is required")
	}
	if err := checkOutputName(outputName); err != nil {
		return nil, err
	}
	board, ctr, err := b.resolve(ctx)
	if err != nil {
		return nil, err
	}
	out := outputDir + "/" + outputName
	args := []string{"kicad-cli", "pcb", "export", "pdf",
		"--mode-single", "--output", out, "--layers", strings.Join(layers, ",")}
	args = append(args, b.Project.hoisted(cmdFlags{defineVar: true, variant: true, drawingSheet: true})...)
	exec, err := runExport(ctx, ctr, "kicad-cli pcb export pdf", append(args, board))
	if err != nil {
		return nil, err
	}
	return exec.File(out), nil
}

// PdfPerLayer plots each layer into its own PDF and returns the directory.
//
// +cache="session"
func (b *Pcb) PdfPerLayer(
	ctx context.Context,
	// Untranslated layer names to plot, one PDF each.
	layers []string,
) (*dagger.Directory, error) {
	if len(layers) == 0 {
		return nil, fmt.Errorf("layers is required")
	}
	board, ctr, err := b.resolve(ctx)
	if err != nil {
		return nil, err
	}
	args := []string{"kicad-cli", "pcb", "export", "pdf",
		"--mode-separate", "--output", outputDir + "/", "--layers", strings.Join(layers, ",")}
	args = append(args, b.Project.hoisted(cmdFlags{defineVar: true, variant: true, drawingSheet: true})...)
	exec, err := runExport(ctx, ctr, "kicad-cli pcb export pdf", append(args, board))
	if err != nil {
		return nil, err
	}
	return exec.Directory(outputDir), nil
}

// Svg plots the given layers into a single SVG.
//
// +cache="session"
func (b *Pcb) Svg(
	ctx context.Context,
	// Untranslated layer names to plot, e.g. F.Cu, Edge.Cuts.
	layers []string,
	// +default="board.svg"
	outputName string,
) (*dagger.File, error) {
	if len(layers) == 0 {
		return nil, fmt.Errorf("layers is required")
	}
	if err := checkOutputName(outputName); err != nil {
		return nil, err
	}
	board, ctr, err := b.resolve(ctx)
	if err != nil {
		return nil, err
	}
	out := outputDir + "/" + outputName
	args := []string{"kicad-cli", "pcb", "export", "svg",
		"--mode-single", "--output", out, "--layers", strings.Join(layers, ",")}
	args = append(args, b.Project.hoisted(cmdFlags{defineVar: true, variant: true, drawingSheet: true})...)
	exec, err := runExport(ctx, ctr, "kicad-cli pcb export svg", append(args, board))
	if err != nil {
		return nil, err
	}
	return exec.File(out), nil
}

// SvgPerLayer plots each layer into its own SVG and returns the directory.
//
// +cache="session"
func (b *Pcb) SvgPerLayer(
	ctx context.Context,
	// Untranslated layer names to plot, one SVG each.
	layers []string,
) (*dagger.Directory, error) {
	if len(layers) == 0 {
		return nil, fmt.Errorf("layers is required")
	}
	board, ctr, err := b.resolve(ctx)
	if err != nil {
		return nil, err
	}
	args := []string{"kicad-cli", "pcb", "export", "svg",
		"--mode-multi", "--output", outputDir + "/", "--layers", strings.Join(layers, ",")}
	args = append(args, b.Project.hoisted(cmdFlags{defineVar: true, variant: true, drawingSheet: true})...)
	exec, err := runExport(ctx, ctr, "kicad-cli pcb export svg", append(args, board))
	if err != nil {
		return nil, err
	}
	return exec.Directory(outputDir), nil
}

// resolve validates the deferred Project config and turns the (possibly
// empty) board path into a concrete one, alongside a prepared container.
func (b *Pcb) resolve(ctx context.Context) (string, *dagger.Container, error) {
	if err := b.Project.validate(); err != nil {
		return "", nil, err
	}
	if err := b.Project.validateVariant(ctx); err != nil {
		return "", nil, err
	}
	board, err := b.Project.discover(ctx, "kicad_pcb", b.Path)
	if err != nil {
		return "", nil, err
	}
	return board, b.Project.container(), nil
}
