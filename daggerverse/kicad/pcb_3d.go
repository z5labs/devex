package main

import (
	"context"
	"fmt"

	"dagger/kicad/internal/dagger"
)

// The 3D board exports. Every format here resolves the same component 3D
// models, which only the -full image bundles: on the slim image an export that
// includes models would silently emit board-only geometry, so this file guards
// that path (require3DModels) and fails naming the -full tag instead.
//
// step, glb, stl, brep, ply, u3d, xao, stpz and 3dpdf share one flag family
// (--board-only, --no-dnp, --force); export3D renders all of them. vrml is the
// odd one out — kicad-cli's `pcb export vrml` has no --board-only flag — so it
// has its own function below.

// require3DModels guards a with-models 3D export. The slim image ships no 3D
// component models, so kicad-cli would resolve none and quietly write a
// board-only model; this turns that into an actionable error naming the -full
// image, which bundles the model libraries.
func (b *Pcb) require3DModels(format string) error {
	k := b.Project.Kicad
	if k.hasComponentModels() {
		return nil
	}
	return fmt.Errorf(
		"pcb export %s requests component 3D models, but the %q image ships none; "+
			"select the -full image (New with full=true, or a -full tag such as %q), "+
			"or pass boardOnly to export board geometry alone",
		format, k.resolvedTag(), k.resolvedTag()+fullSuffix)
}

// export3D runs one of the step-family 3D exports (every format but vrml). The
// with-models path is gated by require3DModels; boardOnly opts out of models
// and so out of the guard.
func (b *Pcb) export3D(ctx context.Context, format, outputName string, boardOnly, excludeDnp bool) (*dagger.File, error) {
	if err := checkOutputName(outputName); err != nil {
		return nil, err
	}
	if !boardOnly {
		if err := b.require3DModels(format); err != nil {
			return nil, err
		}
	}
	board, ctr, err := b.resolve(ctx)
	if err != nil {
		return nil, err
	}
	out := outputDir + "/" + outputName
	args := []string{"kicad-cli", "pcb", "export", format, "--force", "--output", out}
	args = append(args, b.Project.hoisted(cmdFlags{defineVar: true, variant: true})...)
	if boardOnly {
		args = append(args, "--board-only")
	}
	if excludeDnp {
		args = append(args, "--no-dnp")
	}
	exec, err := runExport(ctx, ctr, "kicad-cli pcb export "+format, append(args, board))
	if err != nil {
		return nil, err
	}
	return exec.File(out), nil
}

// Glb exports the board as a binary glTF (GLB) model. Like every 3D export it
// needs the -full image for component models; pass boardOnly for board
// geometry alone on the slim image.
//
// +cache="session"
func (b *Pcb) Glb(
	ctx context.Context,
	// Export the bare board, with no component models.
	// +default=false
	boardOnly bool,
	// Exclude models for components flagged Do Not Populate.
	// +default=false
	excludeDnp bool,
	// +default="board.glb"
	outputName string,
) (*dagger.File, error) {
	return b.export3D(ctx, "glb", outputName, boardOnly, excludeDnp)
}

// Stl exports the board as an STL mesh.
//
// +cache="session"
func (b *Pcb) Stl(
	ctx context.Context,
	// +default=false
	boardOnly bool,
	// +default=false
	excludeDnp bool,
	// +default="board.stl"
	outputName string,
) (*dagger.File, error) {
	return b.export3D(ctx, "stl", outputName, boardOnly, excludeDnp)
}

// Brep exports the board as an OpenCASCADE BREP model.
//
// +cache="session"
func (b *Pcb) Brep(
	ctx context.Context,
	// +default=false
	boardOnly bool,
	// +default=false
	excludeDnp bool,
	// +default="board.brep"
	outputName string,
) (*dagger.File, error) {
	return b.export3D(ctx, "brep", outputName, boardOnly, excludeDnp)
}

