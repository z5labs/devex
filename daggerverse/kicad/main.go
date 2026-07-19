// Package main implements the kicad Dagger module: a wrapper around
// `kicad-cli` from the official kicad/kicad image, so a hardware project's
// design-rule checks and fabrication outputs become `dagger call`s instead of
// the usual pile of shell scripts and Makefile recipes.
//
// Everything kicad-cli does is headless — no Xvfb, no display, including
// renders — so the image runs unmodified. It runs as `USER kicad` (UID 1000)
// with no entrypoint, which is why every output is written under /tmp rather
// than into the mounted (root-owned) project.
//
// The boundary input is a *dagger.Directory, not a lone *dagger.File:
// kicad-cli resolves sub-sheets, footprint libraries and drawing sheets
// relative to the project. Project hoists the options that apply to nearly
// every subcommand (--define-var, --variant, --drawing-sheet) into chained
// modifiers rather than repeating them across a dozen signatures.
package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"dagger/kicad/internal/dagger"
)

const (
	// kicadImagePath is the repository under the configured registry. The
	// default `10.0` tag is the slim CI image (~770MB); the `-full` variant
	// bundles 3D component models and is a follow-up.
	kicadImagePath  = "kicad/kicad"
	defaultRegistry = "docker.io"
	defaultTag      = "10.0"

	// projectDir is where the caller's source is mounted. It is root-owned
	// and the container runs as UID 1000, so it is read-only in practice.
	projectDir = "/project"

	// outputDir is the writable staging area every export writes into.
	outputDir = "/tmp/kicad-out"

	// reportPath is where drc/erc write their report so it can be read back
	// and folded into the returned error.
	reportPath = "/tmp/kicad-report.txt"

	// drawingSheetPath is where WithDrawingSheet's file is mounted.
	drawingSheetPath = "/tmp/drawing-sheet.kicad_wks"

	// violationsExitCode is what `--exit-code-violations` returns when drc or
	// erc found something.
	violationsExitCode = 5

	// projectFileExt is the KiCad project file extension, whose stem names
	// the project's root schematic and board.
	projectFileExt = "kicad_pro"

	// kicadUser is the image's unprivileged user, which every exec runs as.
	kicadUser = "kicad"
)

// Kicad wraps kicad-cli as Dagger functions. Construct via New(); call
// Container() for the raw image, or Project(source) to reach the typed
// pcb/sch helpers.
type Kicad struct {
	// +private
	Registry string
	// +private
	Tag string
}

// New returns a Kicad module backed by <registry>/kicad/kicad:<tag>.
func New(
	// Container registry hosting the kicad/kicad image.
	// +default="docker.io"
	registry string,
	// Image tag for kicad/kicad.
	// +default="10.0"
	tag string,
) *Kicad {
	if registry == "" {
		registry = defaultRegistry
	}
	if tag == "" {
		tag = defaultTag
	}
	return &Kicad{Registry: registry, Tag: tag}
}

// Container returns the bare kicad image. This is the escape hatch for every
// subcommand this module does not wrap — kicad-cli's long tail of exotic and
// legacy exports stays reachable via `container with-exec`.
//
// +cache="session"
func (k *Kicad) Container() *dagger.Container {
	return dag.Container().From(k.image())
}

