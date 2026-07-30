# pdf

Daggerverse module that reads and renders PDFs with
[poppler](https://poppler.freedesktop.org/) as a `dagger call`. Bind a PDF and
get back its text — exactly, in reading order, physical layout or content-stream
order, or with a bounding box per word so you know where on the page each one
was — or its pages as PNG, JPEG or TIFF images, at a resolution or a pixel
size you choose, in colour, grayscale or bilevel. Keep the type outlines instead
and it gives you SVG, EPS or PostScript; ask for markup and it gives you HTML
with the page's images beside it. Ask it what the document is and it tells you:
page count, page size, PDF version, whether it is encrypted and under which
algorithm, which fonts it needs and whether they are embedded, what its XMP
metadata says, and whether its signatures check out. Or leave it a PDF and change
its page structure instead: `Split` takes a document apart a page per file,
`Merge` puts several back together in the order you name them. Or take out what
the file already carries rather than a conversion of it: `EmbeddedImages` pulls
the image objects off the pages at the resolution they are stored at, and
`Attachments` pulls out the files hung off the document — the machine-readable
XML an invoice carries next to its printable page.

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
Pdf.Merge(ctx, sources []*dagger.File) (*dagger.File, error)
```

`Container` is the escape hatch. poppler-utils ships **thirteen** binaries and
this module wraps eleven of them — `pdftotext`, `pdftoppm`, `pdfinfo`,
`pdftocairo`, `pdftohtml`, `pdffonts`, `pdfsig`, `pdfseparate`, `pdfunite`,
`pdfimages` and `pdfdetach` — so `pdftops` and `pdfattach` stay reachable via
`container with-exec`. `pdfattach` is deliberately not planned (see
[Follow-ups](#follow-ups)). So are the flags of a wrapped tool that this module
does not surface, `pdffonts -subst` and `pdfdetach -savefile` among them.

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
Document.Fonts(ctx) (string, error)
Document.Metadata(ctx) (string, error)
Document.Signatures(ctx) (string, error)
Document.Split(ctx) (*dagger.Directory, error)
Document.EmbeddedImages(ctx, format ImageFormat) (*dagger.Directory, error)
Document.Attachments(ctx) (*dagger.Directory, error)
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
as the bound a range is validated against. `Fonts`, `Metadata` and `Signatures`
are the other three reports — see [The four reports](#the-four-reports).

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

It narrows the text outputs, the raster outputs, `Split` and `EmbeddedImages`
alike. It does not narrow `Info`, `PageCount`, `Metadata`, `Signatures` or
`Attachments`, all of which describe the document rather than a slice of it.

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

## The four reports

Four functions describe the document instead of converting it, one per poppler
tool. Each returns its tool's report as text.

| function | tool | page range | passwords |
| --- | --- | --- | --- |
| `Info` | `pdfinfo` | no — describes the document | yes |
| `Fonts` | `pdffonts` | **yes** — which faces a document needs is a question about pages | yes |
| `Metadata` | `pdfinfo -meta` | no — the packet is document-level | yes |
| `Signatures` | `pdfsig` | no — a signature covers byte ranges, and the tool has no page bounds | yes |

**They are not parsed, deliberately.** The formats are stable but wide, and a
caller who wants one field can grep it out of these lines more cheaply than this
module can model all of them. Structured values would mean four schemas to keep
in step with poppler's output for no capability that grepping does not already
have.

### `Fonts` — the blank-page diagnostic, before the blank page

```
name                                 type              encoding         emb sub uni object ID
------------------------------------ ----------------- ---------------- --- --- --- ---------
Helvetica                            Type 1            WinAnsi          no  no  no       3  0
```

The `emb` column is the one to read, and it is the direct diagnostic for the
failure [the font install](#the-font-is-not-decoration) exists to prevent: `no`
means the file names that face without carrying it, so the render depends on the
image's own fonts — the packaged family, or one supplied through `WithFonts`. Get
that wrong and the page renders **blank while exiting 0**. This report is what
that looks like beforehand.

`WithPageRange` narrows it, because a face used only on page 40 is genuinely
absent from a report of pages 1 through 3. Out-of-document bounds are rejected
here the same way they are everywhere else — poppler would print the header and
exit 0, which reads as a document that needs no fonts at all.

Which substitute poppler would *actually* pick for an unembedded face is a
different question, answered by `pdffonts -subst` through
[`Container`](#toolchain).

### `Metadata` — the XMP packet, not the Info dictionary

`Metadata` returns the RDF/XML block a producer writes its own record of the
document into, exactly as it sits in the file, ready for an XML parser. It is
worth having next to `Info` because the two carry different things and disagree
in the wild: `Info` reports the PDF's Info **dictionary**, whose handful of keys a
producer may have left behind when it rewrote the XMP, and the packet is the
record a downstream asset system reads.

A document carrying **no** packet is not an error and does not come back empty.
poppler prints nothing at all and exits 0 for that, and an empty string is
indistinguishable from a function that never ran, so the module answers:

```
No XMP metadata: this document carries no XMP packet. Info reports the metadata in its Info dictionary.
```

### `Signatures` — reports, and does not gate

An unsigned document — which is nearly every document — gets the report saying
so, not a failure:

```
File '/work/source.pdf' does not contain any signatures
```

The path in it is where the module mounts the bound file, poppler naming the file
it was handed; the report is otherwise `pdfsig`'s own, verbatim.

That is the whole point of the function being callable on an arbitrary PDF: "is
this signed, and does it check out" is asked *before* the answer is known, so the
unsigned answer has to arrive as a result. It is also the one place this module
reads a non-zero exit as a report — `pdfsig` exits **2** for it.

It reports rather than gates. A signature that fails to validate comes back as a
report saying `Signature Validation: Signature is Invalid.`, not as an error, and
a caller that wants to stop on a bad signature reads the verdict out of the
report. Only a run that produced no report at all — an unreadable file, a missing
or wrong password — fails.

> `pdfsig` overloads its exit codes: **1** is both "a signature did not validate",
> having printed the whole report, and "could not open the document", having
> printed nothing. The module therefore decides on what was *printed*, not on the
> code.

What the report can say about a signer's **certificate** is limited by the image
and not by the document: chain validation needs a trust database, this image
carries none, and `pdfsig` says so on stderr — which is not part of the report.
Supply one with `-nssdir` through [`Container`](#toolchain).

## Convert — the render options and the eleven outputs

```go
Document.Convert() *Convert

Convert.WithDpi(dpi int) *Convert
Convert.WithColorMode(mode ColorMode) *Convert
Convert.WithScaleTo(pixels int) *Convert
Convert.WithoutAnnotations() *Convert

Convert.Text(ctx, layout LayoutMode, disablePageBreaks bool) (string, error)
Convert.Txt(ctx, layout LayoutMode, disablePageBreaks bool) (*dagger.File, error)
Convert.Bbox(ctx, withLayout bool) (*dagger.File, error)
Convert.Tsv(ctx) (*dagger.File, error)
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
| `Bbox`, `Tsv` | `pdftotext` | — |
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

### `Bbox` and `Tsv` — where the text *is*

`Text` returns words. Layout-aware post-processing needs where each word **was**,
and `Bbox` and `Tsv` report it. They are the text-layer analogues of the
[`tesseract`](../tesseract) module's `Hocr` and `Tsv` — the same shape of answer,
sourced from the document's own coordinates rather than from recognition, so the
boxes are **exact rather than estimated**. Reach for them when the words are not
the whole answer: redacting a region, cropping a figure, deciding which column a
phrase belongs to, or lining extracted text up against a render of the same page.

`Bbox` emits XHTML — a `page` element per page naming its size, holding a `word`
element per space-separated word:

```xml
<page width="612.000000" height="792.000000">
  <word xMin="72.000000" yMin="94.768000" xMax="128.040000" yMax="116.968000">Page</word>
  <word xMin="134.712000" yMin="94.768000" xMax="148.056000" yMax="116.968000">1</word>
</page>
```

`withLayout` wraps those in the `flow`, `block` and `line` elements poppler groups
them into, each carrying the box around everything under it — the same words at
the same coordinates, with the paragraph structure a caller reconstructing prose
needs and a caller reading word boxes pays parsing for.

> The report is a **whole XHTML document** — `-bbox` implies poppler's
> `-htmlmeta`, so the boxes arrive inside a `doc` element inside an `html` element
> with a doctype ahead of it. Read the `doc` subtree, not the file's root.

`Tsv` emits the twelve-column table tesseract's TSV renderer defines, header row
included: `level`, `page_num`, `par_num`, `block_num`, `line_num`, `word_num`,
`left`, `top`, `width`, `height`, `conf`, `text`. `level` says what a row is — **1**
a page, **3** a flow, **4** a line, **5** a word — and structural rows name
themselves in the text column:

```tsv
level	page_num	…	left	top	width	height	conf	text
1	1	…	0.000000	0.000000	612.000000	792.000000	-1	###PAGE###
4	1	…	72.000000	94.768000	222.816000	22.200000	-1	###LINE###
5	1	…	72.00	94.77	56.04	22.20	100	Page
```

Reach for `Tsv` over `Bbox` when the consumer is a table reader — a dataframe, a
spreadsheet, `awk` — and when **the page a row belongs to has to be recoverable
from the row itself**. That is the one thing this format carries and `Bbox` does
not: a `page` element states its size and never its number, so a narrowed `Bbox`
report is traceable only by position, while `page_num` is the page's number in the
whole document even under `WithPageRange` — pages 4 through 6 come back as 4, 5
and 6, not as 1, 2 and 3.

Coordinates are in **points, from the top-left of the page**, which is not where
the PDF's own coordinate system starts. A word's box is the font's
ascender-to-descender span around the baseline rather than the glyphs' ink, so a
one-glyph word is exactly as tall as a long one — 22.2 points at 24pt Helvetica.

A few of poppler's own details are worth knowing before writing an assertion
against either format:

- There is **no level-2 paragraph row**, the `par_num` column notwithstanding.
- `conf` is `-1` on structural rows and `100` on every word. This is extraction,
  not recognition: there is nothing to be uncertain about.
- Word rows round to **two** decimals where structural rows print six.
- A TSV page row's `left` and `top` are junk after the first page — poppler leaves
  the previous page's last word in them. The size is right; the position is not.
- A page with **no text layer** yields an empty `page` element and no word rows
  rather than a failure: poppler notes `no word list` on stderr and exits 0. That
  is the same boundary `Text` draws — no words is the signal to render the page
  and hand it to OCR, not a sign anything went wrong.

The render options are ignored, these being extractions and not renders, and so is
`LayoutMode`: the report's structure is poppler's own and is not the text's reading
order. `WithPageRange` narrows both.

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

## `Split` and `Merge` — page structure, not pixels

```go
Document.Split(ctx) (*dagger.Directory, error)
Pdf.Merge(ctx, sources []*dagger.File) (*dagger.File, error)
```

These two are the other half of "working with PDFs", and they need no rendering
at all. `Split` is `pdfseparate`: one PDF per page, named to the same
[page-naming contract](#page-naming--page-0001png) the render family honours,
`page-0001.pdf` onwards. `Merge` is `pdfunite`: several PDFs concatenated into
one.

They are **lossless** in a way no conversion is. Each split page carries the
original page's own objects across — its text layer, its fonts, its annotations,
its size — so a split page is still a searchable PDF and a round trip gives back
the document it started from:

```go
pages := dag.Pdf().Document(source).Split()

var keep []*dagger.File
for _, name := range wanted {          // page-0004.pdf, page-0007.pdf, …
    keep = append(keep, pages.File(name))
}
excerpt := dag.Pdf().Merge(keep)
```

`Merge` hangs off the **root** rather than off `Document` because it takes
several sources and no single one of them is "the" document — a bound document
carries passwords and a page range, and neither means anything for a merge. It
takes an ordered `[]*dagger.File` rather than a directory and a glob because
**merge order is meaning**: a directory has no order of its own, so a caller
would be relying on whatever file names it happens to carry, and that is a
contract nobody wrote down. The slice is where the caller states what the result
should read like.

One source is legal and produces that source's document, so a caller merging a
computed list does not have to special-case its length. An empty slice is not:
there is no document to return and no empty PDF worth inventing, and
`pdfunite`'s own answer to it is its usage text — a description of a command
line the caller never wrote. The module refuses it before the container starts,
naming what to pass instead.

`Merge` mounts its sources at `/in/source-0001.pdf`, `/in/source-0002.pdf`, … in
slice order, and a failure carries a legend saying so, because the only thing
`pdfunite` tells you about a source it could not read is that path:

```
Merge failed (exit 255):
Syntax Warning: May not be a PDF file (continuing anyway)
…
Syntax Error: Could not merge damaged documents ('/in/source-0002.pdf')
any /in/source-NNNN.pdf above is this module's mount for the NNNNth of the 2
sources, numbered from 1 in the order they were passed
```

> **Neither tool takes a password.** There is no `-upw` and no `-opw` on
> `pdfseparate` or on `pdfunite` — passing one is a *usage* error, not a wrong
> password — so an encrypted document is one these two cannot be made to open,
> and `WithUserPassword` does not change that. What poppler says about it is
> `Incorrect password`, the same sentence a genuinely wrong password produces
> everywhere else in this module, so `Split` and `Merge` report it as the
> limitation it is and point at decrypting the document first (`qpdf --decrypt`;
> wrapping qpdf is #274). This is the one place a password reaches some of the
> module's functions and not others.

Whole documents go in whole. `Merge` narrows nothing — `Split` is what produces
single pages to merge — and the pages keep their own size, so merging US Letter
with A4 yields a document whose pages differ in size, which is what a
concatenation should do.

## `EmbeddedImages` and `Attachments` — what the file carries, not a render

```go
Document.EmbeddedImages(ctx, format ImageFormat) (*dagger.Directory, error)
Document.Attachments(ctx) (*dagger.Directory, error)
```

Two extractions that are not renders, and are routinely mistaken for them.

### `EmbeddedImages` is not `Convert().Png()`

`Convert().Png()` **rasterizes a page**: it draws the text, the vectors, the
annotations and the images into a new bitmap at whatever `WithDpi` names, and the
pixels it produces have never existed before. `EmbeddedImages` returns the **image
objects the file contains**, decoded no further than it has to be. For a scanned
document that is the scan, at the resolution it was scanned at, with nothing
resampled on the way out — which makes it strictly better OCR input than any
resolution `Convert().Png()` can be asked for:

```go
doc := dag.Pdf().Document(scan)

// Better: the scan itself, at its own resolution.
images := doc.EmbeddedImages(dagger.PdfImageFormatPng)

// Worse for OCR: a re-rasterization of pixels that were already pixels.
pages := doc.Convert().WithDpi(300).Png()
```

The two are not interchangeable in the other direction either. A page of text and
vectors carries **no image object at all**, so `EmbeddedImages` returns nothing
for it and `Convert().Png()` is the only thing that draws it. Which is also why
`WithDpi`, `WithColorMode`, `WithScaleTo` and `WithoutAnnotations` are not
reachable from `EmbeddedImages`: they live on `Convert`, and there is nothing to
choose about the resolution of an image that already has one.

`EmbeddedImagesReturnsTheStoredImageNotTheRenderedPage` is the test that pins the
distinction, and it pins it on the numbers: `scan.pdf`'s image is 16x16, and a
render of the US Letter page it is drawn across is 1275x1650.

### `ImageFormat` — keep the encoding, or replace it

An embedded image already has an encoding, so every member either keeps it or
replaces it. There is no default: which one is right depends entirely on what
reads the result, and quietly re-encoding a caller's scan is not a decision this
module makes on their behalf.

| member | flags | encoded image | image with no writable encoding |
| --- | --- | --- | --- |
| `PNG` | `-png` | re-encoded as PNG | PNG |
| `TIFF` | `-tiff` | re-encoded as TIFF | TIFF |
| `ORIGINAL` | `-j -jp2 -jbig2 -ccitt` | **kept byte for byte** | netpbm — `.ppm`, `.pgm`, `.pbm` |
| `ALL` | `-all` | **kept byte for byte** | PNG, or TIFF where PNG cannot represent it |

"Kept byte for byte" is meant literally, and covers JPEG, JPEG 2000, JBIG2 and
CCITT G4 — the four encodings poppler can copy through instead of decoding.
`EmbeddedImagesKeepsTheOriginalEncoding` asserts it as byte equality against the
JPEG stream read out of the fixture itself, because a `.jpg` of the right
dimensions is also what a re-encode produces.

The **fallback** is the whole difference between `ORIGINAL` and `ALL`. A raw or
Flate-compressed bitmap — most PDFs not born as scans — has no encoding to keep,
and `ORIGINAL` writes netpbm for it: a `.ppm` in the result is not a failure. Pick
`ALL` when the images are of mixed encodings and every one of them has to be
readable by something ordinary.

### Image naming — `image-0000-page-0001.png`

A different contract from the page families, and named so it can never be mistaken
for one:

```
image-0000-page-0001.jpg   ← image 0, drawn on page 1
image-0001-page-0001.png
image-0002-page-0002.png
```

The number after `image-` is poppler's index for the image, 0-based — the `num`
column of `pdfimages -list` for an unnarrowed run. The number after `-page-` is
the **1-based source page** it was drawn on, the same promise `Split` makes. The
index comes first so lexicographic order is document order, and both are padded to
at least four digits by a rename pass in the same exec, for the reason the
[page contract](#page-naming--page-0001png) has one: `pdfimages` pads to three, so
the thousandth image of a document widens the field mid-directory.

There is **no one-file-per-page contract** here, and there cannot be: a page may
carry several images and most pages carry none.

`WithPageRange` narrows it, and moves exactly one of the two numbers. The page
number stays the document's; the image index restarts, being poppler's count for
the *run*. Pages 2–3 of a document whose first image is on page 1 come back
starting at `image-0000-page-0002`.

### `Attachments` — the files hung off the document

An embedded file is a whole file attached to the PDF, not part of any page's
content: the machine-readable XML a ZUGFeRD or Factur-X invoice carries beside its
printable page, a spreadsheet behind a report, a CAD file behind a drawing.
Nothing in `Convert` reaches it and neither does `EmbeddedImages` — this is the
only function that does.

The names are the **document's own**, and this is the one directory here that is
not renamed to a contract. An attachment's name is *data*: it is what the producer
called the file and what a consumer looks for, so `invoice.xml` stays
`invoice.xml`.

```go
attached := dag.Pdf().Document(invoice).Attachments()
xml := attached.File("invoice.xml")
```

`WithPageRange` does not narrow it — an embedded file hangs off the catalog, and
`pdfdetach` has no page bounds at all — and unlike `Split` and `Merge`, **the
passwords do reach it**: `pdfdetach` takes both `-upw` and `-opw`, as does
`pdfimages`.

> **One path-bearing attachment name loses them all.** poppler refuses the whole
> `-saveall` with `I/O Error: Preventing directory traversal` rather than skipping
> the one file, so a document naming an attachment `../elsewhere.xml` yields
> nothing at all. Nothing passed to this module changes that — the name is the
> document's — so the failure says so and names `pdfdetach -list` plus
> `pdfdetach -savefile <name> -o <path>` through `Container` as the way to fetch
> the rest one at a time.

### Nothing to extract is a failure, not an empty directory

Both functions **refuse** a document with nothing to give them:

```
EmbeddedImages: this document carries no image objects: there is nothing to
extract, and a page of text and vectors is drawn rather than extracted —
Convert().Png() is what renders one. `pdfimages -list`, through Container,
answers the same question without failing
```

That is a deliberate departure from `Signatures`, which reports an unsigned
document rather than failing on it, and the reason is the return type. `Signatures`
returns a string and has room to say "no signatures"; a `*dagger.Directory` has
none, and an empty one is exactly what an extraction that failed to write anything
also produces — while both tools exit 0 either way. So the module is the only place
that can tell a caller which of the two happened, and the narrowed message names
the pages that were asked for so a caller who narrowed to the wrong ones is not
told the whole document is empty. A caller sweeping many documents, where "no
attachments" is an ordinary outcome, wants either the `-list` reports through
`Container` or to tolerate this error.

## Page naming — `page-0001.png`

`Png`, `Jpeg`, `Tiff`, `Svg`, `Eps`, `Html` and `Split` return a directory of
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
| `Split` | `page-0001.pdf` |

`EmbeddedImages` is **not** on that list and honours a contract of its own —
[`image-0000-page-0001.png`](#image-naming--image-0000-page-0001png) — because an
image is not a page: several can sit on one page and most pages carry none.
`Attachments` is on no list at all, its names being the document's.

The families reach that contract from opposite directions. `pdftoppm` and
`pdfseparate` name the files and this module **renames** them; for `Svg`, `Eps`
and `Html` the module drives the page loop itself and **names them outright**,
one invocation per page, because neither `pdftocairo` nor `pdftohtml` will
produce a usable page-per-file set on its own. The upper bound of that loop — a
`WithPageRange` open-ended `last`, or no range at all — is resolved from
`pdfinfo` **inside the same exec** that renders, so the whole conversion stays
one exec either way.

`pdfseparate` is the one that would honour a padded name if it were asked:
handed a `page-%04d.pdf` destination pattern it zero-pads for you. It goes
through the rename pass anyway, because a fixed width in the pattern is a width
and not a *floor* — a document with more than 9999 pages would come out numbered
to two widths, which is the exact ambiguity the contract exists to remove.

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
existed under the other names. The width is the greater of four and whatever the
tool chose, so a document longer than four digits can express stays uniform
rather than being truncated into ambiguity.

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

Cropping the render window and selecting odd or even pages (#272); the chained
`Ci` builder (#273); linearize, encrypt and repair via qpdf (#274); renderer
fidelity and colour-profile options (#277).

`tesseract`'s `FromPdf` was removed in favour of this module (#276): that module
carries no rasterizer at all now, so `Png()` feeds its `Batch` directly — which
is why the page numbers this module writes are padded to a fixed width rather
than to whatever `pdftoppm` chose. Render at `WithDpi(300)` for OCR and pass the
same 300 to the batch; the resolution is not recoverable from the images. What
comes back is one artifact per page, so `Merge` is the other end of that flow:
it is what turns per-page searchable PDFs back into the document.

Deliberately **not** planned: ghostscript or mupdf rendering backends, and
`pdfattach`. A second rasterizer buys different rendering bugs rather than
different capability, and attaching a file to a PDF is a document-authoring
operation rather than a reading one — this module reads and renders.
