# tesseract

Daggerverse module that runs [Tesseract](https://tesseract-ocr.github.io/) OCR
as a `dagger call`. Hand it an image and get back plain text, or hOCR / ALTO /
TSV / PAGE / a searchable PDF for anything that needs word positions and
confidences. Hand it a directory and get the same artifacts back for every page
in it, at the paths they came in on. Hand it a PDF and it rasterizes the pages
for you first.

**There is no official Tesseract container image** — upstream ships source only,
and every image on Docker Hub is third-party. So this module assembles its own
the way `qemu` does rather than pinning a vendor image the way `kicad` does: a
module-pinned Alpine (`3.24`, whose community repository carries tesseract-ocr
5.5.2) plus `apk add tesseract-ocr` and one `tesseract-ocr-data-<lang>` package
per requested language. Only the `registry` prefix is caller-overridable, for
air-gapped mirrors.

Nothing here handles secrets and nothing opens a port.

## Languages live on the root object

```go
New(registry="docker.io", alpineTag="3.24", languages []string, ompThreadLimit int) *Tesseract
```

On Alpine each language is a **separate apk package**, and the base package
carries no language data at all. Selecting a language therefore changes what the
image *is*, not just what a flag says — which is why the set is fixed on `New`
rather than passed per call. An empty `languages` installs `eng`.

`osd` is in the same namespace but is not a recognition language: it is the
orientation-and-script-detection model, and `Document.Osd` needs it.

```go
dag.Tesseract()                                                     // eng
dag.Tesseract(dagger.TesseractOpts{Languages: []string{"eng","deu","osd"}})
dag.Tesseract(dagger.TesseractOpts{Registry: "ghcr.io"})            // mirror
```

## Models Alpine has no package for — `WithTessdata`

```go
Tesseract.WithTessdata(dir *dagger.Directory) *Tesseract    // --tessdata-dir
```

The package set is a floor, not a ceiling. A fine-tuned model, a `tessdata_best`
or `tessdata_fast` variant, or a language with no apk package at all comes in as
a directory of `.traineddata` files. Each file becomes a language named after its
stem — `deu_frak.traineddata` is the language `deu_frak` — so renaming the file
renames the model.

```go
dag.Tesseract().WithTessdata(host.Directory("./models"))
```

From there a supplied model is indistinguishable from a packaged one: `Langs`
reports the union, `WithLanguage` accepts either, and `Osd` is satisfied by an
`osd.traineddata` that arrived this way.

The directory is **merged** with the packaged one rather than swapped for it.
`--tessdata-dir` moves the whole datadir, and that directory holds more than
models: `configs/` carries the renderer configfiles that recognition names as
its trailing arguments, and `pdf.ttf` is what the PDF renderer draws its
invisible text layer with. Pointed at the caller's directory alone, every
renderer breaks and every packaged language disappears. On a name collision the
caller's file wins, which is what makes replacing the stock `eng` with a
fine-tuned one work.

The merge is mounted, not copied — a model set runs 20MB and up, and there is no
reason for it to become an image layer. Nothing changes for an image without
tessdata: the flag is omitted entirely rather than pointed at the packaged path.

## OpenMP fan-out — `ompThreadLimit`

Alpine's tesseract links `libgomp` and reports `Found OpenMP 201511`, so left
alone it sizes every thread team by the CPUs it can see. Whether that is what
you want depends entirely on how many images you have.

**One image, idle cores — threads help, modestly.** A single pass over a full
page of text, pinned to four CPUs:

| threads | wall | CPU | speedup | CPU cost |
| --- | --- | --- | --- | --- |
| 1 | 1.96s | 1.77s | 1.00x | 1.00x |
| 2 | 1.70s | 2.53s | 1.15x | 1.43x |
| 4 | 1.50s | 3.86s | 1.31x | 2.18x |

Four threads buy 31% off the clock for 2.18x the CPU — 33% parallel efficiency.
Tesseract's OpenMP regions sit inside the LSTM inner loops and do not amortize.
It is still free wall-clock when nothing else wants the cores, which is why
unbounded is the default and `ompThreadLimit` is opt-in.

**More than one image — processes win outright.** Eleven passes over that same
page, four CPUs, every way of spending them:

| shape | wall | CPU |
| --- | --- | --- |
| serial, 4 threads each | 17.70s | 45.69s |
| 2 concurrent, 2 threads each | 10.53s | 28.95s |
| 4 concurrent, 1 thread each | 6.39s | 21.17s |
| 11 concurrent, 1 thread each | **5.95s** | 21.17s |

Same output; the thread-parallel shape burns 2.2x the CPU to take 3x as long.
Process-level parallelism runs at ~90% efficiency where OpenMP manages 33%.

**Oversubscribe both at once and it collapses.** Concurrency multiplied by
per-process threads, rather than bounded by the core count:

| CPUs | `OMP_THREAD_LIMIT` | 11 concurrent passes |
| --- | --- | --- |
| 4 | unset | **8m34s** |
| 4 | `1` | **1.06s** |
| 32 | unset | 0.68s |
| 32 | `1` | 0.55s |

~485x apart on the small fixture. Upstream has carried reports of this shape for
years ([#2611](https://github.com/tesseract-ocr/tesseract/issues/2611), #1171,
#263) and `OMP_THREAD_LIMIT` is the standard answer. This module's own test
suite sets it to `1` (#226); a shared CI runner is the textbook case.

```go
dag.Tesseract()                                          // one image, own machine
dag.Tesseract(dagger.TesseractOpts{OmpThreadLimit: 1})   // many images, or sharing
```

The variable is set on the image after `apk add`, so it applies to everything
reached through `Container()` too, and two images differing only in their limit
still share the package fetch. A negative value is rejected on `New` — libgomp
reads an unparseable `OMP_THREAD_LIMIT` as absent and silently goes back to one
thread per CPU, the opposite of what was asked for.

## Toolchain

```go
Tesseract.WithTessdata(dir *dagger.Directory) *Tesseract
Tesseract.Container() *dagger.Container            // +cache="session"
Tesseract.Version(ctx) (string, error)             // +cache="session"
Tesseract.Langs(ctx) ([]string, error)             // +cache="session"
Tesseract.Parameters(ctx) (string, error)          // +cache="session"
Tesseract.Document(source *dagger.File) *Document
```

`Container` is the escape hatch for everything this module does not wrap — the
training binaries the apk package ships (`lstmtraining`, `text2image`,
`combine_tessdata`), and tesseract's long tail of renderers, stay reachable via
`container with-exec`.

`Parameters` is the list `Document.WithParameter` validates against.

## Document — a file, not a directory

```go
Tesseract.Document(source *dagger.File) *Document
```

Unlike `kicad.Project`, the boundary input is a lone `*dagger.File`: tesseract
resolves nothing relative to its input, so one image — including a multi-page
TIFF — is the whole unit of work. A PDF is `FromPdf`, which rasterizes first and
hands back one of these. A folder of pages is `Batch`, below; the
directory there is a *set of independent inputs*, not context the recognition
resolves against, which is why it is a second entry point rather than a widening
of this one.

`Document` is immutable; every `With*` returns a copy, so one configured
document can be branched into several outputs without the branches interfering.

```go
Document.WithLanguage(lang string) *Document                // -l eng+deu
Document.WithPageSegmentation(mode PageSegMode) *Document   // --psm
Document.WithEngine(mode EngineMode) *Document              // --oem
Document.WithDpi(dpi int) *Document                         // --dpi
Document.WithParameter(name, value string) *Document        // -c name=value
Document.WithUserWords(words *dagger.File) *Document        // --user-words
Document.WithUserPatterns(patterns *dagger.File) *Document  // --user-patterns
```

`WithLanguage` selects from what the image carries — the packages `New`
installed and any model `WithTessdata` supplied — and cannot itself add one,
since both are baked in when the image is assembled. Unset, recognition runs in
the first language `New` installed rather than tesseract's hardcoded `eng`, so
an image built with only `deu` still works.

`WithParameter` takes a name and a value separately because Dagger functions
cannot accept map parameters (the `kicad.Project.WithVar` precedent).

Those seven builders are not implemented here. They forward to an internal
`options` type that `Batch` carries too, so each option is written once — as a
builder, as a piece of argv, and as a deferred check — and the two units of work
cannot drift apart. Adding an option means touching `options.go` plus a
three-line forwarder on each.

## Enums

Strongly-typed closed sets — invalid values are unrepresentable through the SDK.
The enum value maps to the CLI's number internally.

```go
PageSegMode // OSD_ONLY AUTO_OSD AUTO SINGLE_COLUMN SINGLE_BLOCK_VERT_TEXT
            // SINGLE_BLOCK SINGLE_LINE SINGLE_WORD CIRCLE_WORD SINGLE_CHAR
            // SPARSE_TEXT SPARSE_TEXT_OSD RAW_LINE
EngineMode  // LEGACY LSTM LEGACY_LSTM DEFAULT
Format      // TXT HOCR ALTO TSV PDF PAGE
```

`--psm 2` has no member: upstream never implemented it.

All four `EngineMode` members work because Alpine packages the **combined**
`tesseract-ocr/tessdata` models, which carry legacy data alongside LSTM. A build
against `tessdata_fast` or `tessdata_best` would leave `LEGACY` and
`LEGACY_LSTM` failing at runtime; `lstm-engine-recognizes-fixture` exercises
every member so that regression surfaces here.

## Outputs

```go
Document.Text(ctx) (string, error)                              // straight off stdout
Document.Txt(ctx)  (*dagger.File, error)
Document.Hocr(ctx) (*dagger.File, error)
Document.Alto(ctx) (*dagger.File, error)
Document.Tsv(ctx)  (*dagger.File, error)
Document.Pdf(ctx)  (*dagger.File, error)                        // searchable PDF
Document.Page(ctx) (*dagger.File, error)                        // PAGE XML
Document.Export(ctx, formats []Format) (*dagger.Directory, error)
Document.Osd(ctx)  (string, error)                              // --psm 0
```

`Export` exists alongside the single-artifact functions because tesseract
accepts several renderers per invocation: asking for text, hOCR and PDF together
is **one** recognition pass, not three. Artifacts are named `result` plus the
renderer's own extension — `result.txt`, `result.hocr`, `result.xml` (ALTO),
`result.tsv`, `result.pdf`, `result.page.xml`.

The argument builder keeps `-l` / `--psm` / `--oem` ahead of the trailing
configfiles; tesseract stops parsing flags at the first configfile, so a `-l`
after one is read as a file name to open.

`Osd` builds its own invocation rather than reusing the document's recognition
options: orientation detection runs off the `osd` model alone, so the selected
language, engine and page-segmentation mode have nothing to say about it.

Artifacts come back as `exec.File(path)` off the recognition container. The
`dag.CurrentModule().Workdir` staging pattern does not apply — this module
generates no bytes in Go.

## Batch — a directory in, a mirrored directory out

```go
Tesseract.Batch(source *dagger.Directory) *Batch

Batch.WithGlob(pattern string) *Batch    // default: the image extensions below
Batch.WithLanguage(lang string) *Batch   // …and the six other Document builders
Batch.Files(ctx) ([]string, error)
Batch.Export(ctx, formats []Format) (*dagger.Directory, error)
```

A scanned-document pipeline has a folder, not a file. `Export` returns a
directory mirroring the input layout with one artifact per requested format per
image, so `scans/deep/page-3.png` becomes `scans/deep/page-3.txt` next to
`scans/deep/page-3.pdf`. Mirroring is the point of the return type: a flat
`result-1.txt`, `result-2.txt` would force every caller to rebuild the
page-to-text correspondence the input directory already expressed.

### Why not tesseract's own list-file mode

Tesseract accepts a text file of image paths as its `FILE` argument, and #220
proposed exactly that. It does not do what the name suggests: it treats the list
as one multi-page **document** and renders a *single concatenated artifact set* —
one `.txt` with form-feed page breaks, one multi-page PDF, one hOCR carrying
`page_1`/`page_2` divs. There is no way to recover per-image files from it, least
of all for PDF. (That behaviour is not useless — it is precisely what the pages
of one PDF want, and `FromPdf` below is built on it. It is just not what a
folder of independent scans wants.)

So the batch is one container **exec** rather than one tesseract **process**: the
exec loops over a manifest, recognising each image onto its own output base. That
collapses the part that actually costs anything in Dagger — a `WithExec`, its
mounts and its cache lookup, per page — to one, while each page keeps its own
artifacts. The loop reaches flags and configfiles through `"$@"` rather than
string interpolation, so no value needs escaping.

### Which files take part

`Directory.Glob` has no brace expansion, so "common image extensions" cannot be
spelled as one pattern. The default is `**/*` narrowed by a case-insensitive
extension filter:

```
.bmp .gif .jpe .jpeg .jpg .pbm .pgm .pnm .png .ppm .tif .tiff .webp
```

That set is what this image can decode — `tesseract --version` reports leptonica
linked against libgif, libjpeg, libpng, libtiff and libwebp, and leptonica reads
BMP and the PNM family itself. The README, manifest and checksum files a real
scan folder collects are therefore ignored rather than fed to leptonica, which
would fail the run.

`WithGlob` replaces both halves: a caller who names a pattern owns it, and gets
no extension filtering, because leptonica sniffs content rather than trusting
extensions and `**/*.scan` is a reasonable thing to ask for. PDFs stay rejected
either way.

`Files` reports the matched set without paying for the recognition, which is the
answer to the question a glob always raises.

## PDF input — `FromPdf`

```go
Tesseract.FromPdf(source *dagger.File, dpi int /* +default=300 */) *Document
```

Leptonica cannot read PDF, so `Document` rejects one outright. `FromPdf` is the
way in: it rasterizes the pages with `pdftoppm` and returns an ordinary
`Document` over them, so every output, option and error path above works on a
PDF exactly as it does on an image.

A multi-page PDF stays **one** document. The pages are recognised through
tesseract's list-file mode, which accumulates them into a single artifact per
format — `Text` returns the whole document with form feeds between pages, `Pdf`
returns one searchable PDF with the source's page count, and the structured
formats carry one page element per page. This is the same list-file mode `Batch`
deliberately avoids, and for the opposite reason: for a folder of independent
scans a concatenated result is wrong, and for the pages of one PDF it is exactly
right.

`dpi` is what the pages are rasterized at, and is declared to tesseract as the
source resolution too, since it is exactly known at that point. 300 is the
default because it is tesseract's own long-standing recommendation for input
resolution; raising it costs time and memory roughly with its square, and
lowering it costs accuracy on body text.

### Why a separate container

Rasterization runs on its own container — Alpine at the same pinned tag, plus
`poppler-utils` and `ttf-liberation` — rather than on the toolchain image, so
the cost lands on the callers who asked for it:

| | image | cost |
| --- | --- | --- |
| toolchain today | 67.1MiB | — |
| toolchain with poppler + font installed unconditionally | 81.4MiB | +21% for every caller, PDF or not |
| separate rasterizer | 35.0MiB | paid only by `FromPdf` callers; 8.0MiB of it is the shared Alpine base |

It also caches separately, so changing a recognition option does not re-rasterize
the document.

The font package is not decoration. A PDF that names one of the base-14 fonts
without embedding it — the usual shape for anything not born as a scan — is
drawn by poppler through a fontconfig substitute, and with no font installed at
all there is nothing to substitute: **the page renders blank, recognition finds
no text, and the call succeeds**. Liberation is the family to install for it,
being metric-compatible with Helvetica, Times and Courier, so substituted text
keeps the line breaks and positions the document was written with. DejaVu covers
more of Unicode but is 5.8MiB larger and matches none of those metrics.

Page files are listed by globbing rather than by counting pages, because
`pdftoppm` names them with the page number zero-padded to the width of the
*last* page: a 9-page document yields `page-1.png` and a 10-page one
`page-01.png`. One document means one padding width, so a lexicographic sort is
page order.

## Validation

Builders have no error return, so every check below is deferred to the output
call that would have used it. Each rejection names the legal set rather than
letting tesseract fail in its own vocabulary.

| Rejected | Why the raw failure is worse |
| --- | --- |
| `WithLanguage` naming a language neither installed nor supplied | tesseract talks about traineddata paths and `TESSDATA_PREFIX`, never about the fact that models arrive via `New` or `WithTessdata` |
| `Osd` with no `osd` model installed or supplied | same, plus the fix is a different function's argument |
| a `.pdf` source to `Document` or `Batch` | Leptonica has no PDF support and reports the file's first line as if it were a file name it could not open; the message names `FromPdf`, so the fix is a function call rather than an errand |
| `FromPdf` at zero or negative `dpi` | `pdftoppm` would fail much later, talking about its own `-r` flag rather than about the argument the caller passed |
| `WithParameter` with an empty name, or one containing `=` | `-c` takes `name=value`, so an embedded `=` silently sets a *different* variable |
| `WithParameter` naming an unknown control variable | tesseract only warns (`Warning: The parameter '...' was not found.`) and exits 0, so a typo is indistinguishable from a no-op |
| `WithDpi` at zero or negative | tesseract would take it as a real measurement and scale its analysis by it |
| a `Batch` glob matching nothing | an empty directory is indistinguishable from a batch that ran and found no text, so a typo would surface much later as missing output |
| a `Batch` source holding no images | same, but the fix is `WithGlob` rather than the pattern, so the message names the default extension set |
| two `Batch` inputs sharing an output base | `a.png` and `a.jpg` both render onto `a.txt`; the second silently overwrites the first and the run looks like it succeeded with a page missing |
| a `Batch` file name containing a tab or newline | the manifest is tab-separated and newline-delimited, so the loop would recognise the wrong file |

Errors fold tesseract's own output in via `Expect: ReturnTypeAny` plus a
combined stdout+stderr helper, because tesseract splits usage errors onto stderr
and progress onto stdout.

## Caching

No `+cache=` directive appears on any `Document` or `Batch` output: recognition
is a pure function of the image bytes plus the flags, so the 7-day default is
correct and there is no chained-method propagation problem to worry about. `Container`,
`Version`, `Langs` and `Parameters` take `+cache="session"` per `kicad`, because
a floating Alpine tag can resolve differently across sessions.

## Follow-ups

The training toolchain (#222); a chained `Ci` builder (#223); an `examples/go`
cookbook (#224).

Batch OCR over a directory (#220) and PDF-input rasterization (#221) both
landed — see above. `Batch` still deliberately does not expose the concatenated
multi-page output tesseract's list-file mode produces; that mode now has exactly
one caller, `FromPdf`, where the pages really are one document. If "a folder of
pages in, one document out" turns out to be wanted for a `Batch` too, that is a
separate function, not a flag on `Export`.

GPU acceleration is **not** a follow-up: tesseract has no CUDA path and upstream
does not plan one, Alpine builds without OpenCL, and Dagger's GPU passthrough is
NVIDIA-only by construction. Recognition is CPU-bound here by design, not by
omission.
