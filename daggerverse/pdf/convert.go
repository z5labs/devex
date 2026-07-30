package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"dagger/pdf/internal/dagger"
)

// rasterArgv0 is the `$0` the raster script runs under, so the format's
// extension reaches it as `$1` rather than being interpolated into the script
// text.
const rasterArgv0 = "pdf-raster"

// normalizeScript renames every page the tool just wrote so its number is
// zero-padded to a fixed width, and is the reason this directory can be handed
// to another module at all. It is the raster renders' pass and Split's alike:
// both write a page per file, and both leave the number unpadded for it.
//
// pdftoppm pads a page number to the width of the document's page count, so a
// 9-page document yields `page-1.png` and a 10-page one `page-01.png`.
// Lexicographic order is therefore page order within a single document and
// nothing more: a consumer that sorts what it is given — the tesseract module's
// Batch does a bare sort.Strings — has no way to know which width it is holding,
// and a caller who hardcodes one name shape breaks on the next document. Padding
// to a fixed minimum makes the shape a contract instead of a consequence.
//
// The width is the greater of the module's minimum and whatever pdftoppm chose,
// so a document longer than the minimum can express stays uniform rather than
// being truncated into ambiguity.
//
// It runs in the same exec as the render, so the directory this function returns
// has never existed under the other names.
var normalizeScript = strings.Join([]string{
	`ext="$1"`,
	`width=` + strconv.Itoa(minPageNumberWidth),
	`for f in ` + pageGlob + `; do`,
	`	[ -e "$f" ] || continue`,
	`	n=${f##*/` + pageNamePrefix + `}`,
	`	n=${n%."$ext"}`,
	`	if [ ${#n} -gt "$width" ]; then width=${#n}; fi`,
	`done`,
	`for f in ` + pageGlob + `; do`,
	`	[ -e "$f" ] || continue`,
	`	n=${f##*/` + pageNamePrefix + `}`,
	`	n=${n%."$ext"}`,
	// Leading zeros are stripped before printf sees the number: POSIX printf
	// reads a leading-zero argument as an octal constant, so `09` is not 9 but
	// a diagnostic.
	`	stripped=$(printf '%s' "$n" | sed 's/^0*//')`,
	`	[ -n "$stripped" ] || stripped=0`,
	`	padded=$(printf "%0${width}d" "$stripped")`,
	`	if [ "$n" != "$padded" ]; then mv "$f" "` + outputDir + `/` + pageNamePrefix + `$padded.$ext"; fi`,
	`done`,
}, "\n")

// pageGlob matches every page a render wrote, whatever width pdftoppm numbered
// it to. The extension comes from `$1` so one script serves all three formats.
const pageGlob = outputDir + `/` + pageNamePrefix + `*."$ext"`

// Convert is a conversion of a bound document: the options a render reads, and
// the outputs that read them.
//
// Immutable in the same way Document is, so one Convert branches into several
// formats — the same page range at the same resolution as PNG and as TIFF —
// without the branches interfering.
//
// The render options live here rather than on Document because three raster
// outputs share them, and because they describe a conversion rather than the
// document being converted. They have no meaning for Text and Txt and are
// documented no-ops there rather than rejections: a DPI set before asking for
// text is inapplicable rather than wrong, unlike a bound past the end of the
// document, which is always a mistake.
type Convert struct {
	// +private
	Document *Document
	// +private
	Dpi int
	// +private
	HasDpi bool
	// +private
	ColorMode ColorMode
	// +private
	ScaleTo int
	// +private
	HasScaleTo bool
	// +private
	HideAnnotations bool
}

