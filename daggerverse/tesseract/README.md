# tesseract

Daggerverse module that runs [Tesseract](https://tesseract-ocr.github.io/) OCR
as a `dagger call`. Hand it an image and get back plain text, or hOCR / ALTO /
TSV / PAGE / a searchable PDF for anything that needs word positions and
confidences. Hand it a directory and get the same artifacts back for every page
in it, at the paths they came in on, several at a time — or hand that directory
to `Ci` and get the whole pipeline, quality gate included, as one call. Hand it
transcribed lines and it fine-tunes a model against them and gives you back a
`.traineddata` the same module recognises with.

**PDF is not an input.** Leptonica cannot read it and this module carries
nothing that can; render the pages with the [`pdf`](../pdf) module and hand the
directory to `Batch` — see [PDF input](#pdf-input).

**There is no official Tesseract container image** — upstream ships source only,
and every image on Docker Hub is third-party. So this module assembles its own
the way `qemu` does rather than pinning a vendor image the way `kicad` does: a
module-pinned Alpine (`3.24`, whose community repository carries tesseract-ocr
5.5.2) plus `apk add tesseract-ocr` and one `tesseract-ocr-data-<lang>` package
per requested language. Assembling an image that way is **two** fetches, not
one: `registry` overrides where the *image* comes from, and `WithApkRepository`
overrides where the *packages* installed onto it come from — see
[Air-gapped installs](#air-gapped-installs--withapkrepository-withapkkey-withapkauth).

Nothing here opens a port. The one secret it handles is `WithApkAuth`, the
credential for a package repository that requires one.

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
dag.Tesseract(dagger.TesseractOpts{Registry: "ghcr.io"})            // image only
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

## Air-gapped installs — `WithApkRepository`, `WithApkKey`, `WithApkAuth`

```go
Tesseract.WithApkRepository(url string) *Tesseract            // /etc/apk/repositories
Tesseract.WithApkKey(key *dagger.File) *Tesseract             // /etc/apk/keys/<name>
Tesseract.WithApkAuth(credentials *dagger.Secret) *Tesseract  // $NETRC
```

`registry` moves the container image. These move the packages installed onto it,
and on a network that cannot reach `dl-cdn.alpinelinux.org` you need both:
mirroring Alpine into a private registry and stopping there buys a container
that fails on its very first `apk add` — or, where the CDN is blackholed rather
than refused, hangs until it times out.

```go
dag.Tesseract(dagger.TesseractOpts{Registry: "registry.corp"}).
    WithApkRepository("https://mirror.corp/alpine/v3.24/main").
    WithApkRepository("https://mirror.corp/alpine/v3.24/community").
    WithApkKey(host.File("./mirror.rsa.pub")).
    WithApkAuth(netrc)
```

**The first call replaces the image's list rather than appending to it.**
Repeatable after that, in preference order. Appending would be the friendlier
default everywhere except the case the option exists for, where a surviving
default is a repository apk still consults and still waits on. An empty URL is
dropped rather than written, the same way an empty language code is.

**A repository index is signed, and apk refuses one it cannot verify** — so a
private mirror needs `WithApkKey` as well, and `--allow-untrusted` is
deliberately not offered. The key file keeps its own name in `/etc/apk/keys`,
because the name is load-bearing: an index's signature names the key file it was
made with and apk looks that exact name up. That is why the option takes a
`*dagger.File` rather than the key's bytes — and why it is a File and not a
Secret, a public key being meant to be in the image.

**Credentials are a `*dagger.Secret` holding a netrc file**, mounted at
`/run/apk/netrc` with `$NETRC` pointing at it:

```
machine mirror.corp login CI_USER password CI_TOKEN
```

Alpine 3.24 ships apk-tools 3.0, whose built-in libfetch reads credentials from
that file when a repository answers `401`, and from nowhere else that keeps them
out of sight — userinfo in the repository URL would land in
`/etc/apk/repositories` and in every apk error quoting it, and `HTTP_AUTH` would
land in the image's environment. Mounted rather than written, so the credentials
are not a layer either: nothing reaches the cache key, the argv, the environment
or the filesystem a caller exports. Hosts are matched by name only, so one
stanza covers a mirror on any port.

**One `apk add` is all this module performs**, so a mirror carrying
`tesseract-ocr` and the requested `tesseract-ocr-data-<lang>` packages covers it
completely. Rendering a document's pages moved to the `pdf` module along with
the packages that did it, so an air-gapped pipeline that needs both configures
each module's mirror on its own object.

Set none of them and nothing changes. The repositories file, the keys directory
and the environment are the image's own, so an existing caller sees no cache-key
churn from a feature it is not using.

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

**A batch already has more than one image, so it answers this itself.**
`Batch.WithConcurrency` defaults to one recognition per CPU, and recognising
more than one at a time also caps that batch's OpenMP at one thread per
process. Eleven copies of the four-line fixture, four pinned CPUs, timed
against tesseract directly rather than through the module — the same fixture
and CPU count as the oversubscription table above, not the full page the two
before it use:

| concurrent passes | `OMP_THREAD_LIMIT` | wall | CPU |
| --- | --- | --- | --- |
| 1 | unset | 3.37s | 5.78s |
| 4 | `1` | 1.24s | 4.21s |
| 8 | `1` | 1.18s | 4.25s |
| 11 | `1` | **1.05s** | 4.12s |
| 4 | unset | **3m36.8s** | 14m24s |

The last row is the whole reason concurrency implies the bound: same four
passes as the second row, threads unbounded, 174x apart. A caller who reaches
for concurrency without having read anything about libgomp would otherwise land
there. (What `Batch` costs on top of these numbers is a separate question, and a
separate measurement, in [One exec per image](#one-exec-per-image-fanned-out-from-go).)

An `ompThreadLimit` named on `New` is left alone, because it was named.
Otherwise the bound rides on a copy of the module the batch makes for itself, so
the caller's image keeps whatever it had, every other caller's recognition is
untouched, and both images still share the package fetch.

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
TIFF — is the whole unit of work. A folder of pages is `Batch`, below; the
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

Document.Box(ctx)             (*dagger.File, error)             // makebox
Document.ProcessedImages(ctx) (*dagger.File, error)             // get.images
Document.LstmTrain(ctx, groundTruth string) (*dagger.File, error) // lstm.train
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
`dag.CurrentModule().Workdir` staging pattern applies in exactly one place —
the box files `Training` generates in Go, below.

The last three are the training-adjacent renderers, and they are deliberately
**not** `Format` members, so `Export` cannot ask for them. `Export`'s promise is
that a set of formats is one pass producing one artifact per format, and none of
them keeps it: `makebox` and `get.images` describe the recognition rather than
reporting it, and `lstm.train` is not an output of recognition at all — it needs
ground truth, which is an argument an enum member cannot carry.

`Box` is the character-level box file, the only output here that descends below
the word: hOCR and TSV stop there. `ProcessedImages` is the image tesseract
actually recognised — binarized and deskewed — which is what answers "is the
model wrong, or did the page never survive thresholding?". `LstmTrain` pairs one
line image with the text it renders and returns the `.lstmf` training sample
built from the two; `Training` is the shorter path when the whole job is
fine-tuning.

## Batch — a directory in, a mirrored directory out

```go
Tesseract.Batch(source *dagger.Directory) *Batch

Batch.WithGlob(pattern string) *Batch          // default: the image extensions below
Batch.WithConcurrency(concurrency int) *Batch  // default: one per CPU
Batch.WithLanguage(lang string) *Batch         // …and the six other Document builders
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
of one PDF want. It is just not what a folder of independent scans wants, and it
is not what the fast path wants either: one serial pass over N pages forfeits
exactly the concurrency the next section is about.)

So a batch is its images recognised as **`Document`s** — one exec each, each
mounting its own image, each rendering its own artifact set — and what runs
several of them at a time is Go.

### One exec per image, fanned out from Go

```go
tess.Batch(scans).WithConcurrency(8).Export(ctx, formats)   // default: one per CPU
```

`Batch.Export` resolves the glob, builds a `Document` per match, and runs them
through a slot channel bounded by `WithConcurrency`. The bound, the scheduling,
the fail-fast and the error message are all `batch.go` — nothing about running a
batch is interpreted inside a container, so all of it is typed, reviewable and
debuggable the same way the rest of the module is.

That is a reversal. Until #298 the batch was **one** exec looping a
tab-separated manifest in `sh`, on the argument that the exec — its mounts, its
cache lookup — was the expensive part and should be paid once per directory
rather than once per page. Measured end-to-end, it is not:

| pages | one exec, `sh` loop | one exec per image, Go fan-out |
| --- | --- | --- |
| 11 | 5.5s | **2.8s** |
| 50 | 17.5s | **5.0s** |
| 50, one page edited | 17.5s | **2.5s** |

`dagger call batch --source=… export --formats=TXT entries`, median of three,
cold inputs each run, 32-CPU host; ~1.7s of every figure is CLI startup and
module load. The one-exec column is the manifest loop recognising one image at a
time, which is what it did, threads unbounded; the Go column is the default —
one recognition per CPU, one OpenMP thread each.

The last row is the one that does not close with more cores. The old exec
mounted the whole source directory, so editing any page invalidated the mount
and re-recognised **all fifty**; a per-image exec keys on that image, so the
other forty-nine are cache hits. For a corpus that grows or gets corrected a
page at a time, that is the difference between a re-run and a re-do.

Three properties the serial loop had for free, which the fan-out has to keep:

- **A page that cannot be read still fails the batch**, rather than going
  missing from a thousand results, and the error names the page — tesseract's
  own message often cannot, because handed a file leptonica will not decode it
  falls back to reading the file as a list of image paths and reports the
  *contents* of the first line as the file it could not open.
- **The first failure cancels the rest.** A batch that is going to fail has no
  reason to recognise the remaining pages first.
- **The artifacts are assembled in sorted order**, off execs that finished in
  whatever order they finished in, so the returned directory does not depend on
  the scheduling.

What it no longer has to do is protect a record format: file names with tabs or
newlines in them were rejected when a manifest had to carry them, and are
ordinary inputs now.

Recognising more than one image at a time also caps OpenMP at one thread per
process, unless an `ompThreadLimit` was named on `New` — see [OpenMP
fan-out](#openmp-fan-out--ompthreadlimit) for the measured reason, which is that
concurrency multiplied by threads is the one shape slower than doing nothing.
The bound rides on a copy of the module rather than the caller's, so nothing
else built from the same `Tesseract` sees it, and the two images still share the
package fetch.

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

## Ci — the whole pipeline as one call

```go
Tesseract.Ci(source *dagger.Directory) *Ci

Ci.WithLanguage(lang string) *Ci
Ci.WithFormats(formats []Format) *Ci        // default: TXT
Ci.WithMinConfidence(percent int) *Ci       // default: no gate
Ci.Check(ctx) error                         // the gate, no artifacts
Ci.Run(ctx) (*dagger.Directory, error)      // the gate, then the artifacts
```

A document-processing repo's CI wants one declarative call: OCR everything under
a directory, emit the archival formats, and fail the build when recognition
quality drops. `Ci` composes `Batch` without adding capability — every stage is
a call the caller could make by hand — so that CI is one `dagger call` rather
than a recognition step, an export step and a hand-rolled TSV parser.

Which files take part is `Batch`'s default: the image extensions above, at any
depth. `Run` returns the same mirrored directory `Batch.Export` does.

### The confidence gate

`WithMinConfidence` is the part that is not merely a bundled call. It reads the
`conf` column tesseract already publishes in its TSV output — nothing here
re-derives a confidence — and averages the word-level rows per page:

```
level  page_num  …  conf    text
5      1         …  96.063  The
5      1         …  92.481  quick
```

Levels 1–4 (page, block, paragraph, line) all report `-1`, so the `level` column
is what separates a measurement from a placeholder; word rows whose text is empty
are skipped too, or a page would be scored by how aggressively it was segmented
rather than by how well it was read. The columns are located by header name, not
by position: a gate reading the wrong column would not fail, it would pass the
wrong scans.

What that catches is the class of failure recognition does not report as one. A
page fed sideways, a scanner drifting out of focus, a language configured wrong —
all of them recognise *something*, exit 0, and render every artifact asked for.

The failure names the page that measured **worst** and how many others joined it,
because a batch that drifted out of focus fails several pages at once and the one
to go and find in the stack is the one furthest below the bar:

```
Ci: "scans/page-7.png" was recognised at a mean word confidence of 61.4%, below
the required 80%, the lowest of 3 page(s) below it: check the scan and the
recognition language, or lower the bar with WithMinConfidence
```

A page that recognised **no** words gets its own message rather than being
reported as zero: the mean of no words is not zero, it is undefined, and a blank
page is a different fault with a different fix.

### Why the gate rides along on the artifact pass

`Run` renders TSV *alongside* whatever `WithFormats` enabled, in the same
recognition pass, then measures it — rather than recognising the directory once
to check and again to export. Recognising everything twice is the single most
expensive thing this module could be asked to do, and the only difference
between a gated run and an ungated one is a TSV.

A failing gate still returns the error and no directory: the artifacts exist
inside the exec, and a caller who is refused them is no better off for their
having been skipped. When TSV was not among the enabled formats the gate's own
TSVs are dropped from the result, so enabling a threshold cannot silently change
what the pipeline produces.

`Check` is the same gate with nothing to export, for the PR that wants to know
whether the scans are good enough without paying to render the archive. It runs
recognition either way — with no threshold set the bar is simply "every matched
file is an image tesseract can read", which is not nothing — because a page's
confidence is not knowable without recognising it.

## PDF input

Leptonica cannot read PDF, so `Document` and `Batch` reject one outright. The
[`pdf`](../pdf) module renders the pages; this module recognises them:

```go
doc   := dag.Pdf().Document(source)
pages := doc.Convert().WithDpi(300).Png()          // page-0001.png, page-0002.png, ...

out := dag.Tesseract(dagger.TesseractOpts{OmpThreadLimit: 1}).
    Batch(pages).
    WithDpi(300).
    WithConcurrency(runtime.NumCPU()).
    Export([]dagger.TesseractFormat{
        dagger.TesseractFormatTsv,             // word boxes + confidences, per page
        dagger.TesseractFormatPdf,             // searchable PDF, per page
    })

// One artifact per page, named after it. Reassemble what you need:
searchable, _ := dag.Pdf().Merge(pageFiles(out, ".pdf"))
```

**The result is per page, not per document**, and that is the whole trade. This
module used to offer the other shape — `FromPdf` rasterized a PDF and handed
tesseract a page list, which makes it treat N images as one document with one
concatenated artifact set. That is a single serial tesseract process, and
recognition is where all the time is: the [OpenMP
numbers](#openmp-fan-out--ompthreadlimit) above measure process-level
parallelism at ~90% efficiency against OpenMP's 33%, so a 1000-page document
recognised as one document forfeits nearly all of the available speedup. #276
removed it in favour of the concurrent `Batch` (#298).

So reassembly is the caller's job. For the searchable PDF that is one call —
`pdf.Merge`, in page order — and for TSV it is the caller's own reader, which is
usually what they wanted anyway: a `page_num` column that always says `1` is
less useful than a file named after the page it describes.

Two things have to be carried across the boundary by hand, because nothing in a
directory of PNGs states them:

- **The resolution.** `WithDpi(300)` on the render *and* on the batch. It
  reaches tesseract as `--dpi`, which scales its layout analysis, and a mismatch
  costs accuracy silently. 300 is tesseract's own long-standing recommendation;
  the `pdf` module's default is 150, a screen-reading resolution, so an OCR
  render always names it.
- **The thread bound.** `OmpThreadLimit: 1` alongside `WithConcurrency`, or the
  two multiply — see [OpenMP fan-out](#openmp-fan-out--ompthreadlimit). `Batch`
  applies it to its own exec when the caller named no bound, so this is
  belt-and-braces rather than required.

**Page order is name order.** The `pdf` module pads its page numbers to a fixed
minimum width for exactly this reason: `pdftoppm` driven by hand pads to the
width of the *last* page, so a 9-page document yields `page-1.png` and a 10-page
one `page-01.png`, and `page-10.png` sorts before `page-2.png`. `Batch` sorts
its matches, so uniform padding is what makes that sort mean page order.

### What this module no longer carries

Rendering used to happen here, in a second container of its own. Measured with
`apk add --simulate` on the pinned `alpine:3.24`:

| | image | change |
| --- | --- | --- |
| toolchain | 67.1 MiB | unchanged — it never carried the render packages |
| toolchain with `poppler-utils` + `ttf-liberation` installed unconditionally | 81.4 MiB | the +21% tax this module never levied, and now cannot |
| the rasterizer container this module used to assemble | 35.0 MiB | **gone** |

No bytes are saved for a caller who renders a PDF: the `pdf` module installs the
same two packages, so that 35.0 MiB moves rather than vanishing. What changes is
who builds it and when — a caller who never has a PDF cannot end up building it
at all, this module performs exactly one `apk add`, and there is one fewer place
the air-gapped configuration has to reach.


## Training — fine-tuning a model

```go
Tesseract.Training(source *dagger.Directory) *Training

Training.WithBaseModel(lang string) *Training   // required
Training.WithIterations(n int) *Training        // +default 100
Training.Files(ctx) ([]string, error)
Training.Traineddata(ctx) (*dagger.File, error)
Training.Evaluate(ctx) (string, error)          // lstmeval BCER / BWER
```

Recognition run backwards: the text is what you have and the model is what you
want. The apk package already ships every binary the job needs — `lstmtraining`,
`combine_tessdata`, `lstmeval`, `unicharset_extractor`, `text2image` — so what
stands between a directory of transcribed lines and a `.traineddata` is
orchestration, not installation.

The source directory holds pairs: `line-1.png` beside `line-1.gt.txt`, tesseract's
own training-data convention. The unit is **one text line** — one image, one
line of ground truth — which is why a `.gt.txt` carrying more than one line is
refused rather than joined. A page is not a training sample; it is as many
samples as it has lines, and cutting it into them is a decision about the data
rather than about this module.

The model that comes out pairs directly with `WithTessdata`, so a fine-tune and
the recognition that uses it are two calls on the same module. It is named after
the base model — fine-tuning `eng` gives an `eng.traineddata` that *replaces* the
stock one — and renamed by putting it in a directory under another name, since
`WithTessdata` reads the language off the file's stem.

### The base model has to be a float model

`WithBaseModel` is required and has no default, which looks unfriendly until you
try the friendly version: **every model Alpine packages is untrainable.** They
come from `tesseract-ocr/tessdata`, whose weights are quantized to integers so
recognition is fast, and `lstmtraining` refuses to continue from one —

```
Error, eng.lstm is an integer (fast) model, cannot continue training
```

— which names neither the float models nor how to get one onto the image. So a
default would be a default that always fails. The float builds live in
[`tesseract-ocr/tessdata_best`](https://github.com/tesseract-ocr/tessdata_best)
and reach this module the way any other unpackaged model does, through
`WithTessdata`:

```go
best := dag.Directory().WithFile("best.traineddata", dag.HTTP(tessdataBestEng))
model := dag.Tesseract().
    WithTessdata(best).
    Training(lines).
    WithBaseModel("best").
    Traineddata()
```

That failure is the one this module cannot prevent, only explain: the message
carrying `is an integer (fast) model` is caught and replaced with one naming
`tessdata_best` and `WithTessdata`.

### How a sample gets built

`lstm.train` takes its ground truth from a **box file** beside the image, in the
`WordStr` format — one line of text plus the region of the image it occupies.
Since each image here *is* one line, that region is the whole image, so the only
thing to discover is how big the image is.

That discovery happens in Go rather than in the container, because nothing on the
toolchain image reports an image's dimensions: tesseract will, but only by
recognising the page first, and everything else means installing an image toolkit
(`imagemagick` is +26MB) to read two integers out of a header. The source
directory is exported once, headers are decoded with `image.DecodeConfig` plus
`golang.org/x/image` — and a hand-rolled PNM reader, so the training set accepts
the same extensions `Batch` does rather than a smaller undocumented set — and the
box files are staged through `dag.CurrentModule().Workdir` under a
content-addressed name, so the same training set resolves to the same path every
time.

Oversized boxes were tried first and are a trap worth recording: tesseract clips
the crop to the image, so the *sample* comes out byte-identical, but the box is
stored in `int16` coordinates and `GetRectImage` pads it by 4 before clipping —
so `32767` overflows to a negative box and the run dies with `Failed to read
pages`, while `1000000` silently truncates to `16960` and appears to work.

### One exec, one cache entry

Extraction, sample building, training and freezing are a single container exec.
The intermediates are worthless on their own and large — the extracted network is
~12MB and `lstmtraining`'s checkpoints ~70MB — and keeping them inside one
container keeps the whole run one cache entry, which is what lets `Traineddata`
and `Evaluate` share it instead of training twice.

Checks run cheapest first, and the base model is checked *last* on purpose: it is
the only one that needs the image assembled to answer, so a directory that is not
a training set says so without building anything.

### Iterations, and what `Evaluate` measures

One iteration is one sample presented to the network, so a 40-line set runs 40
iterations per pass over the data. The count is **always bounded** — `lstmtraining`
left to itself trains until its error rate stops improving, which on real data is
hours — and defaults to 100. That default is far below what fine-tuning for
production takes (upstream's own worked example uses 400 for a single font, and
thousands is ordinary); it is chosen so that a first call, and this module's own
test suite, finish in seconds. The end-to-end test runs at the default precisely
so that the day it stops being CI-viable is the day that test gets slow.

`Evaluate` returns `lstmeval`'s line — `BCER eval=0.000, BWER eval=0.000`,
character and word error rates as percentages — against the **training set**. That
is a measure of how well the model fit the data it was shown, not of how it will
do on data it has not seen. Hold ground truth back and build a second `Training`
over it for the second number (#231).

## Validation

Builders have no error return, so every check below is deferred to the output
call that would have used it. Each rejection names the legal set rather than
letting tesseract fail in its own vocabulary.

| Rejected | Why the raw failure is worse |
| --- | --- |
| `WithLanguage` naming a language neither installed nor supplied | tesseract talks about traineddata paths and `TESSDATA_PREFIX`, never about the fact that models arrive via `New` or `WithTessdata` |
| `Osd` with no `osd` model installed or supplied | same, plus the fix is a different function's argument |
| a `.pdf` source to `Document` or `Batch` | Leptonica has no PDF support and reports the file's first line as if it were a file name it could not open; the message names the `pdf` module's render call and `Batch`, so the fix is two lines rather than an errand |
| `WithParameter` with an empty name, or one containing `=` | `-c` takes `name=value`, so an embedded `=` silently sets a *different* variable |
| `WithParameter` naming an unknown control variable | tesseract only warns (`Warning: The parameter '...' was not found.`) and exits 0, so a typo is indistinguishable from a no-op |
| `WithDpi` at zero or negative | tesseract would take it as a real measurement and scale its analysis by it |
| a `Batch` glob matching nothing | an empty directory is indistinguishable from a batch that ran and found no text, so a typo would surface much later as missing output |
| a `Batch` source holding no images | same, but the fix is `WithGlob` rather than the pattern, so the message names the default extension set |
| two `Batch` inputs sharing an output base | `a.png` and `a.jpg` both render onto `a.txt`; the second silently overwrites the first and the run looks like it succeeded with a page missing |
| a `Training` file name containing a tab or newline | its manifest is tab-separated and newline-delimited, so the sample-building loop would act on the wrong file. `Batch` needed this too until its manifest went away |
| a `Training` image with no `.gt.txt`, or a `.gt.txt` with no image | training directories are assembled by script and fail off-by-one; "something is unpaired" sends the caller to diff two file listings, the file's name does not |
| two `Training` images pairing with one `.gt.txt` | `a.png` and `a.jpg` would build two samples from one transcription, and only one `.box` |
| a `Training` `.gt.txt` that is empty or holds several lines | the box claims the whole image renders exactly this text; against a two-line image that is false in a way training cannot recover from — the network is shown two lines of pixels and told they are one line of characters |
| `WithBaseModel` unset, or naming a model the image does not carry | there is no default that works, because every packaged model is quantized; the message names `tessdata_best` and `WithTessdata` |
| `WithBaseModel` naming a quantized model | `lstmtraining` says only `is an integer (fast) model`, which names neither the float builds nor how to get one onto the image |
| `WithIterations` at zero or negative | `lstmtraining` would write no checkpoint, and `--stop_training` would fail on its absence rather than on the argument |
| `LstmTrain` with empty or multi-line ground truth | the box claims the whole image renders exactly this text, which a multi-line image makes false in a way training cannot recover from |
| `WithMinConfidence` outside 1–100 | confidences are percentages: a bar of 0 passes every page and one above 100 fails every page, so either is a gate that reports a verdict it never reached |

Errors fold tesseract's own output in via `Expect: ReturnTypeAny` plus a
combined stdout+stderr helper, because tesseract splits usage errors onto stderr
and progress onto stdout.

## Caching

No `+cache=` directive appears on any `Document`, `Batch`, `Ci` or `Training` output:
recognition is a pure function of the image bytes plus the flags, and so is
fine-tuning of the samples plus the base model, so the 7-day default is correct
and there is no chained-method propagation problem to worry about. `Container`,
`Version`, `Langs` and `Parameters` take `+cache="session"` per `kicad`, because
a floating Alpine tag can resolve differently across sessions.

What *is* worth knowing is the granularity underneath those functions. A `Batch`
recognises each image in its own exec mounting only that image, so the engine
caches per page: the second export of a fifty-page folder with one page edited
re-recognises one page. `Training` is the opposite by nature — one exec for the
whole run, one cache entry, see [One exec, one cache entry](#one-exec-one-cache-entry)
— because a fine-tune is not separable into per-sample work.

## Follow-ups

An `examples/go` cookbook (#224); evaluation against a held-out set (#231);
authenticating to a private *container* registry for the mirrored Alpine image
(#235), which the apk options above deliberately do not cover — they configure
the package fetch, not the image pull.

The chained `Ci` builder (#223) and the training toolchain (#222) both landed —
see above. `Ci` deliberately exposes only `WithLanguage` of the seven recognition
options `Batch` carries: the rest describe *how one scan is read* — a page
segmentation mode, a DPI, a control variable — and a pipeline whose input is a
whole intake folder has no single answer for them. A caller who needs one is
configuring a `Batch`, not a CI. `WithGlob` is absent for the same reason from
the other direction: the default takes the images and leaves the manifests, which
is what pointing this at an existing scan folder should do.

`WithConcurrency` is not in that category — it changes how long the same run
takes and nothing about how a scan is read — but `Ci` still does not forward it,
because it inherits the default (one recognition per CPU) and narrowing that is
a different request. #305 is the open question of whether it should, given `Run`
makes two batch passes that may want different answers.

Batch OCR over a directory (#220) landed, and so did PDF-input rasterization
(#221) — which #276 then removed once the `pdf` module existed to render pages
and #298 made `Batch` recognise them concurrently. Nothing here exposes
tesseract's concatenated list-file output any more: it is a serial pass by
construction, which is the opposite of what the primary use case wants. A caller
who needs one document back reassembles it — `pdf.Merge` for the searchable PDF,
their own reader for the per-page TSV — and that is a deliberate boundary rather
than a gap. If it turns out to want a first-party helper, it is a function on
the `pdf` module, not a flag on `Export`.

GPU acceleration is **not** a follow-up: tesseract has no CUDA path and upstream
does not plan one, Alpine builds without OpenCL, and Dagger's GPU passthrough is
NVIDIA-only by construction. Recognition is CPU-bound here by design, not by
omission.
