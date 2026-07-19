package main

import (
	"context"

	"dagger/kicad/internal/dagger"
)

// Sch is a schematic selected within a Project.
type Sch struct {
	// +private
	Project *Project
	// +private
	Path string
}

// Erc runs the Electrical Rule Check and returns a non-nil error listing the
// violations when the schematic fails, nil when it is clean.
//
// Like Pcb.Drc it returns a bare error: Dagger drops a function's value when
// it also returns a non-nil error, so a (report, error) signature would hide
// the violation list on the failure path. `--exit-code-violations` exits 5,
// which Expect=ReturnTypeAny keeps on the value path.
//
// +cache="session"
func (s *Sch) Erc(
	ctx context.Context,
	// Violation levels to report: all, error, warning or exclusions.
	// +default="error"
	severity string,
) error {
	sev, err := severityFlag(severity)
	if err != nil {
		return err
	}
	sheet, ctr, err := s.resolve(ctx)
	if err != nil {
		return err
	}
	args := []string{"kicad-cli", "sch", "erc", "--exit-code-violations", sev, "--output", reportPath}
	args = append(args, s.Project.hoisted(cmdFlags{defineVar: true})...)
	return runCheck(ctx, ctr, "kicad-cli sch erc", append(args, sheet))
}

// Bom exports the Bill of Materials as CSV. The header row is the field list
// verbatim, because fields is always passed through to kicad-cli — the column
// labels follow whatever the caller asked to export.
//
// +cache="session"
func (s *Sch) Bom(
	ctx context.Context,
	// Ordered list of fields to export. Generated fields such as QUANTITY,
	// ITEM_NUMBER and DNP may be used alongside symbol fields.
	// +default="Reference,Value,Footprint,QUANTITY,DNP"
	fields string,
	// Fields to group references by when their values match.
	// +optional
	groupBy string,
	// Field name to sort by.
	// +default="Reference"
	sortField string,
	// Exclude symbols marked Do Not Populate.
	// +default=false
	excludeDnp bool,
	// +default="bom.csv"
	outputName string,
) (*dagger.File, error) {
	if err := checkOutputName(outputName); err != nil {
		return nil, err
	}
	sheet, ctr, err := s.resolve(ctx)
	if err != nil {
		return nil, err
	}
	out := outputDir + "/" + outputName
	args := []string{"kicad-cli", "sch", "export", "bom",
		"--output", out, "--fields", fields, "--sort-field", sortField}
	// `sch export bom` rejects --define-var and --drawing-sheet, so only the
	// variant is hoisted here.
	args = append(args, s.Project.hoisted(cmdFlags{variant: true})...)
	if groupBy != "" {
		args = append(args, "--group-by", groupBy)
	}
	if excludeDnp {
		args = append(args, "--exclude-dnp")
	}
	exec, err := runExport(ctx, ctr, "kicad-cli sch export bom", append(args, sheet))
	if err != nil {
		return nil, err
	}
	return exec.File(out), nil
}

// Netlist exports the schematic's netlist.
//
// +cache="session"
func (s *Sch) Netlist(
	ctx context.Context,
	// Netlist format: kicadsexpr, kicadxml, cadstar, orcadpcb2, spice,
	// spicemodel, pads or allegro.
	// +default="kicadsexpr"
	format string,
	// +default="netlist.net"
	outputName string,
) (*dagger.File, error) {
	if err := oneOf("format", format,
		"kicadsexpr", "kicadxml", "cadstar", "orcadpcb2",
		"spice", "spicemodel", "pads", "allegro"); err != nil {
		return nil, err
	}
	if err := checkOutputName(outputName); err != nil {
		return nil, err
	}
	sheet, ctr, err := s.resolve(ctx)
	if err != nil {
		return nil, err
	}
	out := outputDir + "/" + outputName
	args := []string{"kicad-cli", "sch", "export", "netlist",
		"--output", out, "--format", format}
	args = append(args, s.Project.hoisted(cmdFlags{variant: true})...)
	exec, err := runExport(ctx, ctr, "kicad-cli sch export netlist", append(args, sheet))
	if err != nil {
		return nil, err
	}
	return exec.File(out), nil
}

// Pdf plots the schematic to a single multi-page PDF — one page per sheet of
// a hierarchical design.
//
// +cache="session"
func (s *Sch) Pdf(
	ctx context.Context,
	// +default="schematic.pdf"
	outputName string,
) (*dagger.File, error) {
	if err := checkOutputName(outputName); err != nil {
		return nil, err
	}
	sheet, ctr, err := s.resolve(ctx)
	if err != nil {
		return nil, err
	}
	out := outputDir + "/" + outputName
	args := []string{"kicad-cli", "sch", "export", "pdf", "--output", out}
	args = append(args, s.Project.hoisted(cmdFlags{defineVar: true, variant: true, drawingSheet: true})...)
	exec, err := runExport(ctx, ctr, "kicad-cli sch export pdf", append(args, sheet))
	if err != nil {
		return nil, err
	}
	return exec.File(out), nil
}

// Svg plots the schematic to SVG, one file per sheet, and returns the
// directory. Unlike Pcb.Svg there is no single-file counterpart: kicad-cli
// always plots schematics per sheet.
//
// +cache="session"
func (s *Sch) Svg(ctx context.Context) (*dagger.Directory, error) {
	sheet, ctr, err := s.resolve(ctx)
	if err != nil {
		return nil, err
	}
	args := []string{"kicad-cli", "sch", "export", "svg", "--output", outputDir + "/"}
	args = append(args, s.Project.hoisted(cmdFlags{defineVar: true, variant: true, drawingSheet: true})...)
	exec, err := runExport(ctx, ctr, "kicad-cli sch export svg", append(args, sheet))
	if err != nil {
		return nil, err
	}
	return exec.Directory(outputDir), nil
}

// resolve validates the deferred Project config and turns the (possibly
// empty) schematic path into a concrete one, alongside a prepared container.
func (s *Sch) resolve(ctx context.Context) (string, *dagger.Container, error) {
	if err := s.Project.validate(); err != nil {
		return "", nil, err
	}
	sheet, err := s.Project.discover(ctx, "kicad_sch", s.Path)
	if err != nil {
		return "", nil, err
	}
	return sheet, s.Project.container(), nil
}
