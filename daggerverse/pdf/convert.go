package main

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"dagger/pdf/fanout"
	"dagger/pdf/internal/dagger"
)

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
	// +private
	Concurrency int
	// +private
	HasConcurrency bool
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

// WithConcurrency bounds how many pages a render converts at once, and with it
// how many execs the conversion creates.
//
// It defaults to the number of CPUs the module can see, which is what a
// document wants: poppler renders a page on one core whatever the machine has.
// A 129-page document at 300 dpi spends about a minute of that one core, and the
// recognition it is usually rendered for spends its own time across every core —
// so the render is a third of the CPU and two thirds of the wall clock, and the
// share grows with core count rather than shrinking.
//
// Unlike the tesseract module's Batch, this bound needs no companion. poppler's
// tools are single-threaded, so concurrency here is concurrency and not
// concurrency multiplied by an OpenMP team; the pages contend for cores the way
// any other CPU-bound workload does.
//
// Pass a number to take less of the machine than the default, or 1 to render the
// whole document in one exec, a page at a time. Non-positive is rejected at
// output time.
//
// The two meanings are one setting because they were never independent. The
// pages are split into this many contiguous slices and each slice is one exec
// running its pages' invocations in turn, so the bound is at once how many
// render at a time and how many containers a conversion creates — a
// three-thousand-page document is this many, not three thousand. It is what
// decides how the results cache, too: a slice is the unit that hits or misses.
//
// What it is still not is a change to the answer. The same pages come back under
// every bound, named the same way and rendered from the same bytes; concurrency
// is allowed to change how long a render takes and nothing else.
//
// One caveat, and it is the renderer's rather than this module's: cairo stamps
// the wall clock into every EPS and PS document it writes, as a
// `%%CreationDate` DSC comment, and honours neither SOURCE_DATE_EPOCH nor any
// pdftocairo flag. Two Eps renders at different bounds are different execs at
// different instants, so that one line differs and the directory digests differ
// with it. Everything drawn is identical. Png, Jpeg, Tiff, Svg and Html carry no
// timestamp and are byte-identical at every bound.
//
// Ignored by Text, Txt, Bbox, Tsv and Ps, none of which renders a page at a
// time. See Ps for why that one is a single invocation.
func (c *Convert) WithConcurrency(
	// Maximum number of pages to render at the same time.
	concurrency int,
) *Convert {
	out := c.clone()
	out.Concurrency = concurrency
	out.HasConcurrency = true
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
// That is also why this is the one render that stays a single invocation, and
// why WithConcurrency does nothing here. Every other page-per-file output runs
// one invocation per page and assembles the directory afterwards; a PostScript
// document cannot be assembled that way, because concatenating the fragments is
// not the same operation as writing the document — the result would need its
// prologue, its page markers and its `%%Pages:` count rewritten, which is a
// PostScript editor and not a renderer. A caller who wants the pages rendered
// concurrently wants Eps, which is one page per file by definition.
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
// -singlefile is what makes the page-naming contract this module's rather than
// poppler's. Handed an output base, pdftoppm appends a page number of its own
// choosing and the format's extension; handed -singlefile it writes exactly
// `<base>.<ext>` and nothing else, so a render told to produce `page-0007` does.
// Since each invocation renders one page — several invocations to an exec, but
// still one page each — that is the whole contract, and the rename pass the
// raster family used to run afterwards is gone with it. It is also what lets a
// slice's directory be taken whole: nothing but the pages is in it.
//
// -forcenum is gone for the same reason, and could not survive: it overrides
// -singlefile rather than composing with it, so the two together write
// `page-0007-07.png`. It existed for a caller who overrode alpineTag onto a
// poppler old enough to name a single-page render `page.png`, and -singlefile
// answers that case outright on every release — the number is in the name this
// module chose, not in one the tool appended.
//
// The document's own range flags are deliberately not appended: the page bounds
// are per invocation now, one page wide, and are added by raster.
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

	args := []string{format.flag, "-singlefile"}
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
	return args, nil
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

// pageRender writes one file per page, one exec per slice of pages, and returns
// the directory holding them.
//
// The whole directory of each render is taken rather than the page file alone,
// because pdftohtml writes more than the page: a page carrying images gets them
// beside its markup under names of the tool's choosing, referenced from the
// markup by a relative path. Selecting only what this module can name would
// leave that markup pointing at files the returned directory does not hold.
// Pages cannot collide in the merge — every name a render writes starts with the
// page's own `page-NNNN` base — and they are merged in page order, so the result
// does not depend on which exec finished first.
func (c *Convert) pageRender(ctx context.Context, label string, format pageFormat) (*dagger.Directory, error) {
	flags, err := c.cairoFlags(format)
	if err != nil {
		return nil, err
	}
	pages, err := c.plan(ctx, label)
	if err != nil {
		return nil, err
	}

	jobs := make([]pageJob, 0, len(pages))
	for _, page := range pages {
		// The output is named relative to the output directory, and the script
		// works it from the inside — `cd` rather than an absolute path —
		// because pdftohtml writes the output name it was given into every
		// `img src` in the markup it produces. Named absolutely, it emits
		// markup whose images only resolve on the machine that rendered them.
		name := page.base
		if format.namesExtension {
			name += "." + format.ext
		}
		jobs = append(jobs, pageJob{
			page: page.number,
			command: c.Document.command(format.tool, flags,
				"-f", strconv.Itoa(page.number), "-l", strconv.Itoa(page.number),
				sourcePath, name),
		})
	}

	// The `cd` is the prelude's rather than each page's for the same reason the
	// page commands are unchanged: a slice's script is the per-page commands
	// concatenated, and everything that is per-conversion rather than per-page
	// belongs above them.
	execs, err := c.render(ctx, label,
		[]string{"mkdir -p " + outputDir, "cd " + outputDir}, jobs)
	if err != nil {
		return nil, err
	}
	return assemble(execs), nil
}

// assemble folds the finished execs into the directory a render returns: one
// WithDirectory per exec, in slice order, which is page order.
//
// This is the whole of what #370 changed and the reason it was worth changing.
// Every fold adds an overlayfs lowerdir to the destination snapshot's chain, and
// containerd refuses to mount a chain whose compacted mount data exceeds one
// page — measured at around 440 folds on a fresh engine, and fewer as snapshot
// IDs grow a digit. A directory produced by an exec is *one* snapshot however
// many files that exec wrote, so folding per exec rather than per page makes the
// depth `len(execs)`, which the bound caps and the page count does not enter.
//
// It is also why nothing downstream should fold this directory a page at a time
// to build one of its own: the ceiling is on the chain, not on this module.
func assemble(execs []*dagger.Container) *dagger.Directory {
	out := dag.Directory()
	for _, exec := range execs {
		out = out.WithDirectory("/", exec.Directory(outputDir))
	}
	return out
}

// page is one page of a planned conversion: its number in the source document,
// and the padded file base every output of it is named from.
type page struct {
	number int
	base   string
}

// pageJob is one page's render — the page it covers and the single command that
// renders it — carried together so a failure can name the page rather than the
// slice it was rendered in.
//
// It is a command and not a script now that several of them run in one exec: the
// prelude a family needs is written once at the top of a slice's script, and
// what is per page is exactly the one invocation `plan` sized and named.
type pageJob struct {
	page    int
	command string
}

// plan resolves which pages a conversion covers and what each one's output is
// called, and is the one place the naming contract is decided.
//
// It costs a PageCount round trip that the in-container page loop was written to
// avoid, and the trade is deliberate. A fanned-out render cannot compute the
// padding width from the files it finds, because no exec sees the whole
// document; so the width moves to Go, where it needs the page count. That is one
// pdfinfo per conversion rather than one per page, it is the same exec
// validateRange already pays for whenever a range is set, and Dagger caches it —
// a second conversion of the same document does not run it again.
func (c *Convert) plan(ctx context.Context, label string) ([]page, error) {
	if err := c.validateConcurrency(); err != nil {
		return nil, err
	}
	if err := c.Document.validateRange(ctx); err != nil {
		return nil, err
	}
	count, err := c.Document.pageCount(ctx, label)
	if err != nil {
		return nil, err
	}

	first := 1
	if c.Document.HasRange && c.Document.FirstPage > 0 {
		first = c.Document.FirstPage
	}
	last := count
	if c.Document.HasRange && c.Document.LastPage != endOfDocument {
		last = c.Document.LastPage
	}

	// A conversion covering no pages would otherwise return an empty directory,
	// which a caller reads as a document that rendered fine and had nothing in
	// it — the same silent success EmbeddedImages refuses for a document with no
	// images. It is not reachable through a page range, validateRange having
	// bounded both ends against the count already; it is the floor under a
	// document that reports having no pages at all.
	if last < first {
		return nil, fmt.Errorf(
			"%s: this document reports %d page(s), so there is nothing to render",
			label, count)
	}

	// The width follows the whole document rather than the range, so every
	// conversion of one document numbers its pages the same way however it was
	// narrowed — which is what makes a page traceable to the page it came from.
	width := pageNumberWidth(count)
	pages := make([]page, 0, last-first+1)
	for number := first; number <= last; number++ {
		pages = append(pages, page{
			number: number,
			base:   fmt.Sprintf("%s%0*d", pageNamePrefix, width, number),
		})
	}
	return pages, nil
}

// pageNumberWidth is how many digits a rendered page's number is padded to: the
// greater of the module's minimum and what the document's last page needs.
//
// The minimum is what makes the shape a contract rather than a consequence of
// the document's length, and taking the greater of the two is what keeps a
// document longer than the minimum can express uniform rather than truncating it
// into ambiguity. See normalizeScript, which Split reaches the same contract by.
func pageNumberWidth(count int) int {
	return max(minPageNumberWidth, len(strconv.Itoa(count)))
}

// render partitions the pages into at most workers() slices, runs one exec per
// slice, and returns the finished execs positionally — in slice order, which is
// page order.
//
// One exec per *slice* rather than per page is what keeps a conversion's exec
// count, and so its fold depth, off the length of the document: see assemble for
// what the fold could not survive, and fanout.Partition for the property that
// makes it constant. It costs no parallelism, the pages already having been
// admitted workers() at a time — a three-thousand-page conversion on a 16 CPU
// host was creating three thousand containers in order to run sixteen.
//
// The scheduling and the partitioning are both fanout.RunSlices', one call
// taking the bound once — a module that partitioned by one figure and scheduled
// by another would have two bounds to keep in agreement and no reason for them
// to differ. That package imports no dagger and is tested with `go test -race`;
// see it for why the properties it holds cannot be tested against a document
// instead. What is here is what makes a failure readable: a slice's script names
// the page each command covers, so the error still carries the page and not the
// slice.
func (c *Convert) render(ctx context.Context, label string, prelude []string, jobs []pageJob) ([]*dagger.Container, error) {
	return fanout.RunSlices(ctx, c.workers(), jobs,
		func(ctx context.Context, pages []pageJob) (*dagger.Container, error) {
			// Returned as it is rather than %w-wrapped: the error crosses the
			// module boundary, which unwraps a chain back to the inner error and
			// would drop the page's name along with it.
			res, err := c.Document.runPages(ctx, label,
				pages[0].page, pages[len(pages)-1].page, sliceScript(prelude, pages))
			if err != nil {
				return nil, err
			}
			return res.container, nil
		})
}

// sliceScript is the script one exec runs: the family's prelude once, then the
// per-page commands `plan` generated, in page order, each guarded so its failure
// carries its own page number out of a script that covers several.
//
// The per-page commands are concatenated verbatim, which is what keeps every
// property of the page-per-exec version that was about the *command* rather than
// about the exec — `-singlefile` per page, so the padding width still comes from
// `plan` and `-forcenum` stays out, and one page's `-f`/`-l` bounds.
//
// `set -e` is what makes the prelude fail the exec; the page commands do not
// rely on it, each being an explicitly handled `||`. A failure still stops the
// pages behind it inside its own slice, and fanout.Run's cancellation is what
// stops the other slices.
func sliceScript(prelude []string, jobs []pageJob) string {
	lines := make([]string, 0, len(prelude)+len(jobs)+2)
	lines = append(lines, "set -e",
		pageFailureFn+`() { echo "`+pageFailureMarker+`$1" >&2; exit "$2"; }`)
	lines = append(lines, prelude...)
	for _, job := range jobs {
		lines = append(lines, fmt.Sprintf("%s || %s %d $?", job.command, pageFailureFn, job.page))
	}
	return strings.Join(lines, "\n")
}

// workers is how many pages render at once, and now also how many execs a
// conversion creates: whatever WithConcurrency asked for, or one per CPU the
// module can see.
//
// The default is the core count rather than one because a single pdftoppm
// renders a document's pages one after another on one core however many the
// machine has, so the bound is what turns a document into a parallel render.
// What makes the core count safe here — and needs no companion bound, unlike the
// tesseract module's Batch — is that poppler's tools are single-threaded.
func (c *Convert) workers() int {
	if c.HasConcurrency {
		return c.Concurrency
	}
	return max(minRenderConcurrency, runtime.NumCPU())
}

// validateConcurrency rejects a bound that would render no pages at all. It is
// deferred to output time rather than reported from the builder because a
// builder has no error return, which is the same reason WithDpi's check lives
// away from WithDpi.
func (c *Convert) validateConcurrency() error {
	if c.HasConcurrency && c.Concurrency < minRenderConcurrency {
		return fmt.Errorf(
			"WithConcurrency: concurrency must be positive, got %d: pass 1 to render one page at a time, or leave it unset for one per available CPU",
			c.Concurrency)
	}
	return nil
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
//
// One pdftoppm per page, rather than one over the whole document, is what lets
// the pages render at the same time — poppler renders a range serially in a
// single process, on one core whatever the machine has. The invocations are
// still one per page; what they are not is one per *exec*, several pages'
// invocations sharing a container so the fold that assembles them stays shallow.
// It is the same reversal #298 made in the tesseract module, for the same
// measured reason; see the README.
//
// Each page is named outright rather than renamed afterwards, -singlefile making
// pdftoppm write exactly the file it is given. The directory each exec wrote is
// taken whole, in slice order, so the returned directory is the same whatever
// order the slices finished in — and it holds exactly the pages, -singlefile
// being the reason pdftoppm writes nothing else beside them.
func (c *Convert) raster(ctx context.Context, label string, format rasterFormat) (*dagger.Directory, error) {
	flags, err := c.rasterFlags(label, format)
	if err != nil {
		return nil, err
	}
	pages, err := c.plan(ctx, label)
	if err != nil {
		return nil, err
	}

	jobs := make([]pageJob, 0, len(pages))
	for _, page := range pages {
		bounds := []string{"-f", strconv.Itoa(page.number), "-l", strconv.Itoa(page.number)}
		// A page poppler cannot draw fails the conversion carrying its own
		// message, rather than dropping a file from a directory a caller would
		// read as a document that was short a page all along — see sliceScript
		// for what guards each command.
		jobs = append(jobs, pageJob{
			page: page.number,
			command: c.Document.command("pdftoppm", concat(flags, bounds),
				sourcePath, outputDir+"/"+page.base),
		})
	}

	execs, err := c.render(ctx, label, []string{"mkdir -p " + outputDir}, jobs)
	if err != nil {
		return nil, err
	}
	return assemble(execs), nil
}

// concat joins two flag lists into a new slice, which appending to the first
// would not: the flags are built once per conversion and read once per page, so
// an append that happened to have spare capacity would let one page's bounds
// overwrite the next one's.
func concat(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	out = append(out, a...)
	return append(out, b...)
}
