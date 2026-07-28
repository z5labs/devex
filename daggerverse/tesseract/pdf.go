package main

import (
	"fmt"
	"strconv"

	"dagger/tesseract/internal/dagger"
)

// rasterizeArgv0 is the `$0` the rasterize script runs under, so the DPI
// reaches it as `$1` rather than being interpolated into the script text.
const rasterizeArgv0 = "tesseract-rasterize"

// rasterizeScript renders every page of the PDF and writes the list
// recognition reads them back from.
//
// `set -e` makes a PDF poppler cannot open fail here, carrying pdftoppm's own
// message, rather than turning into an empty page directory that recognition
// would report as a document with no text in it.
//
// The list is built by globbing rather than by counting pages, because
// pdftoppm decides the file names: it appends the page number zero-padded to
// the width of the last page, so a 9-page document yields `page-1.png` and a
// 10-page one `page-01.png`. Sorting is safe within a single document for
// exactly that reason — one document, one padding width, so lexicographic
// order is page order.
const rasterizeScript = `set -e
mkdir -p ` + pdfPagesDir + `
pdftoppm -png -r "$1" ` + pdfSourcePath + ` ` + pdfPageBase + `
ls ` + pdfPageBase + `-*.png | sort > ` + pdfPageListPath

// FromPdf binds a PDF to the toolchain by rasterizing its pages first, which
// is the one thing Document cannot do for itself: tesseract reads images
// through leptonica, and leptonica has no PDF support at all.
//
// The result is an ordinary Document, so every output the module offers —
// Text, the per-format renderers, Export, and the recognition options — works
// on a PDF exactly as it does on an image. A multi-page PDF stays one
// document: the renderers accumulate pages, so Text returns the whole document
// with form feeds between pages and Pdf returns one searchable PDF with the
// same page count as the source.
//
// dpi is the resolution the pages are rasterized at, and is declared to
// tesseract as the source resolution too, since it is exactly known here.
// Raising it costs time and memory roughly with its square; lowering it below
// 300 costs recognition accuracy on body text, because tesseract's models were
// trained on characters of a certain pixel height.
func (t *Tesseract) FromPdf(
	// PDF whose pages are rasterized and then recognised.
	source *dagger.File,
	// Resolution to rasterize each page at, in dots per inch.
	// +default=300
	dpi int,
) *Document {
	if dpi == 0 {
		dpi = defaultPdfDpi
	}
	return &Document{
		Tesseract: t,
		Pages:     t.rasterize(source, dpi),
		PdfDpi:    dpi,
		Options:   options{}.withDpi(dpi),
	}
}

// rasterize renders the PDF's pages and returns the directory holding them
// alongside the list naming them in order.
//
// It runs in its own container rather than on the toolchain image so that
// poppler and its font are paid for only by the callers who rasterize
// something. Sharing the pinned Alpine tag with the toolchain keeps the base
// layer shared, and keeps a PDF rasterized by the same distribution release
// the recognition runs on.
//
// The font package is not decoration. Poppler draws a PDF that names a base-14
// font without embedding one — the usual shape for anything not born as a scan
// — by asking fontconfig for a substitute, and with no font installed at all
// there is nothing to substitute: the page renders blank, recognition finds no
// text, and the call succeeds. Liberation is the family to install for it,
// being metric-compatible with Helvetica, Times and Courier, so substituted
// text keeps the line breaks and positions the document was written with.
func (t *Tesseract) rasterize(source *dagger.File, dpi int) *dagger.Directory {
	return dag.Container().
		From(t.image()).
		WithExec([]string{"apk", "add", "--no-cache", popplerPkg, fontPkg}).
		WithMountedFile(pdfSourcePath, source).
		WithExec([]string{"sh", "-c", rasterizeScript, rasterizeArgv0, strconv.Itoa(dpi)}).
		Directory(pdfPagesDir)
}

// validatePdfDpi rejects a non-positive rasterization resolution, which
// pdftoppm would otherwise reject much later with a message about its own
// `-r` flag rather than about the argument the caller actually passed.
func (d *Document) validatePdfDpi() error {
	if d.Pages == nil || d.PdfDpi > 0 {
		return nil
	}
	return fmt.Errorf("FromPdf: dpi must be positive, got %d", d.PdfDpi)
}
