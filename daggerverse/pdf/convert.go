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

// normalizeScript renames every page pdftoppm just wrote so its number is
// zero-padded to a fixed width, and is the reason this directory can be handed
// to another module at all.
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