// Ply exports the board as a PLY mesh.
//
// +cache="session"
func (b *Pcb) Ply(
	ctx context.Context,
	// +default=false
	boardOnly bool,
	// +default=false
	excludeDnp bool,
	// +default="board.ply"
	outputName string,
) (*dagger.File, error) {
	return b.export3D(ctx, "ply", outputName, boardOnly, excludeDnp)
}

// U3d exports the board as a Universal 3D (U3D) model.
//
// +cache="session"
func (b *Pcb) U3d(
	ctx context.Context,
	// +default=false
	boardOnly bool,
	// +default=false
	excludeDnp bool,
	// +default="board.u3d"
	outputName string,
) (*dagger.File, error) {
	return b.export3D(ctx, "u3d", outputName, boardOnly, excludeDnp)
}

// Xao exports the board as an XAO model (Salome geometry exchange).
//
// +cache="session"
func (b *Pcb) Xao(
	ctx context.Context,
	// +default=false
	boardOnly bool,
	// +default=false
	excludeDnp bool,
	// +default="board.xao"
	outputName string,
) (*dagger.File, error) {
	return b.export3D(ctx, "xao", outputName, boardOnly, excludeDnp)
}

// Stpz exports the board as a zip-compressed STEP (STPZ) model.
//
// +cache="session"
func (b *Pcb) Stpz(
	ctx context.Context,
	// +default=false
	boardOnly bool,
	// +default=false
	excludeDnp bool,
	// +default="board.stpz"
	outputName string,
) (*dagger.File, error) {
	return b.export3D(ctx, "stpz", outputName, boardOnly, excludeDnp)
}

// Pdf3d exports the board as a 3D PDF (a PDF carrying an embedded U3D model).
// Exposed as `pdf-3d` on the CLI.
//
// +cache="session"
func (b *Pcb) Pdf3d(
	ctx context.Context,
	// +default=false
	boardOnly bool,
	// +default=false
	excludeDnp bool,
	// +default="board-3d.pdf"
	outputName string,
) (*dagger.File, error) {
	return b.export3D(ctx, "3dpdf", outputName, boardOnly, excludeDnp)
}

// Vrml exports the board as a VRML model. Unlike the other 3D exports
// kicad-cli's `pcb export vrml` has no --board-only flag, so boardOnly here is
// a module-level acknowledgement rather than a kicad-cli switch: it gates the
// -full-image guard only. On the slim image no component models resolve, so
// boardOnly=true yields the board geometry alone; on the -full image the
// models are always embedded regardless, which is why boardOnly cannot suppress
// them for VRML.
//
// +cache="session"
func (b *Pcb) Vrml(
	ctx context.Context,
	// Acknowledge board-geometry-only output on the slim image, skipping the
	// -full guard. VRML has no board-only mode, so this cannot exclude models
	// on the -full image.
	// +default=false
	boardOnly bool,
	// Exclude models for components flagged Do Not Populate.
	// +default=false
	excludeDnp bool,
	// Output units: mm, m, in or tenths.
	// +default="mm"
	units string,
	// +default="board.wrl"
	outputName string,
) (*dagger.File, error) {
	if err := oneOf("units", units, "mm", "m", "in", "tenths"); err != nil {
		return nil, err
	}
	if err := checkOutputName(outputName); err != nil {
		return nil, err
	}
	if !boardOnly {
		if err := b.require3DModels("vrml"); err != nil {
			return nil, err
		}
	}
	board, ctr, err := b.resolve(ctx)
	if err != nil {
		return nil, err
	}
	out := outputDir + "/" + outputName
	args := []string{"kicad-cli", "pcb", "export", "vrml", "--force", "--output", out, "--units", units}
	args = append(args, b.Project.hoisted(cmdFlags{defineVar: true, variant: true})...)
	if excludeDnp {
		args = append(args, "--no-dnp")
	}
	exec, err := runExport(ctx, ctr, "kicad-cli pcb export vrml", append(args, board))
	if err != nil {
		return nil, err
	}
	return exec.File(out), nil
}