// WithDpi sets the resolution pages are rendered at, in dots per inch.
//
// Unset, pages render at 150 dpi — pdftoppm's own default, and a
// screen-reading resolution. A caller handing these pages to the tesseract
// module wants 300, which is the resolution tesseract's own quality
// documentation asks for and the one scanners are set to for the same reason:
// its models were trained on characters of a certain pixel height, and body
// text below 300 dpi costs accuracy. Cost rises roughly with the square, so
// 600 dpi is four times the pixels of 300 for very little further gain.
//
// Ignored by Text and Txt, which read a text layer rather than rendering
// anything. Overridden by WithScaleTo, which fixes the output's pixel
// dimensions directly.
func (c *Convert) WithDpi(
	// Render resolution in dots per inch. Must be positive.
	dpi int,
) *Convert {
	out := c.clone()
	out.Dpi = dpi
	out.HasDpi = true
	return out
}

// WithColorMode sets the colour space pages are rendered in.
//
// Unset, pages render in full colour. GRAY and MONO exist for the OCR path:
// grayscale and binarized input is what recognition preprocessing wants, and
// producing it here is cheaper and more predictable than letting the recognizer
// threshold a colour image itself.
//
// Ignored by Text and Txt.
func (c *Convert) WithColorMode(
	// Colour space to render in.
	mode ColorMode,
) *Convert {
	out := c.clone()
	out.ColorMode = mode
	return out
}

// WithScaleTo fixes the rendered page's size in pixels rather than its
// resolution, scaling each page to fit inside a square of the given side while
// keeping its aspect ratio. A US Letter page scaled to 500 comes out 386x500.
//
// It overrides WithDpi, and does so in this module rather than by relying on
// poppler's flag precedence: the resolution flag is left off the command line
// entirely when a scale is set, so which one wins is a fact about this code and
// not about an undocumented ordering.
//
// Reach for it when the consumer has a size budget — a thumbnail, a fixed-width
// preview — and for DPI when the consumer cares about the physical scale of what
// is on the page, which is every OCR and measurement case.
//
// Ignored by Text and Txt.
func (c *Convert) WithScaleTo(
	// Side of the pixel box each page is scaled to fit. Must be positive.
	pixels int,
) *Convert {
	out := c.clone()
	out.ScaleTo = pixels
	out.HasScaleTo = true
	return out
}

// WithoutAnnotations renders the page without its annotation layer: no form
// field contents, no stamps, no sticky notes, no highlights.
//
// It is what a caller comparing two revisions of a drawing wants, and what a
// caller feeding pages to OCR usually wants too — an annotation drawn over body
// text is text the recognizer will try to read.
//
// Named for what it removes rather than offered as an `annotations bool`
// defaulting to true, because a `+default=true` bool cannot be set false from
// the Go SDK: the zero value is dropped before it reaches the API, so no caller
// could ever turn annotations off.
//
// Ignored by Text and Txt.
func (c *Convert) WithoutAnnotations() *Convert {
	out := c.clone()
	out.HideAnnotations = true
	return out
}

// Text returns the document's text layer as a string.
//
// This is extraction and not recognition: it returns the text the PDF already
// carries, exactly, in the order the chosen layout mode puts it. A document
// with no text layer — a scan — returns nothing at all, and that empty result
// is the signal to render the pages with Png and hand them to the tesseract
// module instead. Nothing here fails in that case, because from poppler's point
// of view a page with no text is not an error.
//
// Pages are separated by a form feed (U+000C), including after the last one,
// which is pdftotext's own convention. Pass disablePageBreaks to drop them, for
// a consumer that wants one flat stream of text.
func (c *Convert) Text(
	ctx context.Context,
	// How much of the page's physical arrangement survives into the text.
	// Defaults to reading order.
	// +optional
	layout LayoutMode,
	// Emit no form feed between pages.
	// +optional
	disablePageBreaks bool,
) (string, error) {
	flags, err := c.textFlags("Text", layout, disablePageBreaks)
	if err != nil {
		return "", err
	}
	if err := c.Document.validateRange(ctx); err != nil {
		return "", err
	}
	res, err := c.Document.run(ctx, "Text",
		c.Document.command("pdftotext", flags, sourcePath, "-"))
	if err != nil {
		return "", err
	}
	return res.stdout, nil
}

