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
the slim CI image (~770MB). The `-full` variant (~1.34GB) additionally bundles
the 3D component-model libraries.

Select `-full` with the `full` boolean on `New`, which appends the suffix to
whatever tag is in play (so it composes with a pinned tag or a mirror). Passing
a tag that already ends in `-full` works too and is treated identically.

```go
dag.Kicad()                                              // docker.io/kicad/kicad:10.0 (slim)
dag.Kicad(dagger.KicadOpts{Full: true})                  // docker.io/kicad/kicad:10.0-full
dag.Kicad(dagger.KicadOpts{Tag: "10.0-full"})            // same image, tag spelled out
dag.Kicad(dagger.KicadOpts{Registry: "ghcr.io"})         // mirror
```

The `-full` variant is required for any 3D export that includes component
models — `Step`/`Glb`/`Stl`/… without `boardOnly`, and every `Vrml`. On the
slim image those models cannot resolve, so kicad-cli would silently emit a
board-only model. The module refuses that instead: a with-models 3D export on
the slim image fails with an error naming the `-full` tag as the fix. Pass
`boardOnly` to export board geometry alone on the slim image, or select `-full`
for populated assemblies. `Render` is the one exception — it degrades to a
bare-board render rather than failing, because a board-only render is still a
useful artifact.

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
| `Pcb.Ipc2581(version, units, outputName)` | IPC-2581 XML. Exposed as `ipc-2581` on the CLI. |
| `Pcb.Odb(compression, units, outputName)` | ODB++ archive. |
| `Pcb.Pdf(layers, outputName)` / `Pcb.PdfPerLayer(layers)` | `--mode-single` / `--mode-separate`. |
| `Pcb.Svg(layers, outputName)` / `Pcb.SvgPerLayer(layers)` | `--mode-single` / `--mode-multi`. |
| `Pcb.Dxf(layers, units, outputName)` / `Pcb.Ps(layers, outputName)` | Single-file DXF / PostScript plots. |
| `Pcb.Stats(format, units, outputName)` | Board statistics report (`report` or `json`). |
| `Pcb.Gencad(outputName)` / `Pcb.Ipcd356(outputName)` | GenCAD interchange / IPC-D-356 bare-board test netlist. |
| `Pcb.Render(side, quality, width, height, outputName)` | 3D render to PNG/JPEG (format follows the extension). |
| `Pcb.Import(inputPath, format, outputName)` | Convert a non-KiCad board to a `.kicad_pcb`. |
| `Pcb.Upgrade(force)` | Resave the board in the current KiCad format. |
| `Pcb.Step(boardOnly, excludeDnp, outputName)` | STEP model. |
| `Pcb.Glb` / `Stl` / `Brep` / `Ply` / `U3d` / `Xao` / `Stpz` / `Pdf3d` `(boardOnly, excludeDnp, outputName)` | The 3D model exports; `Pdf3d` is `pdf-3-d` on the CLI. |
| `Pcb.Vrml(boardOnly, excludeDnp, units, outputName)` | VRML model. `boardOnly` only gates the `-full` guard — VRML has no kicad-cli board-only mode. |
| `Sch.Erc(severity)` | Electrical Rule Check. Returns a bare `error` carrying the violation report. |
| `Sch.Bom(fields, groupBy, sortField, excludeDnp, outputName)` | BOM as CSV. |
| `Sch.Netlist(format, outputName)` | Netlist. |
| `Sch.Pdf(outputName)` | Multi-page schematic PDF. |
| `Sch.Svg()` / `Sch.Dxf()` / `Sch.Ps()` | One file per sheet, as a `Directory`. |
| `Fp(source)` | Bind a footprint library (a `.pretty` directory) for the `fp` family. |
| `Fp.Svg(footprint)` / `Fp.Upgrade(force)` | Export the library to SVG / resave it in the current format. |
| `Sym(source)` | Bind a symbol library (a `.kicad_sym` file) for the `sym` family. |
| `Sym.Svg(symbol)` / `Sym.Upgrade(force)` | Export the library to SVG / resave it in the current format. |
| `Ci(source)` | Chained builder composing the checks and outputs into one staged pipeline (parallel checks → fabrication outputs). |
| `Ci.WithErc()` / `Ci.WithDrc(schematicParity)` | Enable the ERC / DRC check stages. |
| `Ci.WithFabricationOutputs()` | Enable the fabrication package output (gerbers, drill, pos, BOM). |
| `Ci.Check()` | Run only the enabled checks in parallel; returns the aggregated `error`. |
| `Ci.Run()` | Run the checks then the outputs, returning the merged `Directory`; a failing check short-circuits before any export. |

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

