package main

import (
	"context"

	"dagger/kicad/internal/dagger"
)

// The footprint (fp) and symbol (sym) library command families. Unlike every
// other command in this module these operate on a library, not on a board or
// schematic within a project, so they attach to Kicad directly rather than to
// Project/Pcb/Sch: Kicad.Fp(dir) binds a .pretty footprint library (a
// directory of .kicad_mod files), Kicad.Sym(file) binds a .kicad_sym symbol
// library (a single file). That split mirrors what the two artifacts actually
// are on disk, and keeps the project-scoped hoisted options (--variant,
// --drawing-sheet) — which these commands do not accept — off their surface.

const (
	// libraryDir is where a footprint library directory is mounted. The
	// .pretty suffix is load-bearing: kicad-cli recognises a footprint library
	// by that extension and silently finds zero footprints without it.
	libraryDir = "/tmp/kicad-lib.pretty"

	// symLibPath is where a symbol library file is mounted.
	symLibPath = "/tmp/kicad-lib.kicad_sym"

	// fpUpgradeDir and symUpgradePath are the output targets for the upgrade
	// commands. kicad-cli refuses to upgrade into an existing path, so these
	// must be fresh locations the container never creates up front.
	fpUpgradeDir   = "/tmp/kicad-fp-upgraded"
	symUpgradePath = "/tmp/kicad-sym-upgraded.kicad_sym"
)

// Fp is a footprint library (a .pretty directory of .kicad_mod files) bound to
// the toolchain.
type Fp struct {
	// +private
	Kicad *Kicad
	// +private
	Source *dagger.Directory
}

// Fp binds a footprint library directory (a .pretty folder, or any directory
// of .kicad_mod files) to the toolchain for the `fp` command family.
func (k *Kicad) Fp(source *dagger.Directory) *Fp {
	return &Fp{Kicad: k, Source: source}
}

// Svg exports the footprint library to SVG, one file per footprint, and
// returns the directory. Pass footprint to export a single footprint by name
// instead of the whole library.
//
// +cache="session"
func (f *Fp) Svg(
	ctx context.Context,
	// Export only this footprint from the library; empty exports all.
	// +default=""
	footprint string,
) (*dagger.Directory, error) {
	ctr := f.Kicad.Container().
		WithMountedDirectory(libraryDir, f.Source).
		WithExec([]string{"mkdir", "-p", outputDir})
	args := []string{"kicad-cli", "fp", "export", "svg", "--output", outputDir + "/"}
	if footprint != "" {
		args = append(args, "--footprint", footprint)
	}
	exec, err := runExport(ctx, ctr, "kicad-cli fp export svg", append(args, libraryDir))
	if err != nil {
		return nil, err
	}
	return exec.Directory(outputDir), nil
}

// Upgrade resaves the footprint library in the current KiCad format and returns
// the upgraded .pretty directory.
//
// +cache="session"
func (f *Fp) Upgrade(
	ctx context.Context,
	// Resave even when the library is already at the latest format version.
	// +default=false
	force bool,
) (*dagger.Directory, error) {
	ctr := f.Kicad.Container().WithMountedDirectory(libraryDir, f.Source)
	args := []string{"kicad-cli", "fp", "upgrade"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, "--output", fpUpgradeDir, libraryDir)
	exec, err := runExport(ctx, ctr, "kicad-cli fp upgrade", args)
	if err != nil {
		return nil, err
	}
	return exec.Directory(fpUpgradeDir), nil
}

// Sym is a symbol library (a .kicad_sym file) bound to the toolchain.
type Sym struct {
	// +private
	Kicad *Kicad
	// +private
	Source *dagger.File
}

// Sym binds a symbol library file (.kicad_sym) to the toolchain for the `sym`
// command family. It takes a lone *File, not a directory, because a symbol
// library is a single self-contained file — the footprint library's on-disk
// counterpart is a directory, which is why Fp takes a *Directory instead.
func (k *Kicad) Sym(source *dagger.File) *Sym {
	return &Sym{Kicad: k, Source: source}
}

// Svg exports the symbol library to SVG, one file per symbol unit, and returns
// the directory. Pass symbol to export a single symbol by name instead of the
// whole library.
//
// +cache="session"
func (s *Sym) Svg(
	ctx context.Context,
	// Export only this symbol from the library; empty exports all.
	// +default=""
	symbol string,
) (*dagger.Directory, error) {
	ctr := s.Kicad.Container().
		WithMountedFile(symLibPath, s.Source).
		WithExec([]string{"mkdir", "-p", outputDir})
	args := []string{"kicad-cli", "sym", "export", "svg", "--output", outputDir + "/"}
	if symbol != "" {
		args = append(args, "--symbol", symbol)
	}
	exec, err := runExport(ctx, ctr, "kicad-cli sym export svg", append(args, symLibPath))
	if err != nil {
		return nil, err
	}
	return exec.Directory(outputDir), nil
}

// Upgrade resaves the symbol library in the current KiCad format and returns
// the upgraded .kicad_sym file.
//
// +cache="session"
func (s *Sym) Upgrade(
	ctx context.Context,
	// Resave even when the library is already at the latest format version.
	// +default=false
	force bool,
) (*dagger.File, error) {
	ctr := s.Kicad.Container().WithMountedFile(symLibPath, s.Source)
	args := []string{"kicad-cli", "sym", "upgrade"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, "--output", symUpgradePath, symLibPath)
	exec, err := runExport(ctx, ctr, "kicad-cli sym upgrade", args)
	if err != nil {
		return nil, err
	}
	return exec.File(symUpgradePath), nil
}