// Txt returns the same text Text returns, as a file rather than a string.
//
// Which of the two to reach for is plumbing and nothing more: a file composes
// with whatever consumes files — another module, an export, a directory being
// assembled — without the bytes passing through the caller.
func (c *Convert) Txt(
	ctx context.Context,
	// How much of the page's physical arrangement survives into the text.
	// Defaults to reading order.
	// +optional
	layout LayoutMode,
	// Emit no form feed between pages.
	// +optional
	disablePageBreaks bool,
) (*dagger.File, error) {
	flags, err := c.textFlags("Txt", layout, disablePageBreaks)
	if err != nil {
		return nil, err
	}
	if err := c.Document.validateRange(ctx); err != nil {
		return nil, err
	}
	res, err := c.Document.run(ctx, "Txt", strings.Join([]string{
		"set -e",
		"mkdir -p " + outputDir,
		c.Document.command("pdftotext", flags, sourcePath, textOutputPath),
	}, "\n"))
	if err != nil {
		return nil, err
	}
	return res.container.File(textOutputPath), nil
}

// Bbox returns the document's text layer as an XHTML report carrying a bounding
// box for every word, in points, measured from the top-left of the page.
//
// It is the text-layer analogue of the tesseract module's Hocr — the same shape
// of answer, sourced from the document's own coordinates rather than from
// recognition, so the boxes are exact rather than estimated. Reach for it when
// the words are not the whole answer: redacting a region, cropping a figure out
// of a page, deciding which of two columns a phrase belongs to, or lining
// extracted text up against a render of the same page.
//
// Every page arrives as a `page` element naming its width and height, holding one
// `word` element per space-separated word with its `xMin`, `yMin`, `xMax` and
// `yMax`. Pass withLayout to wrap those in the `flow`, `block` and `line`
// elements poppler groups them into, each carrying the box around everything
// under it — which is what a caller reconstructing paragraphs wants, and what a
// caller who only needs word boxes pays parsing for.
//
// The report is a whole XHTML document, `-bbox` implying poppler's `-htmlmeta`:
// the boxes sit inside a `doc` element inside an `html` element with a doctype
// ahead of it. Read the `doc` subtree, not the file's root.
//
// A page with no text layer is reported as an empty `page` element rather than as
// a failure — poppler notes `no word list` on stderr and exits 0 — which is the
// same boundary Text draws: no words is the signal to render the page and hand it
// to OCR, not a sign that anything went wrong.
//
// The render options are ignored, this being an extraction and not a render, and
// so is LayoutMode: the report's structure is poppler's own and is not the text's
// reading order. WithPageRange narrows it.
func (c *Convert) Bbox(
	ctx context.Context,
	// Add the block and line boxes poppler groups the words into.
	// +optional
	withLayout bool,
) (*dagger.File, error) {
	flag := bboxFlag
	if withLayout {
		flag = bboxLayoutFlag
	}
	return c.geometry(ctx, "Bbox", flag, bboxOutputPath)
}

// Tsv returns the document's text layer as a tab-separated table with one row
// per layout element, each naming the page it came from and the box it occupies.
//
// It is the text-layer analogue of the tesseract module's Tsv, and the format is
// literally tesseract's: twelve columns — `level`, `page_num`, `par_num`,
// `block_num`, `line_num`, `word_num`, `left`, `top`, `width`, `height`, `conf`,
// `text` — with a header row ahead of them. `level` says what a row describes: 1
// a page, 3 a flow, 4 a line, 5 a word. Poppler emits no level-2 paragraph row
// despite carrying the column for it. Structural rows name themselves in the text
// column — `###PAGE###`, `###FLOW###`, `###LINE###` — and `conf` is -1 on them
// and 100 on every word, this being extraction and not recognition: there is
// nothing to be uncertain about.
//
// Reach for it over Bbox when the consumer is a table reader — a dataframe, a
// spreadsheet, awk — rather than a markup parser, and when the page a row belongs
// to has to be recoverable from the row itself. That is the one thing this format
// carries and Bbox does not: a `-bbox` page element states its size and never its
// number, so a narrowed report is traceable only by position, while `page_num`
// here is the page's number in the whole document even under WithPageRange.
//
// Coordinates are in points, from the top-left of the page, and a word's box is
// the font's ascender-to-descender span around the baseline rather than the
// glyphs' ink — so a one-glyph word is exactly as tall as a long one. Word rows
// round to two decimals where the structural rows print six.
//
// A page with no text layer yields its page row and no word rows rather than a
// failure, the same boundary Text and Bbox draw. The render options are ignored,
// as is LayoutMode. WithPageRange narrows it.
func (c *Convert) Tsv(ctx context.Context) (*dagger.File, error) {
	return c.geometry(ctx, "Tsv", tsvFlag, tsvOutputPath)
}