## Footprint and symbol libraries (`fp`, `sym`)

`fp` and `sym` operate on a *library*, not on a board or schematic within a
project, so they attach to `Kicad` directly rather than to `Project`/`Pcb`/`Sch`:

- `Kicad.Fp(source)` takes a `*Directory` — a `.pretty` footprint library is a
  folder of `.kicad_mod` files. (The library is mounted at a `.pretty` path
  internally, because kicad-cli recognises a footprint library by that
  extension and silently finds zero footprints without it.)
- `Kicad.Sym(source)` takes a `*File` — a `.kicad_sym` symbol library is a
  single self-contained file.

That directory-vs-file split mirrors what each artifact actually is on disk,
and it keeps the project-scoped hoisted options (`--variant`,
`--drawing-sheet`), which neither command accepts, off their surface. Each
family exposes `Svg` (export the library, or one named footprint/symbol, to
SVG) and `Upgrade` (resave in the current KiCad format).

## Not wrapped

`hpgl` is the only `pcb export` format deliberately left out: KiCad 10's
`pcb export hpgl` self-reports as *"No longer supported as of KiCad 10.0"*, so
wrapping it would only surface a dead command. `python-bom` is likewise omitted
— it is the legacy XML-plus-plugin BOM path that `sch export bom` (see
`Sch.Bom`) supersedes.

Anything still unwrapped remains reachable via `Container()`, the escape hatch:

```sh
dagger -m daggerverse/kicad call container \
    with-directory --path=/project --source=./hardware \
    with-workdir --path=/project \
    with-exec --args="kicad-cli,pcb,export,hpgl,board.kicad_pcb"
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
step := p.Pcb().Step(dagger.KicadPcbStepOpts{BoardOnly: true}) // board geometry on the slim image

// A populated assembly needs the -full image; without boardOnly on the slim
// image this errors, naming the -full tag.
full := dag.Kicad(dagger.KicadOpts{Full: true}).Project(src)
glb := full.Pcb().Glb() // component models included

// fp/sym act on libraries, so they hang off Kicad, not Project.
fpSvgs := dag.Kicad().Fp(prettyDir).Svg()          // *Directory, one SVG per footprint
symSvgs := dag.Kicad().Sym(symLibFile).Svg()       // *Directory, one SVG per symbol unit

// Text variables override what the .kicad_pro declares.
xml := p.WithVar("REV", "B").Pcb().Ipc2581()
```

## CI pipeline (`Ci` builder)

`Ci(source)` returns a builder that composes the design-rule checks and the
fabrication outputs into a single staged pipeline, so a hardware repo's CI is
one `dagger call` rather than a hand-assembled sequence of per-export calls. It
adds no capability of its own — every stage is a call you could make by hand
against `Project`/`Pcb`/`Sch`.

### Stages

1. **Parallel checks** — enabled individually via `WithErc()` and
   `WithDrc(schematicParity)`. Errors from enabled checks are aggregated via
   `github.com/dagger/dagger/util/parallel`; stage 2 is short-circuited on any
   stage-1 failure, so a failing check produces no output directory.
2. **Outputs** — enabled via `WithFabricationOutputs()` (gerbers, drill, pos,
   BOM). `Run` merges them into one `Directory`: `gerbers/` and `drill/`
   subdirectories plus `pos.pos` and `bom.csv` at the root. `Check` runs stage 1
   alone, for a PR gate that never needs the package.

The board and schematic are auto-discovered per stage, exactly as a bare
`Project(source).Pcb()`/`.Sch()` would. Same shape as `zig`'s and `go`'s `Ci`.

### CLI

    dagger -m daggerverse/kicad call ci \
        --source=./hardware \
        with-erc with-drc --schematic-parity=true \
        with-fabrication-outputs \
        run export --path=./fab

### Kicad SDK

```go
// Ci produces the fabrication package; a downstream pipeline composes it.
fab := dag.Kicad().Ci(src).
    WithErc().
    WithDrc(dagger.KicadCiWithDrcOpts{SchematicParity: true}).
    WithFabricationOutputs().
    Run()

// Or run only the checks as a PR gate.
if err := dag.Kicad().Ci(src).WithErc().WithDrc().Check(ctx); err != nil {
    return err
}
```

See `tests/main.go` for one example per function.