// Version returns the KiCad release the pinned image ships, as reported by
// `kicad-cli version`.
//
// +cache="session"
func (k *Kicad) Version(ctx context.Context) (string, error) {
	out, err := k.Container().
		WithExec([]string{"kicad-cli", "version"}).
		Stdout(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Project binds a KiCad project directory to the toolchain. source is the
// whole project tree, not a single file, because kicad-cli resolves
// sub-sheets, footprint libraries and drawing sheets relative to it.
func (k *Kicad) Project(source *dagger.Directory) *Project {
	return &Project{Kicad: k, Source: source}
}

func (k *Kicad) image() string {
	return fmt.Sprintf("%s/%s:%s", k.Registry, kicadImagePath, k.Tag)
}

// Project is a KiCad project tree plus the options that apply to nearly every
// kicad-cli subcommand. It is immutable: every With* returns a copy.
type Project struct {
	// +private
	Kicad *Kicad
	// +private
	Source *dagger.Directory
	// +private
	VarNames []string
	// +private
	VarValues []string
	// +private
	Variant string
	// +private
	DrawingSheet *dagger.File
}

// WithVar sets a KiCad text variable, overriding or adding to the ones the
// project file declares (kicad-cli's `--define-var name=value`).
//
// It takes a name and a value rather than a map because Dagger functions
// cannot accept map parameters. Validation is deferred to the exec: builder
// methods have no error return, so a bad name surfaces when the export or
// check that would have used it runs.
func (p *Project) WithVar(name string, value string) *Project {
	out := p.clone()
	out.VarNames = append(out.VarNames, name)
	out.VarValues = append(out.VarValues, value)
	return out
}

// WithVariant selects a KiCad assembly variant (`--variant`). It applies to
// the exports that support variants; checks (drc, erc) and drill files ignore
// it because kicad-cli does not accept the flag there.
func (p *Project) WithVariant(variant string) *Project {
	out := p.clone()
	out.Variant = variant
	return out
}

// WithDrawingSheet overrides the project's drawing sheet with the supplied
// .kicad_wks file (`--drawing-sheet`). It applies to the plotting exports;
// subcommands that do not accept the flag ignore it.
func (p *Project) WithDrawingSheet(sheet *dagger.File) *Project {
	out := p.clone()
	out.DrawingSheet = sheet
	return out
}

// Pcb selects a board within the project. An empty path auto-discovers the
// single *.kicad_pcb in the tree and errors when there are zero or more than
// one; discovery is deferred to the exec so the error surfaces on the call
// that needed the board.
func (p *Project) Pcb(
	// Project-relative path to the .kicad_pcb; empty auto-discovers.
	// +default=""
	path string,
) *Pcb {
	return &Pcb{Project: p, Path: path}
}

// Sch selects a schematic within the project. An empty path auto-discovers
// the single *.kicad_sch in the tree, ignoring the sub-sheets of a
// hierarchical design, and errors when there are zero or more than one root.
func (p *Project) Sch(
	// Project-relative path to the .kicad_sch; empty auto-discovers.
	// +default=""
	path string,
) *Sch {
	return &Sch{Project: p, Path: path}
}

// Jobset runs a .kicad_jobset file and returns the project directory with
// everything the jobset produced, so a project that already declares its
// output set in-repo can generate the whole fabrication package in one call.
//
// The jobset's outputs are written relative to the project, which is why the
// whole tree comes back rather than a lone output folder: the jobset — not
// this module — decides where its artifacts land.
//
// +cache="session"
func (p *Project) Jobset(ctx context.Context, path string) (*dagger.Directory, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("jobset path is required")
	}
	if err := p.requireFile(ctx, path, "jobset file"); err != nil {
		return nil, err
	}
	pro, err := p.discover(ctx, projectFileExt, "")
	if err != nil {
		return nil, err
	}

	// The jobset writes its outputs next to the project, so it runs against a
	// writable copy rather than the mounted source. The copy is owned by the
	// image's UID 1000 user: a root-owned one would leave kicad-cli unable to
	// create the output folders the jobset declares.
	const workDir = "/tmp/kicad-jobset"
	exec := p.Kicad.Container().
		WithDirectory(workDir, p.Source, dagger.ContainerWithDirectoryOpts{Owner: kicadUser}).
		WithWorkdir(workDir).
		WithExec(
			[]string{"kicad-cli", "jobset", "run", "--file", path, pro},
			dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny},
		)
	code, err := exec.ExitCode(ctx)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("kicad-cli jobset run %s failed (exit %d):\n%s", path, code, combinedOutput(ctx, exec))
	}
	return exec.Directory(workDir), nil
}

func (p *Project) clone() *Project {
	out := *p
	out.VarNames = append([]string(nil), p.VarNames...)
	out.VarValues = append([]string(nil), p.VarValues...)
	return &out
}

// validate reports the deferred WithVar validation. `--define-var` takes
// `name=value`, so an empty name or one containing `=` would silently produce
// a different variable than the caller asked for.
func (p *Project) validate() error {
	for _, name := range p.VarNames {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("WithVar: variable name is required")
		}
		if strings.Contains(name, "=") {
			return fmt.Errorf("WithVar: variable name %q must not contain %q", name, "=")
		}
	}
	return nil
}

// container mounts the project read-only and stages a writable output dir.
func (p *Project) container() *dagger.Container {
	ctr := p.Kicad.Container().
		WithMountedDirectory(projectDir, p.Source).
		WithWorkdir(projectDir).
		WithExec([]string{"mkdir", "-p", outputDir})
	if p.DrawingSheet != nil {
		ctr = ctr.WithMountedFile(drawingSheetPath, p.DrawingSheet)
	}
	return ctr
}

// cmdFlags describes which of the hoisted Project options a given kicad-cli
// subcommand actually accepts. `sch export bom`, `sch export netlist`,
// `pcb export pos` and `pcb export drill` reject `--define-var`, for example,
// so passing it unconditionally would turn a valid call into a usage error.
type cmdFlags struct {
	defineVar    bool
	variant      bool
	drawingSheet bool
}

// hoisted renders the Project-level options the subcommand supports.
func (p *Project) hoisted(f cmdFlags) []string {
	var args []string
	if f.defineVar {
		for i, name := range p.VarNames {
			args = append(args, "--define-var", name+"="+p.VarValues[i])
		}
	}
	if f.variant && p.Variant != "" {
		args = append(args, "--variant", p.Variant)
	}
	if f.drawingSheet && p.DrawingSheet != nil {
		args = append(args, "--drawing-sheet", drawingSheetPath)
	}
	return args
}