// Png renders each page in range to a PNG and returns the directory holding
// them, named `page-0001.png`, `page-0002.png`, and so on.
//
// PNG is the format to hand another module: it is lossless, so nothing
// downstream is reading compression artefacts as page content, and it is what
// every image library reads without a codec question.
func (c *Convert) Png(ctx context.Context) (*dagger.Directory, error) {
	return c.raster(ctx, "Png", formatPng)
}

// Jpeg renders each page in range to a JPEG and returns the directory holding
// them, named `page-0001.jpg`, `page-0002.jpg`, and so on — `.jpg` being
// poppler's own choice of extension for the format.
//
// Lossy, so it is the format for a preview a human looks at rather than for
// input another program measures: the artefacts a flat page compresses into sit
// exactly where thin strokes are, which is where OCR reads.
func (c *Convert) Jpeg(ctx context.Context) (*dagger.Directory, error) {
	return c.raster(ctx, "Jpeg", formatJpeg)
}

// Tiff renders each page in range to a TIFF and returns the directory holding
// them, named `page-0001.tif`, `page-0002.tif`, and so on — `.tif` being
// poppler's own choice of extension for the format.
//
// It is the format the document-imaging world already speaks, and the one to
// pair with MONO: a bilevel TIFF is what a scanner emits and what an archive
// expects.
func (c *Convert) Tiff(ctx context.Context) (*dagger.Directory, error) {
	return c.raster(ctx, "Tiff", formatTiff)
}

// Svg renders each page in range to its own SVG and returns the directory
// holding them, named `page-0001.svg`, `page-0002.svg`, and so on.
//
// This is the output for a caller who wants to keep the type outlines rather
// than pixels of them: embedding a page in a web view, feeding a plotter,
// re-flowing it in a vector editor. Text arrives as glyph outlines and not as
// `<text>` — poppler converts every glyph to a `<path>` and references it with
// `<use>` — so the page is faithful and is not searchable. Reach for Text when
// the words are what is wanted.
//
// One SVG per page is this module's doing and matters. `pdftocairo -svg` run
// once over a multi-page document writes a single file holding every page,
// wrapped in the SVG 1.2 `pageSet` and `page` elements — which no browser,
// librsvg or Inkscape implements, so the file draws page one and silently
// discards the rest. Rendering each page on its own invocation is what makes
// every page reachable.
//
// WithDpi governs the rasterized regions a page can still contain — an embedded
// photograph has no vector form to keep. WithColorMode, WithScaleTo and
// WithoutAnnotations are ignored: see the note on Ps.
func (c *Convert) Svg(ctx context.Context) (*dagger.Directory, error) {
	return c.pageRender(ctx, "Svg", formatSvg)
}

// Eps renders each page in range to its own Encapsulated PostScript file and
// returns the directory holding them, named `page-0001.eps`, `page-0002.eps`,
// and so on.
//
// EPS is the format for placing one page inside another document — a figure in
// a LaTeX paper, artwork in a layout program — which is why it is one page per
// file by definition and not by this module's choice: `pdftocairo -eps` handed a
// multi-page document refuses to write anything at all and exits non-zero. The
// per-page loop is what turns that refusal into the whole document.
//
// The bounding box is the page's *ink*, not its media box: poppler crops an EPS
// to what is drawn, so a page of a US Letter document with a single line of text
// on it comes out a few inches wide. That is EPS's own convention — a figure
// carries its extent so the placing document can size it — and it is the one
// place these files do not agree with the page geometry Png reports.
//
// WithDpi governs rasterized regions; WithColorMode, WithScaleTo and
// WithoutAnnotations are ignored: see the note on Ps.
func (c *Convert) Eps(ctx context.Context) (*dagger.Directory, error) {
	return c.pageRender(ctx, "Eps", formatEps)
}

