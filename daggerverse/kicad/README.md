# kicad

A Dagger module wrapping [`kicad-cli`](https://docs.kicad.org/) from the
official `kicad/kicad` image, so a hardware project's design-rule checks and
fabrication outputs become `dagger call`s instead of the usual pile of shell
scripts and Makefile recipes.

Everything `kicad-cli` does is headless — no Xvfb, no display, including
renders — so the image runs unmodified.

## Image variants

The image is composed as `<registry>/kicad/kicad:<tag>`, following the
`registry`+`tag` idiom used by `kafka` and `envoy`. The default `10.0` tag is
the slim CI image (~770MB). The `-full` variant additionally bundles the 3D
component model libraries; without them a `Step` export carries only board
geometry, which is why `Step(boardOnly: true)` is the sensible default pairing
with the slim image.

```go
dag.Kicad()                                              // docker.io/kicad/kicad:10.0
dag.Kicad(dagger.KicadOpts{Tag: "10.0-full"})            // 3D models included
dag.Kicad(dagger.KicadOpts{Registry: "ghcr.io"})         // mirror
```

## The UID-1000 constraint

The image declares `USER kicad` (UID 1000) and has no entrypoint. The caller's
project is mounted read-only at `/project` and owned by root, so `kicad-cli`
cannot write next to it. Every export therefore stages its output under `/tmp`
and the module returns that directory or file.

`Jobset` is the exception: a jobset declares its own output paths relative to
the project, so the project is *copied* — with `Owner: "kicad"` — into a
writable `/tmp` workdir, and the whole tree comes back with the jobset's
artifacts in place.

## Project, not file

`Project` takes a `*Directory`, not a lone `*File`: `kicad-cli` resolves
sub-sheets, footprint libraries and drawing sheets relative to the project.

`--define-var`, `--variant` and `--drawing-sheet` apply to nearly every
subcommand, so they are hoisted onto `Project` as chained modifiers rather
than repeated as optional params across a dozen signatures. Each is passed
through only to the subcommands that accept it — `sch export bom`, `sch export
netlist`, `pcb export pos` and `pcb export drill` reject `--define-var`, for
instance, so passing it there would turn a valid call into a usage error.

`Pcb("")` and `Sch("")` auto-discover. A unique match wins outright; when
several files share the extension — the normal shape of a hierarchical
schematic, whose sub-sheets are `*.kicad_sch` too — the one named after the
`.kicad_pro` is the root, exactly as KiCad itself treats it. Anything still
ambiguous is an error naming the candidates.

## Function surface

| Name | Purpose |
|---|---|
| `Container()` | The bare kicad image — the escape hatch for every subcommand this module does not wrap. |
| `Version()` | `kicad-cli version`. |
| `Project(source)` | Bind a project tree to the toolchain. |
| `Project.WithVar(name, value)` | `--define-var name=value`. Takes a pair, not a map, because Dagger functions cannot accept map params. |
| `Project.WithVariant(variant)` | `--variant`. |
| `Project.WithDrawingSheet(sheet)` | `--drawing-sheet`. |
| `Project.Pcb(path)` | Select a board; empty auto-discovers. |
| `Project.Sch(path)` | Select a schematic; empty auto-discovers. |
| `Project.Jobset(path)` | `jobset run`; returns the project tree with everything the jobset produced. |
| `Pcb.Drc(severity, schematicParity, refillZones)` | Design Rule Check. Returns a bare `error` carrying the violation report. |
| `Pcb.Gerbers(layers, precision, checkZones)` | Gerber plot as a `Directory`; empty `layers` plots every layer plus the `.gbrjob`. |
| `Pcb.Drill(format, units, origin, separatePlatedHoles, generateMap)` | Drill files as a `Directory`. |
| `Pcb.Pos(side, format, units, smdOnly, outputName)` | Pick-and-place file. |
| `Pcb.Step(boardOnly, excludeDnp, outputName)` | STEP model. |
| `Pcb.Ipc2581(version, units, outputName)` | IPC-2581 XML. Exposed as `ipc-2581` on the CLI. |
| `Pcb.Pdf(layers, outputName)` / `Pcb.PdfPerLayer(layers)` | `--mode-single` / `--mode-separate`. |
| `Pcb.Svg(layers, outputName)` / `Pcb.SvgPerLayer(layers)` | `--mode-single` / `--mode-multi`. |
| `Sch.Erc(severity)` | Electrical Rule Check. Returns a bare `error` carrying the violation report. |
| `Sch.Bom(fields, groupBy, sortField, excludeDnp, outputName)` | BOM as CSV. |
| `Sch.Netlist(format, outputName)` | Netlist. |
| `Sch.Pdf(outputName)` | Multi-page schematic PDF. |
| `Sch.Svg()` | One SVG per sheet, as a `Directory`. |

### Why `Drc`/`Erc` return a bare `error`

Dagger drops a function's value whenever it also returns a non-nil error, so a
`(report, error)` signature would leave the violation list unreachable on
exactly the failure path that needs it. Both run with
`Expect: dagger.ReturnTypeAny` — `--exit-code-violations` exits 5 — read the
report file back, and fold it into the returned error. Same shape as `zig`'s
`Fmt`.

### Why `Pdf`/`Svg` come in pairs

A Dagger function has exactly one return type, so modelling `--mode-single` vs
`--mode-separate` as a parameter would force `*Directory` onto the common
single-file case. They are split instead.

### Why `outputName`, never `output`

`-o` collides with the Dagger CLI's own top-level flag. Each `outputName` is
validated to reject `/`, so an artifact cannot be written outside the
module-owned staging directory.

## Not wrapped

The long tail of exotic and legacy exports (`brep`, `ply`, `u3d`, `vrml`,
`xao`, `stpz`, `3dpdf`, `gencad`, `ipcd356`, `stl`, `glb`, `dxf`, `ps`,
`stats`, `python-bom`, `odb`) plus `fp`, `sym`, `pcb import`, `pcb upgrade`
and `pcb render` are all reachable today via `Container()`:

```sh
dagger -m daggerverse/kicad call container \
    with-directory --path=/project --source=./hardware \
    with-workdir --path=/project \
    with-exec --args="kicad-cli,pcb,export,stats,board.kicad_pcb"
```

## CLI quick reference

```sh
# List functions
dagger -m daggerverse/kicad functions

# Run ERC and DRC on a project
dagger -m daggerverse/kicad call project --source=./hardware sch erc
dagger -m daggerverse/kicad call project --source=./hardware pcb drc --schematic-parity

# Fabrication outputs
dagger -m daggerverse/kicad call project --source=./hardware \
    pcb gerbers --layers=F.Cu,B.Cu,F.SilkS,Edge.Cuts export --path=./fab
dagger -m daggerverse/kicad call project --source=./hardware \
    sch bom --output-name=bom.csv export --path=./bom.csv

# Run the project's own jobset
dagger -m daggerverse/kicad call project --source=./hardware \
    jobset --path=fab.kicad_jobset export --path=./out
```

## Kicad SDK quick reference

```go
p := dag.Kicad().Project(src)

// Checks return a bare error carrying the violation report.
if err := p.Sch().Erc(ctx); err != nil { return err }
if err := p.Pcb().Drc(ctx, dagger.KicadPcbDrcOpts{SchematicParity: true}); err != nil { return err }

// Exports are lazy; validation errors surface on resolve.
gerbers := p.Pcb().Gerbers(dagger.KicadPcbGerbersOpts{Layers: []string{"F.Cu", "B.Cu"}})
step := p.Pcb().Step(dagger.KicadPcbStepOpts{BoardOnly: true})

// Text variables override what the .kicad_pro declares.
xml := p.WithVar("REV", "B").Pcb().Ipc2581()
```

See `tests/main.go` for one example per function.
