# pdf

Daggerverse module that reads and renders PDFs with
[poppler](https://poppler.freedesktop.org/) as a `dagger call`. Bind a PDF and
get back its text — exactly, in reading order, physical layout or content-stream
order — or its pages as PNG, JPEG or TIFF images, at a resolution or a pixel
size you choose, in colour, grayscale or bilevel. Keep the type outlines instead
and it gives you SVG, EPS or PostScript; ask for markup and it gives you HTML
with the page's images beside it. Ask it what the document is and it tells you:
page count, page size, PDF version, whether it is encrypted and under which
algorithm.

**There is no official poppler container image**, so this module assembles its
own the way `tesseract` and `qemu` do rather than pinning a vendor image the way
`kicad` does: a module-pinned Alpine (`3.24`, whose main repository carries
poppler-utils 25.12.0) plus `apk add poppler-utils ttf-liberation`. Assembling
an image that way is **two** fetches, not one: `registry` overrides where the
*image* comes from, and `WithApkRepository` overrides where the *packages*
installed onto it come from — see
[Air-gapped installs](#air-gapped-installs--withapkrepository-withapkkey-withapkauth).

Nothing here opens a port. The secrets it handles are the document's passwords
and `WithApkAuth`.

## Try this module first, and `tesseract` only if it comes back empty

`pdftotext` reads the text layer a PDF **already carries**. It is exact,
instant, and involves no recognition: the characters it returns are the
characters the document says are there, not a model's best guess at some pixels.

A scan carries no text layer at all, and that is the case the
[`tesseract`](../tesseract) module exists for. This module's job for a scan is
to produce the page images OCR runs on:

```go
doc := dag.Pdf().Document(source)

text, _ := doc.Convert().Text(ctx)
if strings.TrimSpace(strings.ReplaceAll(text, "\f", "")) == "" {
    // No text layer. Render at OCR resolution and recognise the pages —
    // one .txt per page, at the page names this module guarantees.
    pages := doc.Convert().WithDpi(300).WithColorMode(dagger.PdfColorModeGray).Png()
    results := dag.Tesseract().Batch(pages).Export([]dagger.TesseractFormat{
        dagger.TesseractFormatTxt,
    })
    // ...
}
```

`Text` returning nothing is the **only** signal a caller gets, and it is not an
error: from poppler's point of view a page with no text is a perfectly good page
that has no text on it. A module that failed there instead would make the two
paths impossible to choose between programmatically, which is why
`TextOnImageOnlyPdfReturnsNothing` is a test rather than a footnote.

Everything downstream of that branch is why the page-naming contract below
exists at all.

## The font is not decoration

Poppler draws a PDF that names one of the base-14 fonts **without embedding
it** — the usual shape for anything not born as a scan — by asking fontconfig
for a substitute. With no font installed there is nothing to substitute, and the
failure is silent: the page renders **blank**, anything reading it finds
nothing, and the command **exits 0**.

`ttf-liberation` is therefore installed unconditionally. It is the right family
for the job because it is metric-compatible with Helvetica, Times and Courier,
so substituted text keeps the positions the document was written with rather
than reflowing into a different set of line breaks. It also brings fontconfig,
which is what makes `WithFonts` work.

## Faces Alpine has no package for — `WithFonts`

```go
Pdf.WithFonts(dir *dagger.Directory) *Pdf
```

The packaged family is a floor, not a ceiling, and it fails the same way for the
same reason: a document naming a corporate face or a CJK family that fontconfig
cannot resolve renders without those glyphs and succeeds. Supplying the face is
the fix.

The directory is mounted under `/usr/share/fonts`, the one tree
`/etc/fonts/fonts.conf` tells fontconfig to scan, so a supplied face is
indistinguishable from a packaged one as far as poppler is concerned. It is
mounted rather than copied, so a large family does not become an image layer.

```go
dag.Pdf().WithFonts(host.Directory("./fonts"))
```

## Air-gapped installs — `WithApkRepository`, `WithApkKey`, `WithApkAuth`

```go
Pdf.WithApkRepository(url string) *Pdf      // repeatable, in preference order
Pdf.WithApkKey(key *dagger.File) *Pdf       // repeatable
Pdf.WithApkAuth(credentials *dagger.Secret) *Pdf
```

`New`'s `registry` argument moves the **image**. It is not enough on its own and
never was: the packages are still fetched by `apk add` from whatever
`/etc/apk/repositories` carries, so mirroring Alpine into a private registry
buys a container that then fails on its first `apk add` — or, where the CDN is
blackholed rather than refused, hangs until it times out.

The first `WithApkRepository` call **replaces** the image's list rather than
appending to it, because the air-gapped case needs the unreachable defaults gone
rather than merely deprioritized: a repository apk still consults is a
repository apk still waits for.

A repository's index is signed, so pair it with `WithApkKey` unless the mirror
is signed by a key the base image already trusts. The key file keeps its own
name, because the name is load-bearing — an index signature names the key file
it was made with.

`WithApkAuth` takes netrc-formatted credentials as a `*dagger.Secret`, which is
what apk-tools 3's built-in libfetch reads when a repository answers 401. It is
mounted rather than written, so the credentials stay out of the cache key, out
of argv, out of the image's environment and out of any layer a caller exports.
That is also why they are not simply userinfo in the repository URL, which would
land them in `/etc/apk/repositories` and in every apk error message quoting it.

```go
dag.Pdf().
    WithApkRepository("https://mirror.example.com/alpine/v3.24/main").
    WithApkKey(host.File("./keys/mirror.rsa.pub")).
    WithApkAuth(mirrorNetrc)
```

## Toolchain

```go
New(registry="docker.io", alpineTag="3.24") (*Pdf, error)
Pdf.Container() *dagger.Container
Pdf.Version(ctx) (string, error)
```

`Container` is the escape hatch. poppler-utils ships **thirteen** binaries and
this module wraps three of them — `pdftotext`, `pdftoppm` and `pdfinfo` — so
`pdfimages`, `pdfseparate`, `pdfunite`, `pdfsig`, `pdffonts`, `pdftocairo`,
`pdftohtml`, `pdftops`, `pdfattach` and `pdfdetach` stay reachable via
`container with-exec` until they get wrapped too (see
[Follow-ups](#follow-ups)).

`Version` reads **stderr**, not stdout: poppler's tools print their version
banner on stderr and still exit 0, so a module reading stdout would report the
empty string for every image it ever built.

## Document — a file, not a directory

```go
Pdf.Document(source *dagger.File) *Document

Document.WithUserPassword(password *dagger.Secret) *Document
Document.WithOwnerPassword(password *dagger.Secret) *Document
Document.WithPageRange(first int, last int) *Document
Document.Info(ctx) (string, error)
Document.PageCount(ctx) (int, error)
Document.Convert() *Convert
```

The boundary input is a `*dagger.File` rather than a `*dagger.Directory`, unlike
the `kicad` module's `Project`: a PDF resolves nothing relative to its own
location, so one file is the whole unit of work however many pages it carries.

`Document` is **immutable** — every `With*` returns a copy — so one bound
document branches into several outputs without the branches interfering:

```go
doc := dag.Pdf().Document(source).WithUserPassword(pw)

text  := doc.Convert().Text(ctx)                       // whole document
cover := doc.WithPageRange(1, 1).Convert().Png()       // just the cover
body  := doc.WithPageRange(2, 0).Convert().WithDpi(300).Png()
```

`Info` and `PageCount` describe the **document**, not a conversion of it, so
`WithPageRange` does not narrow them. That is also what makes `PageCount` usable
as the bound a range is validated against.

### Passwords

Passwords are `*dagger.Secret` and reach the tool as an environment variable the
shell expands — `-upw "$PDF_USER_PASSWORD"` — never as a `-upw <password>` argv
word: **argv is visible in every Dagger trace and error message.** Same
reasoning as mounting apk credentials as a netrc file rather than putting
userinfo in a repository URL.

The user password grants reading. The owner password grants reading too and
additionally clears the permission bits poppler otherwise honours, which is what
a caller entitled to ignore `copy:no` reaches for. Setting both is fine.

> **Poppler reads only the first 32 bytes of a password.** It truncates to the
> limit PDF revisions before 2.0 imposed, even for an AES-256 document whose own
> limit is 127. A document encrypted under a longer password by a tool that
> honours the full length — `qpdf` does — is one poppler reports `Incorrect
> password` for no matter what is passed here, and no argument to this module can
> open it.

An encrypted document with no password, or the wrong one, is reported by naming
the **encryption** rather than by passing poppler's message through. poppler
says `Incorrect password` either way — it tries the empty password when given
none — so passing that through would tell a caller who supplied nothing that
what they supplied was wrong.

### `WithPageRange`

Bounds are **1-based and inclusive**, matching poppler's own `-f`/`-l`, so
`WithPageRange(2, 3)` is the second and third pages and `WithPageRange(1, 1)` is
the first page alone. A zero `last` means "to the last page", which is the only
way to spell an open-ended range without first asking how many pages there are.

It narrows the text outputs and the raster outputs alike.

Bounds are checked against the document and rejected by naming the bound, at the
point of use rather than in the builder — the check needs the page count, which
needs the document opened, which needs a password the builder cannot see the
future of. The out-of-document cases are what justify checking at all: poppler
renders nothing for them and **exits 0**, so a caller who asked for page 20 of a
12-page document would otherwise get an empty result indistinguishable from a
document with no text in it.

| rejected | message names |
| --- | --- |
| `WithPageRange(0, 3)` | `first must be at least 1` |
| `WithPageRange(1, -2)` | `last must not be negative` |
| `WithPageRange(5, 3)` | `last (3) must not precede first (5)` |
| `WithPageRange(20, 0)` on 12 pages | `first (20) is past the end of the document, which has 12 pages` |
| `WithPageRange(1, 13)` on 12 pages | `last (13) is past the end …` |

## Convert — the render options and the nine outputs

```go
Document.Convert() *Convert

Convert.WithDpi(dpi int) *Convert
Convert.WithColorMode(mode ColorMode) *Convert
Convert.WithScaleTo(pixels int) *Convert
Convert.WithoutAnnotations() *Convert

Convert.Text(ctx, layout LayoutMode, disablePageBreaks bool) (string, error)
Convert.Txt(ctx, layout LayoutMode, disablePageBreaks bool) (*dagger.File, error)
Convert.Png(ctx) (*dagger.Directory, error)
Convert.Jpeg(ctx) (*dagger.Directory, error)
Convert.Tiff(ctx) (*dagger.Directory, error)
Convert.Svg(ctx) (*dagger.Directory, error)
Convert.Eps(ctx) (*dagger.Directory, error)
Convert.Ps(ctx) (*dagger.File, error)
Convert.Html(ctx) (*dagger.Directory, error)
```

The render options live on `Convert` rather than on `Document` because the three
raster outputs share them and because they describe a *conversion* rather than
the document being converted — `WithDpi` says nothing about the PDF.

Which options an output actually reads depends on the renderer behind it:

| output | renderer | reads |
| --- | --- | --- |
| `Text`, `Txt` | `pdftotext` | `layout`, `disablePageBreaks` |
| `Png`, `Jpeg`, `Tiff` | `pdftoppm` | `WithDpi`, `WithColorMode`, `WithScaleTo`, `WithoutAnnotations` |
| `Svg`, `Eps`, `Ps` | `pdftocairo` | `WithDpi` |
| `Html` | `pdftohtml` | — |

Everything an output does not read is a **documented no-op** rather than a
rejection. Unlike a page bound past the end of the document, which is always a
mistake, a DPI set before asking for text is inapplicable rather than wrong — the
natural shape of "render this at 300 and also give me the text" should not have
to un-set an option to ask the second question.

> For the `pdftocairo` outputs that is a decision with teeth, because **poppler
> does not ignore those flags — it refuses**: `-mono may only be used with the
> -png, -jpeg, or -tiff output options`, exit 99, nothing written. And
> `-hide-annotations` is not a `pdftocairo` option at all. So the flags are
> dropped here rather than forwarded, and "render this as PNG for OCR and as SVG
> for the web view" works off one `Convert`.

`WithDpi` still reaches `pdftocairo`, where it governs the **rasterized regions**
a vector output can still contain — an embedded photograph has no vector form to
keep — and a non-positive value is still rejected by name.

`Convert` is immutable too, so one configuration fans out into several formats:

```go
conv := doc.Convert().WithDpi(300).WithColorMode(dagger.PdfColorModeMono)
png  := conv.Png()
tiff := conv.Tiff()
```

### Text and `Txt`

`Text` returns a string and `Txt` the same bytes as a `*dagger.File`. Which to
reach for is plumbing and nothing more: a file composes with whatever consumes
files without the bytes passing through the caller.

Pages are separated by a form feed (`U+000C`), **including after the last one**,
which is pdftotext's own convention. `disablePageBreaks` drops them, for a
consumer that wants one flat stream.

`disablePageBreaks` is inverted rather than spelled `pageBreaks bool
+default=true` because **a `+default=true` bool cannot be set false from the Go
SDK**: the zero value is dropped before it reaches the API, so no caller could
ever have turned page breaks off. `WithoutAnnotations` is named for what it
removes for the same reason.

### `LayoutMode`

```go
LayoutModeReading   // reading order, pdftotext's default — no flag
LayoutModePhysical  // -layout, preserves columns and tables
LayoutModeRaw       // -raw, content-stream order
```

Three different answers rather than degrees of one. On a two-column page whose
content stream is written row-major while the page reads column-major, all three
disagree:

| mode | output |
| --- | --- |
| `READING` | the whole left column, then the whole right column |
| `PHYSICAL` | `ALPHA opens the left column here     XRAY opens the right column here` |
| `RAW` | `ALPHA opens the left column here XRAY opens the right column here` |

A table wants `PHYSICAL`, prose in columns wants `READING`, and a caller
reconstructing the content stream wants `RAW`.

### `ColorMode`

```go
ColorModeColor  // full colour, pdftoppm's default — no flag
ColorModeGray   // -gray
ColorModeMono   // -mono, bilevel
```

`GRAY` and `MONO` exist for the OCR path: grayscale and binarized input is what
recognition preprocessing wants, and producing it here is cheaper and more
predictable than letting the recognizer threshold a colour image itself.

> **The mode changes the pixels, not the file format.** Poppler's PNG writer
> emits 8-bit RGB whatever mode was asked for, so a grayscale render is an RGB
> image whose channels are equal and a monochrome one an RGB image whose pixels
> are pure black or white — with flat tones **dithered** rather than averaged. A
> bit-depth assertion would pass identically for all three and prove nothing,
> which is why this module's own test measures pixels.

### `WithDpi` and `WithScaleTo`

`WithDpi` defaults to **150** — pdftoppm's own default, and a screen-reading
resolution. A caller handing pages to `tesseract` wants **300**, which is what
tesseract's quality documentation asks for and what scanners are set to for the
same reason: its models were trained on characters of a certain pixel height, and
body text below 300 dpi costs accuracy. Cost rises roughly with the square, so
600 dpi is four times the pixels for very little further gain.

`WithScaleTo` fixes the output's pixel size instead, scaling each page to fit
inside a square of the given side while keeping its aspect ratio — a US Letter
page scaled to 500 comes out 386×500. Reach for it when the consumer has a size
budget, and for DPI when the consumer cares about the physical scale of what is
on the page, which is every OCR and measurement case.

**`WithScaleTo` overrides `WithDpi`**, and does so in this module rather than by
relying on poppler's flag precedence: the resolution flag is left off the command
line entirely when a scale is set, so which one wins is a fact about this code.

Both reject a non-positive value by naming the argument (`WithDpi: dpi must be
positive, got 0`). Left to poppler these arrive much later as a complaint about
`-r` or `-scale-to`, which names a flag the caller never wrote.

### `Svg`, `Eps` and `Ps` — keeping the outlines

`Png` gives you pixels of the type. These give you the type: paths a browser, a
plotter or a vector editor can scale, restyle and re-flow.

Text arrives as **glyph outlines, not `<text>`** — poppler converts every glyph
to a `<path>` and places it with `<use>` — so the page is faithful and is not
searchable. `Text` is what to reach for when the words are what is wanted.

**One SVG per page is this module's doing, and it matters.** `pdftocairo -svg`
run once over a twelve-page document writes a single file holding all twelve,
wrapped in the SVG 1.2 `<pageSet>`/`<page>` elements — which no browser, librsvg
or Inkscape implements. The pages are in the file and invisible to everything
that draws it. Rendering each page on its own invocation is what makes them
reachable, and `SvgWritesOneVectorFilePerPage` asserts the absence of a
`<pageSet>` for exactly that reason.

**EPS refuses multi-page outright.** `pdftocairo -eps` handed a document with a
second page writes nothing and exits 99 with `EPS files can only contain one
page.` The per-page loop is what turns that refusal into the whole document.
Note that an EPS's bounding box is the page's **ink**, not its media box —
poppler crops to what is drawn, so a US Letter page with one line of text on it
comes out a few inches wide. That is EPS's own convention (a figure carries its
extent so the placing document can size it) and the one place these files
disagree with the geometry `Png` reports.

**`Ps` returns a `*dagger.File`, not a directory**, because PostScript is a
multi-page format: one document with a `%%Pages:` count and a `%%Page:` marker
per page. A directory of one-page fragments would look tidier beside `Svg` and
`Eps` and be individually invalid.

### `Html` — markup with the images beside it

`Html` returns a directory of `page-0001.html`, `page-0002.html`, … **plus each
page's extracted images**, which are the only entries in this module that do not
follow the page contract:

```
page-0001.html
page-0001-1_1.png   ← referenced from page-0001.html as src="page-0001-1_1.png"
page-0002.html
```

Those image names are `pdftohtml`'s, and they are left alone precisely because
they are written *into* the markup — renaming them to a contract would mean
rewriting the HTML to match. **A consumer walking this directory should select
`page-*.html` rather than assume every entry is a page.**

The conversion runs with the output directory as its **working directory**, and
that is load-bearing: `pdftohtml` writes the output name it was given straight
into every `img src`, so an absolute output base — the obvious way to write into
a staging directory — produces markup whose images resolve only on the machine
that rendered them, while still passing every assertion about entry names.
`HtmlCarriesPageMarkupAndItsImages` checks the `src` for that reason.

`-noframes` is passed unconditionally. `pdftohtml`'s default is a three-file
frameset — a frameset, an index and the content — of which only the last carries
anything.

## Page naming — `page-0001.png`

`Png`, `Jpeg`, `Tiff`, `Svg`, `Eps` and `Html` return a directory of
`page-0001.<ext>`, `page-0002.<ext>`, … zero-padded to at least four digits and
uniform within a document. The extensions are **poppler's** spellings, not this
module's:

| output | extension |
| --- | --- |
| `Png` | `page-0001.png` |
| `Jpeg` | `page-0001.jpg` |
| `Tiff` | `page-0001.tif` |
| `Svg` | `page-0001.svg` |
| `Eps` | `page-0001.eps` |
| `Html` | `page-0001.html` (+ its images, see above) |

The raster family and the per-page family reach that contract from opposite
directions. `pdftoppm` names the files and this module **renames** them; for
`Svg`, `Eps` and `Html` the module drives the page loop itself and **names them
outright**, one invocation per page, because neither `pdftocairo` nor
`pdftohtml` will produce a usable page-per-file set on its own. The upper bound
of that loop — a `WithPageRange` open-ended `last`, or no range at all — is
resolved from `pdfinfo` **inside the same exec** that renders, so the whole
conversion stays one exec either way.

For the raster family in particular, **this is a normalization, not
`pdftoppm`'s own behaviour.** `pdftoppm` pads a
page number to the width of the *document's* page count, so a 9-page document
yields `page-1.png` and a 10-page one `page-01.png` — and a 9-page *range* of a
12-page document yields `page-01.png` too, because the width follows the
document rather than what was rendered.

Lexicographic order is therefore page order **only within a single document**,
and a consumer that sorts what it is handed — `tesseract`'s `Batch` does a bare
`sort.Strings` — has no way to know which width it is holding. A caller that
hardcodes one name shape breaks on the next document. Padding to a fixed minimum
makes the shape a contract instead of a consequence, and that is what lets this
directory be handed to another module at all.

A rename pass runs **in the same exec** as the render, so the directory never
existed under the other names. The width is the greater of four and whatever
`pdftoppm` chose, so a document longer than four digits can express stays
uniform rather than being truncated into ambiguity.

Page numbers are the **source document's**, not positions within a range, so
`WithPageRange(4, 6)` yields `page-0004`…`page-0006`. That keeps a rendered page
traceable back to the page it came from.

`-forcenum` is passed unconditionally to `pdftoppm`. On poppler 25.12 — what the
pinned tag ships — it changes nothing, because a page number is always appended.
It is there for a caller who overrode `alpineTag` onto an older release, where a
single-page render was named `page.png` with no number at all, breaking the
contract for exactly the documents most likely to be one page. The per-page
family needs no equivalent, naming its own output.

### Why `pdftoppm` for raster and `pdftocairo` for vector

Both render PNG, JPEG and TIFF, and both are now in this module — but for
disjoint formats, not as alternatives.

Only `pdftoppm` offers `-mono` and `-gray`, and binarized or grayscale input is
exactly what OCR preprocessing wants, so the raster family is its. Only
`pdftocairo` emits SVG, EPS and PostScript, so the vector family is its. Neither
is a drop-in for the other: they are separate rasterizers with different fidelity
characteristics, which is why *choosing* between them for a format both can
produce is still a follow-up (#277) rather than something this module does.

## Caching

No cache directive appears anywhere in this module. Every function is a pure
transform of its inputs — the document bytes plus the flags — so Dagger's 7-day
default is correct and there is no chained-method propagation problem to worry
about. Same posture as `tesseract`'s outputs and `kicad`.

## Follow-ups

Reporting a document's fonts, metadata and signatures (#268);
text-layer geometry as bbox HTML and TSV (#269); splitting and merging pages
(#270); extracting embedded images and attachments (#271); cropping the render
window and selecting odd or even pages (#272); the chained `Ci` builder (#273);
linearize, encrypt and repair via qpdf (#274); testing package assembly off a
private apk mirror (#275); removing `tesseract`'s `FromPdf` in favour of this
module (#276); renderer fidelity and colour-profile options (#277).

Deliberately **not** planned: ghostscript or mupdf rendering backends, and
`pdfattach`. A second rasterizer buys different rendering bugs rather than
different capability, and attaching a file to a PDF is a document-authoring
operation rather than a reading one — this module reads and renders.