// Ps renders the pages in range to a single PostScript file.
//
// It returns a file rather than a directory because PostScript is a multi-page
// format: one document with a `%%Pages:` count and a `%%Page:` marker per page,
// which is what a printer queue and every downstream PostScript tool expect.
// Splitting it into a page-per-file directory would produce fragments that are
// each individually invalid.
//
// WithDpi governs the rasterized regions the output can still contain.
// WithColorMode, WithScaleTo and WithoutAnnotations are ignored here and by Svg,
// Eps and Html, and are documented no-ops rather than rejections for the same
// reason they are on Text: a conversion configured for the raster outputs should
// fan out into a vector one without having to un-set an option first. The flags
// are dropped rather than forwarded because poppler does not ignore them — it
// exits non-zero with `-mono may only be used with the -png, -jpeg, or -tiff
// output options`, and `-hide-annotations` is not a pdftocairo option at all.
func (c *Convert) Ps(ctx context.Context) (*dagger.File, error) {
	flags, err := c.cairoFlags(formatPs)
	if err != nil {
		return nil, err
	}
	if err := c.Document.validateRange(ctx); err != nil {
		return nil, err
	}
	flags = append(flags, c.Document.rangeArgs()...)

	script := strings.Join([]string{
		"set -e",
		"mkdir -p " + outputDir,
		c.Document.command(formatPs.tool, flags, sourcePath, psOutputPath),
	}, "\n")

	res, err := c.Document.runScript(ctx, "Ps", script)
	if err != nil {
		return nil, err
	}
	return res.container.File(psOutputPath), nil
}

// Html converts each page in range to its own HTML file and returns the
// directory holding them, named `page-0001.html`, `page-0002.html`, and so on,
// with each page's extracted images beside them.
//
// The directory therefore holds more than the pages: a page carrying images gets
// `page-0001-1_1.png` and so on next to `page-0001.html`, referenced from the
// markup by a relative path. Those names are pdftohtml's, not this module's, and
// are left alone precisely because they are written *into* the HTML — renaming
// them to a contract would mean rewriting the markup to match. A consumer
// walking this directory should select `page-*.html`, not assume every entry is
// a page.
//
// The relative reference is the reason the conversion runs with the output
// directory as its working directory: handed an absolute output base, pdftohtml
// writes that absolute path into every `img src`, and the markup then only
// resolves on the machine that produced it.
//
// Every render option is ignored, pdftohtml having no resolution, colour or
// annotation switch of any kind. WithPageRange narrows it like everything else.
func (c *Convert) Html(ctx context.Context) (*dagger.Directory, error) {
	return c.pageRender(ctx, "Html", formatHtml)
}

// clone copies the conversion for a builder method. Every field is a value or a
// handle, so the shallow copy is the deep one.
func (c *Convert) clone() *Convert {
	out := *c
	return &out
}

// textFlags renders everything pdftotext needs that is not the document and not
// a password: the page bounds, the layout mode, and the page-break switch.
func (c *Convert) textFlags(label string, layout LayoutMode, disablePageBreaks bool) ([]string, error) {
	flags, ok := layout.flags()
	if layout != "" && !ok {
		return nil, fmt.Errorf(
			"%s: unknown layout mode %q: must be one of %s",
			label, string(layout), layoutNames())
	}
	args := append([]string(nil), c.Document.rangeArgs()...)
	args = append(args, flags...)
	if disablePageBreaks {
		args = append(args, "-nopgbrk")
	}
	return args, nil
}

