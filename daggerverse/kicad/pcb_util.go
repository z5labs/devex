package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/kicad/internal/dagger"
)

// The long-tail 2D and utility board commands: the exotic and legacy plot
// formats (dxf, ps), the board reports and exchange formats (stats, gencad,
// ipcd356, odb), the 3D render, and the two in-place transforms (import,
// upgrade). `pcb export hpgl` is deliberately absent — it self-reports as
// "No longer supported as of KiCad 10.0".

// Dxf plots the given layers into a single DXF drawing (`--mode-single`).
//
// +cache="session"
func (b *Pcb) Dxf(
	ctx context.Context,
	// Untranslated layer names to plot, e.g. F.Cu, Edge.Cuts.
	layers []string,
	// Output units: mm or in.
	// +default="mm"
	units string,
	// +default="board.dxf"
	outputName string,
) (*dagger.File, error) {
	if len(layers) == 0 {
		return nil, fmt.Errorf("layers is required")
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
	args := []string{"kicad-cli", "pcb", "export", "dxf",
		"--mode-single", "--output", out, "--layers", strings.Join(layers, ","), "--output-units", units}
	args = append(args, b.Project.hoisted(cmdFlags{defineVar: true, variant: true, drawingSheet: true})...)
	exec, err := runExport(ctx, ctr, "kicad-cli pcb export dxf", append(args, board))
	if err != nil {
		return nil, err
	}
	return exec.File(out), nil
}

// Ps plots the given layers into a single PostScript file (`--mode-single`).
//
// +cache="session"
func (b *Pcb) Ps(
	ctx context.Context,
	// Untranslated layer names to plot, e.g. F.Cu, Edge.Cuts.
	layers []string,
	// +default="board.ps"
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
	args := []string{"kicad-cli", "pcb", "export", "ps",
		"--mode-single", "--output", out, "--layers", strings.Join(layers, ",")}
	args = append(args, b.Project.hoisted(cmdFlags{defineVar: true, variant: true, drawingSheet: true})...)
	exec, err := runExport(ctx, ctr, "kicad-cli pcb export ps", append(args, board))
	if err != nil {
		return nil, err
	}
	return exec.File(out), nil
}

// Stats generates a board statistics report (pad, via, track and area counts).
//
// +cache="session"
func (b *Pcb) Stats(
	ctx context.Context,
	// Report format: report (human-readable) or json.
	// +default="report"
	format string,
	// Report units: mm or in.
	// +default="mm"
	units string,
	// +default="stats.txt"
	outputName string,
) (*dagger.File, error) {
	if err := oneOf("format", format, "report", "json"); err != nil {
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
	args := []string{"kicad-cli", "pcb", "export", "stats",
		"--output", out, "--format", format, "--units", units}
	exec, err := runExport(ctx, ctr, "kicad-cli pcb export stats", append(args, board))
	if err != nil {
		return nil, err
	}
	return exec.File(out), nil
}

// Gencad exports the board in GenCAD format — a legacy interchange format still
// used by some test-fixture and assembly houses.
//
// +cache="session"
func (b *Pcb) Gencad(
	ctx context.Context,
	// +default="board.cad"
	outputName string,
) (*dagger.File, error) {
	if err := checkOutputName(outputName); err != nil {
		return nil, err
	}
	board, ctr, err := b.resolve(ctx)
	if err != nil {
		return nil, err
	}
	out := outputDir + "/" + outputName
	args := []string{"kicad-cli", "pcb", "export", "gencad", "--output", out}
	args = append(args, b.Project.hoisted(cmdFlags{defineVar: true})...)
	exec, err := runExport(ctx, ctr, "kicad-cli pcb export gencad", append(args, board))
	if err != nil {
		return nil, err
	}
	return exec.File(out), nil
}

// Ipcd356 generates an IPC-D-356 netlist file, the bare-board electrical test
// format a fab house uses to flying-probe an unpopulated board.
//
// +cache="session"
func (b *Pcb) Ipcd356(
	ctx context.Context,
	// +default="board.d356"
	outputName string,
) (*dagger.File, error) {
	if err := checkOutputName(outputName); err != nil {
		return nil, err
	}
	board, ctr, err := b.resolve(ctx)
	if err != nil {
		return nil, err
	}
	out := outputDir + "/" + outputName
	args := []string{"kicad-cli", "pcb", "export", "ipcd356", "--output", out}
	exec, err := runExport(ctx, ctr, "kicad-cli pcb export ipcd356", append(args, board))
	if err != nil {
		return nil, err
	}
	return exec.File(out), nil
}

// Odb exports the board in ODB++ format as a single compressed archive — the
// fabrication data package many modern fab houses prefer over Gerbers.
//
// +cache="session"
func (b *Pcb) Odb(
	ctx context.Context,
	// Archive compression: zip, tgz or none. none writes an uncompressed
	// directory tree rather than a single file.
	// +default="zip"
	compression string,
	// Units: mm or in.
	// +default="mm"
	units string,
	// +default="odb.zip"
	outputName string,
) (*dagger.File, error) {
	if err := oneOf("compression", compression, "zip", "tgz", "none"); err != nil {
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
	args := []string{"kicad-cli", "pcb", "export", "odb",
		"--output", out, "--compression", compression, "--units", units}
	args = append(args, b.Project.hoisted(cmdFlags{defineVar: true, variant: true, drawingSheet: true})...)
	exec, err := runExport(ctx, ctr, "kicad-cli pcb export odb", append(args, board))
	if err != nil {
		return nil, err
	}
	return exec.File(out), nil
}

// Render ray-traces the board's 3D view to a PNG or JPEG image; the output
// format follows the outputName extension. Like the 3D model exports it is
// only meaningful with component models, so it wants the -full image — but
// unlike them it degrades to a bare-board render on the slim image rather than
// failing, because a board-only render is still a useful artifact.
//
// +cache="session"
func (b *Pcb) Render(
	ctx context.Context,
	// Camera side: top, bottom, left, right, front or back.
	// +default="top"
	side string,
	// Render quality: basic, high, user or job_settings.
	// +default="basic"
	quality string,
	// Image width in pixels.
	// +default=1600
	width int,
	// Image height in pixels.
	// +default=900
	height int,
	// +default="render.png"
	outputName string,
) (*dagger.File, error) {
	if err := oneOf("side", side, "top", "bottom", "left", "right", "front", "back"); err != nil {
		return nil, err
	}
	if err := oneOf("quality", quality, "basic", "high", "user", "job_settings"); err != nil {
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
	args := []string{"kicad-cli", "pcb", "render",
		"--output", out, "--side", side, "--quality", quality,
		"--width", fmt.Sprint(width), "--height", fmt.Sprint(height)}
	args = append(args, b.Project.hoisted(cmdFlags{defineVar: true, variant: true})...)
	exec, err := runExport(ctx, ctr, "kicad-cli pcb render", append(args, board))
	if err != nil {
		return nil, err
	}
	return exec.File(out), nil
}

// Import converts a non-KiCad board file into KiCad format and returns the
// produced .kicad_pcb. inputPath names the foreign file within the project
// source; unlike the export commands it is not auto-discovered, since it is not
// a .kicad_pcb and the board selected by Pcb() is irrelevant here.
//
// +cache="session"
func (b *Pcb) Import(
	ctx context.Context,
	// Project-relative path to the non-KiCad board file to convert.
	inputPath string,
	// Input format hint: auto, pads, altium, eagle, cadstar, fabmaster, pcad
	// or solidworks.
	// +default="auto"
	format string,
	// +default="imported.kicad_pcb"
	outputName string,
) (*dagger.File, error) {
	if err := oneOf("format", format,
		"auto", "pads", "altium", "eagle", "cadstar", "fabmaster", "pcad", "solidworks"); err != nil {
		return nil, err
	}
	if err := checkOutputName(outputName); err != nil {
		return nil, err
	}
	if strings.TrimSpace(inputPath) == "" {
		return nil, fmt.Errorf("inputPath is required")
	}
	if err := b.Project.validate(); err != nil {
		return nil, err
	}
	if err := b.Project.requireFile(ctx, inputPath, "input board"); err != nil {
		return nil, err
	}
	out := outputDir + "/" + outputName
	args := []string{"kicad-cli", "pcb", "import",
		"--output", out, "--format", format, inputPath}
	exec, err := runExport(ctx, b.Project.container(), "kicad-cli pcb import", args)
	if err != nil {
		return nil, err
	}
	return exec.File(out), nil
}

// Upgrade resaves the board in the current KiCad file format and returns the
// upgraded .kicad_pcb. kicad-cli's `pcb upgrade` rewrites the file in place and
// has no output flag, so the board runs against a writable copy (owned by the
// image's UID-1000 user) rather than the read-only mounted source.
//
// +cache="session"
func (b *Pcb) Upgrade(
	ctx context.Context,
	// Resave even when the board is already at the latest format version.
	// +default=false
	force bool,
) (*dagger.File, error) {
	if err := b.Project.validate(); err != nil {
		return nil, err
	}
	board, err := b.Project.discover(ctx, "kicad_pcb", b.Path)
	if err != nil {
		return nil, err
	}
	const workDir = "/tmp/kicad-upgrade"
	args := []string{"kicad-cli", "pcb", "upgrade"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, board)
	exec, err := runExport(ctx,
		b.Project.Kicad.Container().
			WithDirectory(workDir, b.Project.Source, dagger.ContainerWithDirectoryOpts{Owner: kicadUser}).
			WithWorkdir(workDir),
		"kicad-cli pcb upgrade", args)
	if err != nil {
		return nil, err
	}
	return exec.File(workDir + "/" + board), nil
}