// discover resolves an explicit project-relative path, or finds the project's
// file with the given extension when path is empty.
//
// A unique match wins outright. When several files share the extension —
// which is the normal shape of a hierarchical schematic, whose sub-sheets are
// *.kicad_sch too — the one named after the project file is the root, exactly
// as KiCad itself treats it. Anything still ambiguous is an error naming the
// candidates rather than an arbitrary pick.
func (p *Project) discover(ctx context.Context, ext string, explicit string) (string, error) {
	if explicit != "" {
		if err := p.requireFile(ctx, explicit, "."+ext); err != nil {
			return "", err
		}
		return explicit, nil
	}
	matches, err := p.Source.Glob(ctx, "**/*."+ext)
	if err != nil {
		return "", fmt.Errorf("search project for *.%s: %w", ext, err)
	}
	sort.Strings(matches)
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("no *.%s found in project; pass an explicit path", ext)
	}
	if ext != projectFileExt {
		if stem, ok := p.projectStem(ctx); ok {
			for _, m := range matches {
				if m == stem+"."+ext {
					return m, nil
				}
			}
		}
	}
	return "", fmt.Errorf(
		"project contains %d *.%s files (%s); pass an explicit path to pick one",
		len(matches), ext, strings.Join(matches, ", "))
}

// projectStem returns the single .kicad_pro's path with its extension
// stripped, which is the stem KiCad gives the project's root schematic and
// board. It reports false when there is no unique project file to key off.
func (p *Project) projectStem(ctx context.Context) (string, bool) {
	matches, err := p.Source.Glob(ctx, "**/*."+projectFileExt)
	if err != nil || len(matches) != 1 {
		return "", false
	}
	return strings.TrimSuffix(matches[0], "."+projectFileExt), true
}

// requireFile turns "path not in the tree" into an error naming the path,
// rather than letting kicad-cli fail with its own less specific message.
func (p *Project) requireFile(ctx context.Context, name string, what string) error {
	matches, err := p.Source.Glob(ctx, name)
	if err != nil {
		return fmt.Errorf("look up %s %q: %w", what, name, err)
	}
	for _, m := range matches {
		if m == name {
			return nil
		}
	}
	return fmt.Errorf("%s %q not found in project", what, name)
}

// oneOf validates an enum-ish parameter, listing the legal set on failure.
func oneOf(param string, value string, allowed ...string) error {
	for _, a := range allowed {
		if value == a {
			return nil
		}
	}
	return fmt.Errorf("invalid %s %q: must be one of %s", param, value, strings.Join(allowed, ", "))
}

// checkOutputName rejects a path separator in an artifact name. Outputs are
// staged in a module-owned directory, so a name that walks out of it would
// silently land somewhere the caller never sees.
func checkOutputName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("outputName is required")
	}
	if strings.ContainsRune(name, '/') {
		return fmt.Errorf("invalid outputName %q: must be a file name, not a path (no %q)", name, "/")
	}
	return nil
}

// severityFlag maps the severity parameter onto kicad-cli's per-level flags.
// kicad-cli has no `--severity <level>`; it has one boolean flag per level.
func severityFlag(severity string) (string, error) {
	switch severity {
	case "all":
		return "--severity-all", nil
	case "error":
		return "--severity-error", nil
	case "warning":
		return "--severity-warning", nil
	case "exclusions":
		return "--severity-exclusions", nil
	default:
		return "", fmt.Errorf(
			"invalid severity %q: must be one of all, error, warning, exclusions", severity)
	}
}

// runCheck executes a drc/erc invocation and folds its report into the error.
//
// The report goes to a file rather than stdout because kicad-cli only prints a
// violation *count* to stdout; the actual violation list is what makes a
// failing check actionable. Expect=ReturnTypeAny keeps exit 5
// (--exit-code-violations) on the value path so the report is still readable.
func runCheck(ctx context.Context, ctr *dagger.Container, label string, args []string) error {
	exec := ctr.WithExec(args, dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny})
	code, err := exec.ExitCode(ctx)
	if err != nil {
		return err
	}
	if code == 0 {
		return nil
	}
	out := combinedOutput(ctx, exec)
	if code == violationsExitCode {
		report, reportErr := exec.File(reportPath).Contents(ctx)
		if reportErr != nil {
			report = strings.TrimSpace(out)
		}
		return fmt.Errorf("%s found violations:\n%s", label, strings.TrimSpace(report))
	}
	return fmt.Errorf("%s failed (exit %d):\n%s", label, code, strings.TrimSpace(out))
}

// runExport executes an export and returns the container holding its output,
// turning a non-zero exit into an error that carries kicad-cli's own message.
func runExport(ctx context.Context, ctr *dagger.Container, label string, args []string) (*dagger.Container, error) {
	exec := ctr.WithExec(args, dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny})
	code, err := exec.ExitCode(ctx)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("%s failed (exit %d):\n%s", label, code, combinedOutput(ctx, exec))
	}
	return exec, nil
}

// combinedOutput joins a finished exec's stdout and stderr. kicad-cli splits
// usage errors onto stderr and progress onto stdout, so an error message built
// from either stream alone drops half of what went wrong.
func combinedOutput(ctx context.Context, exec *dagger.Container) string {
	stdout, _ := exec.Stdout(ctx)
	stderr, _ := exec.Stderr(ctx)
	return strings.TrimSpace(strings.TrimSpace(stdout) + "\n" + strings.TrimSpace(stderr))
}