// rasterFlags renders everything pdftoppm needs that is not the document, the
// output base or a password.
//
// -forcenum is unconditional. In poppler 25.12 — what the pinned Alpine tag
// ships — it changes nothing, because a page number is always appended. It is
// here for the caller who overrode alpineTag onto an older release, where a
// single-page render was named `page.png` with no number in it at all, breaking
// the naming contract for exactly the documents most likely to be one page.
func (c *Convert) rasterFlags(label string, format rasterFormat) ([]string, error) {
	if c.HasDpi && c.Dpi <= 0 {
		return nil, fmt.Errorf(
			"WithDpi: dpi must be positive, got %d", c.Dpi)
	}
	if c.HasScaleTo && c.ScaleTo <= 0 {
		return nil, fmt.Errorf(
			"WithScaleTo: pixels must be positive, got %d", c.ScaleTo)
	}
	colorFlags, ok := c.ColorMode.flags()
	if c.ColorMode != "" && !ok {
		return nil, fmt.Errorf(
			"%s: unknown colour mode %q: must be one of %s",
			label, string(c.ColorMode), colorNames())
	}

	args := []string{format.flag, "-forcenum"}
	// Exactly one of the two ever reaches the command line, which is what
	// makes WithScaleTo override WithDpi here rather than inside poppler.
	if c.HasScaleTo {
		args = append(args, "-scale-to", strconv.Itoa(c.ScaleTo))
	} else {
		args = append(args, "-r", strconv.Itoa(c.dpi()))
	}
	args = append(args, colorFlags...)
	if c.HideAnnotations {
		args = append(args, "-hide-annotations")
	}
	return append(args, c.Document.rangeArgs()...), nil
}

// dpi is the resolution a render runs at: what the caller asked for, or the
// module's default.
func (c *Convert) dpi() int {
	if c.HasDpi {
		return c.Dpi
	}
	return defaultDpi
}

// cairoFlags renders everything a pdftocairo invocation needs that is not the
// document, the page bounds or the output name.
//
// Only the resolution reaches the command line. The colour modes, the pixel
// bound and the annotation switch are dropped rather than translated, because
// pdftocairo has no equivalent for any of them and rejects the two it recognises
// by name — see the note on Ps for why that is a silent no-op here rather than a
// rejection.
func (c *Convert) cairoFlags(format pageFormat) ([]string, error) {
	if c.HasDpi && c.Dpi <= 0 {
		return nil, fmt.Errorf(
			"WithDpi: dpi must be positive, got %d", c.Dpi)
	}
	args := append([]string(nil), format.flags...)
	if format.takesResolution {
		args = append(args, "-r", strconv.Itoa(c.dpi()))
	}
	return args, nil
}

// pageRender writes one file per page and returns the directory holding them.
//
// The page loop runs in the container rather than here because its upper bound
// is the document's page count, and reading that from Go would mean a second
// round trip to fetch what the very next exec is about to open anyway. Resolving
// it in the same shell keeps the whole conversion one exec, the way the raster
// path's rename pass does.
func (c *Convert) pageRender(ctx context.Context, label string, format pageFormat) (*dagger.Directory, error) {
	flags, err := c.cairoFlags(format)
	if err != nil {
		return nil, err
	}
	if err := c.Document.validateRange(ctx); err != nil {
		return nil, err
	}

	name := pageNamePrefix + `$padded`
	if format.namesExtension {
		name += "." + format.ext
	}
	// The bounds are the loop variable, so the document's own range flags are
	// deliberately not appended: this invocation renders exactly one page.
	render := c.Document.command(format.tool, flags,
		"-f", `"$p"`, "-l", `"$p"`, sourcePath, name)

	res, err := c.Document.runScript(ctx, label, c.pageLoop(render))
	if err != nil {
		return nil, err
	}
	return res.container.Directory(outputDir), nil
}

// pageLoop is the script that runs a per-page conversion once for every page in
// range, with `$padded` set to the page's zero-padded number.
//
// It works the output directory from the inside — `cd` rather than an absolute
// output path — because pdftohtml writes the output name it was given into every
// `img src` in the markup it produces. Named absolutely, it emits markup whose
// images only resolve on the machine that rendered them.
func (c *Convert) pageLoop(render string) string {
	first := 1
	if c.Document.HasRange && c.Document.FirstPage > 0 {
		first = c.Document.FirstPage
	}
	last := endOfDocument
	if c.Document.HasRange {
		last = c.Document.LastPage
	}

	return strings.Join([]string{
		// set -e is what makes a page poppler cannot write stop the loop
		// carrying its own message, rather than leaving a directory that is
		// short a page and says nothing about it.
		`set -e`,
		`mkdir -p ` + outputDir,
		`cd ` + outputDir,
		`first=` + strconv.Itoa(first),
		`last=` + strconv.Itoa(last),
		// An open-ended range is resolved here rather than in Go so the page
		// count and the render share one exec.
		//
		// The report is captured and parsed in two steps rather than piped
		// straight into sed, because set -e reads the exit status of the *last*
		// command in a pipeline. Piped, a document pdfinfo cannot open leaves
		// sed succeeding on nothing, and the run gets as far as the page-count
		// check below and fails there — with poppler's diagnostic still on
		// stderr, but joined by a second message about a page count that was
		// never the problem. Split, the script stops on pdfinfo's own status.
		`if [ "$last" -eq ` + strconv.Itoa(endOfDocument) + ` ]; then`,
		`	info=$(` + c.Document.command("pdfinfo", nil, sourcePath) + `)`,
		`	last=$(printf '%s\n' "$info" | sed -n 's/^Pages:[[:space:]]*//p')`,
		`fi`,
		`case "$last" in`,
		`	''|*[!0-9]*) echo "could not read the page count: $last" >&2; exit 1;;`,
		`esac`,
		// The width is the greater of the module's minimum and what the last
		// page number needs, so a document longer than the minimum can express
		// stays uniform. Same contract the raster path normalizes to.
		`width=` + strconv.Itoa(minPageNumberWidth),
		`if [ ${#last} -gt "$width" ]; then width=${#last}; fi`,
		`p="$first"`,
		`while [ "$p" -le "$last" ]; do`,
		`	padded=$(printf "%0${width}d" "$p")`,
		`	` + render,
		`	p=$((p + 1))`,
		`done`,
	}, "\n")
}

// geometry runs pdftotext in one of its geometry-reporting modes and returns the
// report it wrote.
//
// The three modes share everything but their flag: none of them takes a layout
// mode, none of them takes a page-break switch, and all of them narrow to the
// document's page range. The report is written to a file rather than read off
// stdout because both formats are structured — markup and a table — and a
// consumer parses them rather than reading them.
func (c *Convert) geometry(ctx context.Context, label, flag, output string) (*dagger.File, error) {
	if err := c.Document.validateRange(ctx); err != nil {
		return nil, err
	}
	flags := append([]string{flag}, c.Document.rangeArgs()...)
	res, err := c.Document.runScript(ctx, label, strings.Join([]string{
		"set -e",
		"mkdir -p " + outputDir,
		c.Document.command("pdftotext", flags, sourcePath, output),
	}, "\n"))
	if err != nil {
		return nil, err
	}
	return res.container.File(output), nil
}

// raster renders the pages and returns the directory holding them, with every
// page's number padded to a fixed width.
func (c *Convert) raster(ctx context.Context, label string, format rasterFormat) (*dagger.Directory, error) {
	flags, err := c.rasterFlags(label, format)
	if err != nil {
		return nil, err
	}
	if err := c.Document.validateRange(ctx); err != nil {
		return nil, err
	}
	// set -e makes a document poppler cannot open fail here, carrying its own
	// message, rather than turning into an empty page directory a caller would
	// read as a document with no pages in it.
	script := strings.Join([]string{
		"set -e",
		"mkdir -p " + outputDir,
		c.Document.command("pdftoppm", flags, sourcePath, pageBase),
		normalizeScript,
	}, "\n")

	res, err := c.Document.runScript(ctx, label, script, rasterArgv0, format.ext)
	if err != nil {
		return nil, err
	}
	return res.container.Directory(outputDir), nil
}
