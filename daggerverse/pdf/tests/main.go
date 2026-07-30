// Package main implements the test module for the pdf Dagger module. Each test
// is exposed as a standalone dagger function so it can be invoked individually
// during TDD; All wires them up for parallel execution under `dagger call all`.
//
// The fixtures under fixtures/ are hand-authored and committed. Their content
// streams are uncompressed, so the text a page is supposed to render is readable
// in the fixture itself and an assertion about it can be checked against the
// PDF rather than against another tool's opinion of the PDF.
package main

import (
	"context"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"slices"
	"strings"

	// Registered for their decoders alone: the geometry and pixel assertions
	// decode what poppler wrote, and Go's image package needs the format
	// registered before it will recognise it.
	_ "image/jpeg"
	_ "image/png"

	par "github.com/dagger/dagger/util/parallel"

	"dagger/tests/internal/dagger"
)

const (
	// ledgerPdf is a twelve-page PDF carrying different text on every page.
	// Twelve is not arbitrary: it is the smallest page count that makes
	// pdftoppm pad its page numbers to two digits, which is what the
	// four-digit naming contract has to normalize away.
	//
	// Its pages name the base-14 font Helvetica without embedding it, which is
	// the common shape for a PDF that was never meant to be a scan. That makes
	// it the fixture for the module's font substitution too: with no font
	// installed, poppler draws these pages blank and exits 0.
	ledgerPdf = "ledger.pdf"

	// ledgerPages is how many pages ledgerPdf has.
	ledgerPages = 12

	// columnsPdf is a one-page PDF laying two columns of prose side by side,
	// with its content stream written row-major while the page reads
	// column-major. That disagreement is deliberate: it is what forces the
	// three LayoutMode members apart.
	columnsPdf = "columns.pdf"

	// columnsLeftFirst, columnsLeftLast and columnsRightFirst are marker words
	// occurring exactly once each in columnsPdf, at the top of the left column,
	// the bottom of the left column and the top of the right column. Their
	// relative positions are what tell the three layout modes apart.
	columnsLeftFirst  = "ALPHA"
	columnsLeftLast   = "DELTA"
	columnsRightFirst = "XRAY"

	// scanPdf is a one-page PDF carrying a hex-encoded grayscale image and no
	// text layer at all — the shape a scan arrives in, and the case that has to
	// be handed to the tesseract module.
	scanPdf = "scan.pdf"

	// fontsPdf is a two-page PDF whose pages name different faces: page one an
	// unembedded Helvetica, page two an unembedded Courier alongside a Type 3
	// font.
	//
	// Both halves of that are deliberate. The faces differ per page, which is
	// the only way a page range is observable in a font report at all — every
	// page of ledgerPdf names the same face, so narrowing it changes nothing.
	// And the Type 3 font is embedded by construction, its glyph procedures
	// being in the file itself, which is what gives the `emb` column something
	// to say `yes` about.
	fontsPdf = "fonts.pdf"

	// fontsHelvetica, fontsCourier and fontsEmbedded are the face names
	// pdffonts reports for fontsPdf: the first on page one, the other two on
	// page two.
	fontsHelvetica = "Helvetica"
	fontsCourier   = "Courier"
	fontsEmbedded  = "SquareGlyphs"

	// metadataPdf is a one-page PDF carrying an XMP packet in a /Metadata
	// stream, plus an Info dictionary. The two carry deliberately different
	// titles, which is what lets an assertion tell the packet apart from
	// pdfinfo's ordinary report of the same document.
	metadataPdf = "metadata.pdf"

	// xmpTitleMarker occurs in metadataPdf's XMP packet and infoTitleMarker in
	// its Info dictionary, and neither occurs anywhere else in the fixture.
	xmpTitleMarker  = "XMPTITLE"
	infoTitleMarker = "INFOTITLE"

	// swatchPdf is a one-page PDF of saturated colour patches and one mid grey.
	// It exists because a colour mode is not observable in a PNG's header:
	// poppler's writer emits 8-bit RGB whatever was asked for, so the only place
	// GRAY and MONO show up is in the pixels, and flat saturated colour is what
	// makes them show up unambiguously.
	swatchPdf = "swatch.pdf"

	// annotatedPdf is a one-page PDF whose only colour comes from a Square
	// annotation's appearance stream. Every red pixel in a render of it is the
	// annotation layer, which is what makes WithoutAnnotations measurable.
	annotatedPdf = "annotated.pdf"

	// defaultDpi is the resolution the module renders at when the caller names
	// none, and pointsPerInch converts a MediaBox into pixels at it.
	defaultDpi    = 150
	pointsPerInch = 72

	// fontPkg is the font family the module installs unconditionally, and
	// dejavuPkg one Alpine packages that it does not — which is what makes
	// DejaVu usable as the face WithFonts has to carry in.
	fontPkg   = "ttf-liberation"
	dejavuPkg = "font-dejavu"

	// dejavuDir is where dejavuPkg drops its faces, and dejavuFamily the
	// family name fontconfig reports for them.
	dejavuDir    = "/usr/share/fonts/dejavu"
	dejavuFamily = "DejaVu"

	// ledgerPageWidth and ledgerPageHeight are the fixture's MediaBox in
	// points: US Letter, which is what pdfinfo names as its page size and what
	// the render geometry assertions are derived from.
	ledgerPageWidth  = 612
	ledgerPageHeight = 792
)

// ledgerMarkers is the marker word on each page of ledgerPdf, in page order.
//
// Every page carries a word that occurs nowhere else in the document, which is
// what makes a dropped or reordered page visible: twelve pages of the same text
// would extract identically however they were shuffled.
var ledgerMarkers = []string{
	"ALFA", "BRAVO", "CHARLIE", "DELTA", "ECHO", "FOXTROT",
	"GOLF", "HOTEL", "INDIA", "JULIETT", "KILO", "LIMA",
}

// popplerBinaries is every executable poppler-utils installs. Container's
// promise is that all of them are reachable, not just the nine this module
// wraps: the module wraps pdftotext, pdftoppm, pdfinfo, pdftocairo, pdftohtml,
// pdffonts, pdfsig, pdfseparate and pdfunite, and the escape hatch is the whole
// point of the other four.
var popplerBinaries = []string{
	"pdfattach", "pdfdetach", "pdffonts", "pdfimages", "pdfinfo",
	"pdfseparate", "pdfsig", "pdftocairo", "pdftohtml", "pdftoppm",
	"pdftops", "pdftotext", "pdfunite",
}

type Tests struct{}

// All runs every pdf-module test in parallel.
//
// The fan-out is unbounded by default. Unlike the tesseract suite, which had to
// be capped because an unbounded tesseract sizes its OpenMP teams by CPU count
// and multiplied the concurrency rather than sharing it (#226), poppler's tools
// are single-threaded: twenty of them contend for cores the way any other
// oversubscribed workload does. The cap stays available for a host that wants a
// narrower slice.
//
// +check
func (t *Tests) All(
	ctx context.Context,
	// Maximum number of tests to run concurrently. Zero fans out unbounded.
	// +default=0
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}

	jobs = jobs.WithJob("VersionReportsPopplerRelease", t.VersionReportsPopplerRelease)
	jobs = jobs.WithJob("ContainerCarriesEveryPopplerBinaryAndTheFont", t.ContainerCarriesEveryPopplerBinaryAndTheFont)
	jobs = jobs.WithJob("WithFontsPutsFaceWhereFontconfigFindsIt", t.WithFontsPutsFaceWhereFontconfigFindsIt)

	jobs = jobs.WithJob("InfoReportsPageCountAndSize", t.InfoReportsPageCountAndSize)
	jobs = jobs.WithJob("FontsReportsWhetherEachFaceIsEmbedded", t.FontsReportsWhetherEachFaceIsEmbedded)
	jobs = jobs.WithJob("FontsNarrowsToThePageRange", t.FontsNarrowsToThePageRange)
	jobs = jobs.WithJob("MetadataReturnsTheXmpPacketOrSaysThereIsNone", t.MetadataReturnsTheXmpPacketOrSaysThereIsNone)
	jobs = jobs.WithJob("SignaturesReportsAnUnsignedDocumentInsteadOfFailing", t.SignaturesReportsAnUnsignedDocumentInsteadOfFailing)
	jobs = jobs.WithJob("ReportsOpenAnEncryptedDocumentWithThePassword", t.ReportsOpenAnEncryptedDocumentWithThePassword)

	jobs = jobs.WithJob("TextReproducesTextLayerExactly", t.TextReproducesTextLayerExactly)
	jobs = jobs.WithJob("TextOnImageOnlyPdfReturnsNothing", t.TextOnImageOnlyPdfReturnsNothing)
	jobs = jobs.WithJob("TxtMatchesText", t.TxtMatchesText)
	jobs = jobs.WithJob("DisablePageBreaksControlsFormFeeds", t.DisablePageBreaksControlsFormFeeds)
	jobs = jobs.WithJob("LayoutModesProduceDifferentOrderings", t.LayoutModesProduceDifferentOrderings)

	jobs = jobs.WithJob("PageRangeNarrowsText", t.PageRangeNarrowsText)
	jobs = jobs.WithJob("PageRangeOpenEndedRunsToTheLastPage", t.PageRangeOpenEndedRunsToTheLastPage)
	jobs = jobs.WithJob("PageRangeRejectsInvalidBounds", t.PageRangeRejectsInvalidBounds)

	jobs = jobs.WithJob("PngNamesEveryPageWithFourDigits", t.PngNamesEveryPageWithFourDigits)
	jobs = jobs.WithJob("PngNarrowedRangeKeepsFourDigitNames", t.PngNarrowedRangeKeepsFourDigitNames)
	jobs = jobs.WithJob("PngSinglePageIsStillNumbered", t.PngSinglePageIsStillNumbered)
	jobs = jobs.WithJob("JpegAndTiffFollowTheSameContract", t.JpegAndTiffFollowTheSameContract)

	jobs = jobs.WithJob("DpiDefaultsToOneFiftyAndScalesWithTheSetting", t.DpiDefaultsToOneFiftyAndScalesWithTheSetting)
	jobs = jobs.WithJob("ScaleToOverridesDpi", t.ScaleToOverridesDpi)
	jobs = jobs.WithJob("RenderSettingsRejectNonPositiveValues", t.RenderSettingsRejectNonPositiveValues)
	jobs = jobs.WithJob("ColorModesProduceDifferentPixels", t.ColorModesProduceDifferentPixels)
	jobs = jobs.WithJob("WithoutAnnotationsRemovesTheAnnotationLayer", t.WithoutAnnotationsRemovesTheAnnotationLayer)

	jobs = jobs.WithJob("SvgWritesOneVectorFilePerPage", t.SvgWritesOneVectorFilePerPage)
	jobs = jobs.WithJob("EpsWritesEveryPageOfTheDocument", t.EpsWritesEveryPageOfTheDocument)
	jobs = jobs.WithJob("PsHoldsEveryPageInOneFile", t.PsHoldsEveryPageInOneFile)
	jobs = jobs.WithJob("HtmlCarriesPageMarkupAndItsImages", t.HtmlCarriesPageMarkupAndItsImages)
	jobs = jobs.WithJob("PageRangeNarrowsEveryPerPageFormat", t.PageRangeNarrowsEveryPerPageFormat)
	jobs = jobs.WithJob("VectorFormatsIgnoreRasterOnlySettings", t.VectorFormatsIgnoreRasterOnlySettings)

	jobs = jobs.WithJob("SplitWritesOnePdfPerPage", t.SplitWritesOnePdfPerPage)
	jobs = jobs.WithJob("SplitNarrowsToThePageRange", t.SplitNarrowsToThePageRange)
	jobs = jobs.WithJob("MergePreservesTheOrderOfItsSources", t.MergePreservesTheOrderOfItsSources)
	jobs = jobs.WithJob("MergeRejectsWhatItCannotMerge", t.MergeRejectsWhatItCannotMerge)
	jobs = jobs.WithJob("SplitThenMergeRoundTripsTheDocument", t.SplitThenMergeRoundTripsTheDocument)
	jobs = jobs.WithJob("SplitAndMergeCannotOpenAnEncryptedDocument", t.SplitAndMergeCannotOpenAnEncryptedDocument)

	jobs = jobs.WithJob("EncryptedDocumentNeedsThePassword", t.EncryptedDocumentNeedsThePassword)

	return jobs.Run(ctx)
}

// SvgWritesOneVectorFilePerPage asserts Svg renders a multi-page document to one
// SVG per page, named to the page contract, carrying vector geometry.
//
// The per-page invocation is the whole point. `pdftocairo -svg` run once over a
// twelve-page document writes a single file — no page is dropped, but they are
// wrapped in the SVG 1.2 `<pageSet>`/`<page>` elements that essentially no
// renderer implements, so a browser, Inkscape or librsvg shows page one and
// silently discards the other eleven. The `<pageSet>` assertion is what would
// catch a return to the single invocation: it passes an entry-count check and
// fails here.
//
// Vectorness is asserted as drawing operators present and no raster image
// anywhere. Poppler converts text to glyph outlines rather than to `<text>`, so
// the marker words are not in the file at all — a Contains check for one would
// fail on a perfectly good SVG.
func (t *Tests) SvgWritesOneVectorFilePerPage(ctx context.Context) error {
	dir := pdf().Document(fixture(ledgerPdf)).Convert().Svg()

	entries, err := dir.Entries(ctx)
	if err != nil {
		return fmt.Errorf("Svg: %w", err)
	}
	if err := assertPageNames(entries, pageNames("svg", 1, ledgerPages)); err != nil {
		return err
	}

	for _, page := range []int{1, ledgerPages} {
		name := fmt.Sprintf("page-%04d.svg", page)
		got, err := dir.File(name).Contents(ctx)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if !strings.Contains(got, "<path") {
			return fmt.Errorf("expected %s to carry vector path data, got:\n%s", name, head(got))
		}
		if strings.Contains(got, "<image") {
			return fmt.Errorf("expected %s to carry no embedded raster image, got:\n%s", name, head(got))
		}
		if strings.Contains(got, "<pageSet") {
			return fmt.Errorf("expected %s to be a single-page SVG, got a pageSet:\n%s", name, head(got))
		}
	}
	return nil
}

// EpsWritesEveryPageOfTheDocument asserts Eps turns a twelve-page
// document into twelve EPS files rather than into a failure.
//
// This is the criterion that a multi-page source never silently loses pages, and
// for EPS the failure it guards is not silent at all: `pdftocairo -eps` handed a
// multi-page document writes nothing and exits 99 with `EPS files can only
// contain one page.` A single invocation would make Eps unusable for every
// document with a second page in it, so the page count here is the assertion
// that the per-page loop is what runs.
//
// Each file is then checked to be an EPS in its own right — the EPSF version
// header, and exactly one page in it — because a loop that wrote twelve copies
// of page one would satisfy the names alone.
func (t *Tests) EpsWritesEveryPageOfTheDocument(ctx context.Context) error {
	dir := pdf().Document(fixture(ledgerPdf)).Convert().Eps()

	entries, err := dir.Entries(ctx)
	if err != nil {
		return fmt.Errorf("Eps: %w", err)
	}
	if err := assertPageNames(entries, pageNames("eps", 1, ledgerPages)); err != nil {
		return err
	}

	for _, page := range []int{1, ledgerPages} {
		name := fmt.Sprintf("page-%04d.eps", page)
		got, err := dir.File(name).Contents(ctx)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if !strings.HasPrefix(got, "%!PS-Adobe-3.0 EPSF-3.0") {
			return fmt.Errorf("expected %s to open with an EPSF header, got:\n%s", name, head(got))
		}
		if n := strings.Count(got, "\n%%Page: "); n != 1 {
			return fmt.Errorf("expected %s to hold exactly one page, got %d", name, n)
		}
	}

	// The pages differ from one another, which is what says the loop advanced
	// rather than rendering the same page twelve times under twelve names.
	first, err := dir.File("page-0001.eps").Contents(ctx)
	if err != nil {
		return fmt.Errorf("read page-0001.eps: %w", err)
	}
	last, err := dir.File(fmt.Sprintf("page-%04d.eps", ledgerPages)).Contents(ctx)
	if err != nil {
		return fmt.Errorf("read the last page: %w", err)
	}
	if first == last {
		return fmt.Errorf("expected the first and last page to differ, got identical EPS")
	}
	return nil
}

// PsHoldsEveryPageInOneFile asserts Ps returns one PostScript document carrying
// every page, and that the page range narrows it.
//
// The return type is the claim under test. PostScript is a multi-page format
// with a document-level `%%Pages:` count and a `%%Page:` marker per page, so a
// directory of one-page fragments would be the wrong answer even though it would
// look tidier beside Svg and Eps. Counting the markers is what says the pages
// reached the file rather than only the header saying they did.
func (t *Tests) PsHoldsEveryPageInOneFile(ctx context.Context) error {
	doc := pdf().Document(fixture(ledgerPdf))

	whole, err := doc.Convert().Ps().Contents(ctx)
	if err != nil {
		return fmt.Errorf("Ps: %w", err)
	}
	if !strings.HasPrefix(whole, "%!PS-Adobe-") {
		return fmt.Errorf("expected a PostScript header, got:\n%s", head(whole))
	}
	if want := fmt.Sprintf("%%%%Pages: %d", ledgerPages); !strings.Contains(whole, want) {
		return fmt.Errorf("expected %q in the document header, got:\n%s", want, head(whole))
	}
	if got := strings.Count(whole, "\n%%Page: "); got != ledgerPages {
		return fmt.Errorf("expected %d page markers, got %d", ledgerPages, got)
	}

	const first, last = 4, 6
	narrowed, err := doc.WithPageRange(first, last).Convert().Ps().Contents(ctx)
	if err != nil {
		return fmt.Errorf("Ps for pages %d-%d: %w", first, last, err)
	}
	if got := strings.Count(narrowed, "\n%%Page: "); got != last-first+1 {
		return fmt.Errorf("expected %d page markers for pages %d-%d, got %d",
			last-first+1, first, last, got)
	}
	return nil
}

// HtmlCarriesPageMarkupAndItsImages asserts both halves of what Html promises:
// the page's text as markup, and the images the page carries extracted beside it
// and referenced by a path that resolves.
//
// The relative reference is the half that is easy to get wrong and impossible to
// notice. pdftohtml writes the output name it was given straight into every `img
// src`, so an absolute output base — the obvious way to write into a staging
// directory — produces markup whose images resolve only on the machine that
// rendered them, and which still passes every assertion about entry names. The
// conversion runs with the output directory as its working directory for exactly
// this reason, and the `src` check here is what holds it there.
//
// Two fixtures are needed because no one page is both: ledger.pdf is text and no
// images, scan.pdf is one image and no text.
func (t *Tests) HtmlCarriesPageMarkupAndItsImages(ctx context.Context) error {
	const page = 3

	text := pdf().Document(fixture(ledgerPdf)).Convert().HTML()
	entries, err := text.Entries(ctx)
	if err != nil {
		return fmt.Errorf("Html: %w", err)
	}
	// A document with no images produces the pages and nothing else, so this is
	// the fixture the page-naming contract is asserted on.
	if err := assertPageNames(entries, pageNames("html", 1, ledgerPages)); err != nil {
		return err
	}

	markup, err := text.File(fmt.Sprintf("page-%04d.html", page)).Contents(ctx)
	if err != nil {
		return fmt.Errorf("read page %d: %w", page, err)
	}
	for _, want := range []string{
		"<html", fmt.Sprintf("Page %d of the ledger.", page), ledgerMarkers[page-1],
	} {
		if !strings.Contains(markup, want) {
			return fmt.Errorf("expected page %d's markup to contain %q, got:\n%s", page, want, head(markup))
		}
	}
	// The pages are separate documents, so page 3 carries page 3 and nothing
	// from its neighbours.
	if other := ledgerMarkers[page]; strings.Contains(markup, other) {
		return fmt.Errorf("expected page %d's markup to carry only its own page, found %q", page, other)
	}

	scan := pdf().Document(fixture(scanPdf)).Convert().HTML()
	scanEntries, err := scan.Entries(ctx)
	if err != nil {
		return fmt.Errorf("Html of a scan: %w", err)
	}
	if !slices.Contains(scanEntries, "page-0001.html") {
		return fmt.Errorf("expected page-0001.html, got %v", scanEntries)
	}
	images := imageEntries(scanEntries)
	if len(images) == 0 {
		return fmt.Errorf("expected the page's image to be extracted beside it, got %v", scanEntries)
	}

	scanMarkup, err := scan.File("page-0001.html").Contents(ctx)
	if err != nil {
		return fmt.Errorf("read the scan's markup: %w", err)
	}
	for _, img := range images {
		want := `src="` + img + `"`
		if !strings.Contains(scanMarkup, want) {
			return fmt.Errorf("expected the markup to reference %s relatively as %s, got:\n%s",
				img, want, head(scanMarkup))
		}
	}
	// An absolute reference is the failure this test exists for: it resolves on
	// the machine that rendered the page and nowhere else.
	if strings.Contains(scanMarkup, `src="/`) {
		return fmt.Errorf("expected no absolute image path in the markup, got:\n%s", head(scanMarkup))
	}
	return nil
}

// PageRangeNarrowsEveryPerPageFormat asserts the page bounds reach Svg, Eps and
// Html, in all three of the shapes a range comes in.
//
// The per-page loop resolves the bounds itself rather than handing poppler `-f`
// and `-l`, so none of this follows from the raster path already honouring them.
// The open-ended case is the one that exercises the resolution: a zero last is
// the document's page count read inside the same exec that renders, and a loop
// that mishandled it would render nothing at all and return an empty directory
// rather than fail.
//
// The numbers are the source document's page numbers and not positions within
// the range, which is why pages 4 through 6 come out `page-0004` through
// `page-0006` — the same promise the raster contract makes, so a page stays
// traceable to the page it came from whichever format it was rendered to.
func (t *Tests) PageRangeNarrowsEveryPerPageFormat(ctx context.Context) error {
	doc := pdf().Document(fixture(ledgerPdf))

	for _, rng := range []struct {
		name        string
		first, last int
		wantFirst   int
		wantLast    int
	}{
		{"a span", 4, 6, 4, 6},
		{"one page", 2, 2, 2, 2},
		{"open ended", 10, 0, 10, ledgerPages},
	} {
		conv := doc.WithPageRange(rng.first, rng.last).Convert()

		for _, format := range []struct {
			name string
			ext  string
			dir  *dagger.Directory
		}{
			{"Svg", "svg", conv.Svg()},
			{"Eps", "eps", conv.Eps()},
			{"Html", "html", conv.HTML()},
		} {
			got, err := format.dir.Entries(ctx)
			if err != nil {
				return fmt.Errorf("%s: %s: %w", rng.name, format.name, err)
			}
			want := pageNames(format.ext, rng.wantFirst, rng.wantLast)
			if err := assertPageNames(got, want); err != nil {
				return fmt.Errorf("%s: %s: %s", rng.name, format.name, err.Error())
			}
		}
	}
	return nil
}

// VectorFormatsIgnoreRasterOnlySettings asserts a conversion configured for the
// raster outputs still converts when it is asked for a vector one.
//
// Forwarding those flags is not a no-op, which is what makes this worth an
// assertion of its own: pdftocairo rejects both of the ones it recognises —
// `-mono may only be used with the -png, -jpeg, or -tiff output options` — and
// exits 99 having written nothing, and `-hide-annotations` is not a pdftocairo
// option at all. So the natural shape of "render this document as PNG for OCR
// and as SVG for the web view" would fail on its second half unless the module
// drops what it cannot honour.
//
// WithDpi is the one setting that does reach pdftocairo, and it is set here too
// so this is not accidentally asserting that no flags are passed at all.
func (t *Tests) VectorFormatsIgnoreRasterOnlySettings(ctx context.Context) error {
	conv := pdf().Document(fixture(annotatedPdf)).
		Convert().
		WithDpi(300).
		WithColorMode(dagger.PdfColorModeMono).
		WithScaleTo(500).
		WithoutAnnotations()

	for _, format := range []struct {
		name string
		ext  string
		dir  *dagger.Directory
	}{
		{"Svg", "svg", conv.Svg()},
		{"Eps", "eps", conv.Eps()},
		{"Html", "html", conv.HTML()},
	} {
		got, err := format.dir.Entries(ctx)
		if err != nil {
			return fmt.Errorf("%s with raster-only settings: %w", format.name, err)
		}
		if !slices.Contains(got, "page-0001."+format.ext) {
			return fmt.Errorf("%s: expected page-0001.%s, got %v", format.name, format.ext, got)
		}
	}

	if _, err := conv.Ps().Contents(ctx); err != nil {
		return fmt.Errorf("Ps with raster-only settings: %w", err)
	}

	// A resolution that cannot mean anything is still refused, because that one
	// does reach the tool. Left to poppler it arrives as a complaint about `-r`,
	// which names a flag the caller never wrote.
	if _, err := conv.WithDpi(0).Svg().Entries(ctx); err == nil {
		return fmt.Errorf("expected Svg to reject a zero dpi")
	} else {
		for _, want := range []string{"WithDpi", "dpi", "positive"} {
			if !strings.Contains(err.Error(), want) {
				return fmt.Errorf("expected the rejection to mention %q, got: %v", want, err)
			}
		}
	}
	return nil
}

// imageEntries picks the extracted images out of an Html directory's listing.
//
// They are everything that is not a page, and they keep pdftohtml's own names
// rather than the page contract's: the names are written into the markup that
// references them, so renaming them would mean rewriting the markup.
func imageEntries(entries []string) []string {
	var images []string
	for _, e := range entries {
		if !strings.HasSuffix(e, ".html") {
			images = append(images, e)
		}
	}
	return images
}

// head is the first part of a file, for an error message that has to quote what
// it read without pasting a whole rendered page into the log.
func head(s string) string {
	const limit = 400
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// VersionReportsPopplerRelease asserts Version reports the poppler release the
// pinned Alpine tag ships, as a bare version number.
//
// poppler's tools print their banner on stderr and still exit 0, so a module
// reading stdout would return the empty string here for every image it ever
// built. The assertion is on the shape and the major release rather than the
// exact patch: Alpine may rebuild poppler-utils within the v3.24 branch, and a
// test that pins the patch level would fail on a change this module has no
// opinion about.
func (t *Tests) VersionReportsPopplerRelease(ctx context.Context) error {
	got, err := pdf().Version(ctx)
	if err != nil {
		return fmt.Errorf("Version: %w", err)
	}
	if strings.ContainsAny(got, " \t\n") {
		return fmt.Errorf("expected a bare version number, got %q", got)
	}
	fields := strings.Split(got, ".")
	if len(fields) != 3 {
		return fmt.Errorf("expected a three-part version number, got %q", got)
	}
	if fields[0] != "25" {
		return fmt.Errorf("expected poppler 25.x on alpine 3.24, got %q", got)
	}
	return nil
}

// ContainerCarriesEveryPopplerBinaryAndTheFont asserts the assembled image is
// the escape hatch it claims to be: all thirteen poppler binaries on PATH, and
// the substitute font family installed.
//
// The font half is not a packaging detail. Without a font installed poppler has
// nothing to substitute for a PDF that names a base-14 face without embedding
// it, and renders the page blank while exiting 0 — a silent wrong answer, which
// is the failure mode this assertion exists to keep out.
func (t *Tests) ContainerCarriesEveryPopplerBinaryAndTheFont(ctx context.Context) error {
	ctr := pdf().Container()

	out, err := ctr.WithExec([]string{"sh", "-c",
		"set -e; for b in " + strings.Join(popplerBinaries, " ") + `; do command -v "$b"; done`,
	}).Stdout(ctx)
	if err != nil {
		return fmt.Errorf("resolving poppler binaries: %w", err)
	}
	for _, bin := range popplerBinaries {
		if !strings.Contains(out, "/"+bin+"\n") {
			return fmt.Errorf("expected %s on PATH, got:\n%s", bin, out)
		}
	}

	installed, err := ctr.WithExec([]string{"apk", "info", "-e", fontPkg}).Stdout(ctx)
	if err != nil {
		return fmt.Errorf("querying %s: %w", fontPkg, err)
	}
	if !strings.Contains(installed, fontPkg) {
		return fmt.Errorf("expected %s installed, got %q", fontPkg, installed)
	}
	return nil
}

// WithFontsPutsFaceWhereFontconfigFindsIt asserts a supplied face is one
// fontconfig reports, which is the only thing that matters: poppler asks
// fontconfig for a substitute and draws nothing at all when the answer is
// nothing.
//
// DejaVu is the face because Alpine packages it separately from the family this
// module installs, so the negative control is real — the plain image genuinely
// cannot see it, and the assertion is not passing on something the base image
// already had.
func (t *Tests) WithFontsPutsFaceWhereFontconfigFindsIt(ctx context.Context) error {
	faces := pdf().Container().
		WithExec([]string{"apk", "add", "--no-cache", dejavuPkg}).
		Directory(dejavuDir)

	before, err := pdf().Container().WithExec([]string{"fc-list"}).Stdout(ctx)
	if err != nil {
		return fmt.Errorf("fc-list without fonts: %w", err)
	}
	if strings.Contains(before, dejavuFamily) {
		return fmt.Errorf("expected the plain image not to carry %s, got:\n%s", dejavuFamily, before)
	}

	after, err := pdf().WithFonts(faces).Container().WithExec([]string{"fc-list"}).Stdout(ctx)
	if err != nil {
		return fmt.Errorf("fc-list with fonts: %w", err)
	}
	if !strings.Contains(after, dejavuFamily) {
		return fmt.Errorf("expected fontconfig to report %s, got:\n%s", dejavuFamily, after)
	}
	return nil
}

// InfoReportsPageCountAndSize asserts Info surfaces what pdfinfo knows, and
// PageCount reads the page count back out of it.
//
// They are asserted together because the second is defined in terms of the
// first: PageCount parses Info's `Pages:` line, so a change to how Info is
// captured that broke the parse would otherwise show up only in whatever used
// PageCount next.
func (t *Tests) InfoReportsPageCountAndSize(ctx context.Context) error {
	doc := pdf().Document(fixture(ledgerPdf))

	info, err := doc.Info(ctx)
	if err != nil {
		return fmt.Errorf("Info: %w", err)
	}
	wantSize := fmt.Sprintf("%d x %d pts", ledgerPageWidth, ledgerPageHeight)
	for _, want := range []string{
		fmt.Sprintf("Pages:%s%d", strings.Repeat(" ", 11), ledgerPages),
		wantSize,
	} {
		if !strings.Contains(info, want) {
			return fmt.Errorf("expected Info to contain %q, got:\n%s", want, info)
		}
	}

	count, err := doc.PageCount(ctx)
	if err != nil {
		return fmt.Errorf("PageCount: %w", err)
	}
	if count != ledgerPages {
		return fmt.Errorf("expected PageCount %d, got %d", ledgerPages, count)
	}
	return nil
}

// FontsReportsWhetherEachFaceIsEmbedded asserts Fonts surfaces pdffonts' table
// and that the `emb` column in it says what it is supposed to say.
//
// ledgerPdf is the shape the module's font install exists for: its pages name
// Helvetica without embedding it, so poppler has to ask fontconfig for a
// substitute, and with no font installed there is nothing to substitute — the
// page renders blank and the command exits 0. `Helvetica … no` is what that
// silent failure looks like before it happens, which is the whole reason a
// pipeline asks for this report.
//
// fontsPdf carries the other half of the column. Its Type 3 font is embedded by
// construction, so the report has to read `yes` for it; without that contrast
// the assertion would pass just as well on a report that said `no` about
// everything, including one produced by a module that had hardcoded the answer.
func (t *Tests) FontsReportsWhetherEachFaceIsEmbedded(ctx context.Context) error {
	report, err := pdf().Document(fixture(ledgerPdf)).Fonts(ctx)
	if err != nil {
		return fmt.Errorf("Fonts: %w", err)
	}
	// The header is part of the report and is what makes it readable as a table
	// rather than as a list of names.
	for _, want := range []string{"name", "type", "encoding", "emb"} {
		if !strings.Contains(report, want) {
			return fmt.Errorf("expected the report's header to name the %q column, got:\n%s", want, report)
		}
	}
	row, err := fontRow(report, fontsHelvetica)
	if err != nil {
		return fmt.Errorf("%s: %s", ledgerPdf, err.Error())
	}
	if got := fontEmbedded(row); got != "no" {
		return fmt.Errorf("expected %s to be reported unembedded, got %q in:\n%s",
			fontsHelvetica, got, report)
	}

	report, err = pdf().Document(fixture(fontsPdf)).Fonts(ctx)
	if err != nil {
		return fmt.Errorf("Fonts of %s: %w", fontsPdf, err)
	}
	for _, tc := range []struct {
		face string
		emb  string
	}{
		{fontsHelvetica, "no"},
		{fontsCourier, "no"},
		{fontsEmbedded, "yes"},
	} {
		row, err := fontRow(report, tc.face)
		if err != nil {
			return fmt.Errorf("%s: %s", fontsPdf, err.Error())
		}
		if got := fontEmbedded(row); got != tc.emb {
			return fmt.Errorf("expected %s's emb column to read %q, got %q in:\n%s",
				tc.face, tc.emb, got, report)
		}
	}
	return nil
}

// FontsNarrowsToThePageRange asserts WithPageRange reaches pdffonts, and that a
// range it cannot honour is refused the way every other one is.
//
// Which faces a document needs is a question about pages, so this is not a
// cosmetic option: a report of pages 1 through 3 that listed a face used only on
// page 40 would say the render depends on a font it does not, and one that
// dropped a face used on page 2 would say the opposite. fontsPdf is built for
// it, its two pages naming different faces, because narrowing a report of
// ledgerPdf — every page of which names the same Helvetica — changes nothing at
// all and would pass against a module that ignored the bounds entirely.
func (t *Tests) FontsNarrowsToThePageRange(ctx context.Context) error {
	doc := pdf().Document(fixture(fontsPdf))

	for _, tc := range []struct {
		first, last int
		want        []string
		absent      []string
	}{
		{1, 1, []string{fontsHelvetica}, []string{fontsCourier, fontsEmbedded}},
		{2, 2, []string{fontsCourier, fontsEmbedded}, []string{fontsHelvetica}},
		// An open-ended range is the whole document from page one, so both
		// pages' faces are back.
		{1, 0, []string{fontsHelvetica, fontsCourier, fontsEmbedded}, nil},
	} {
		report, err := doc.WithPageRange(tc.first, tc.last).Fonts(ctx)
		if err != nil {
			return fmt.Errorf("Fonts for pages %d-%d: %w", tc.first, tc.last, err)
		}
		for _, face := range tc.want {
			if _, err := fontRow(report, face); err != nil {
				return fmt.Errorf("pages %d-%d: %s", tc.first, tc.last, err.Error())
			}
		}
		for _, face := range tc.absent {
			if strings.Contains(report, face) {
				return fmt.Errorf("pages %d-%d: expected no %s in the report, got:\n%s",
					tc.first, tc.last, face, report)
			}
		}
	}

	// The bounds are checked against the document rather than handed to poppler,
	// which for pdffonts is the difference between a named refusal and a report
	// of no fonts at all — it prints the header and exits 0 for a range past the
	// end of the document.
	_, err := doc.WithPageRange(9, 0).Fonts(ctx)
	if err == nil {
		return fmt.Errorf("expected a range past the end of the document to be rejected")
	}
	for _, want := range []string{"first (9)", "2 pages"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the rejection to mention %q, got: %v", want, err)
		}
	}
	return nil
}

// MetadataReturnsTheXmpPacketOrSaysThereIsNone asserts both halves of what
// Metadata answers, because the interesting one is the absence.
//
// The packet is asserted to be the packet and not pdfinfo's ordinary report of
// the same document: `pdfinfo -meta` prints the XMP alone, so a module that
// dropped the flag would return a report that still mentions a title and still
// looks like metadata. metadataPdf's Info dictionary carries a deliberately
// different title for exactly that reason — the wrong one showing up is what
// makes the substitution visible.
//
// The absence is the half that needs a decision. poppler prints nothing at all
// and exits 0 for a document with no XMP, and an empty string is
// indistinguishable from a function that did not run, so the module answers with
// a line naming the absence and pointing at the report that does carry the
// document's metadata.
func (t *Tests) MetadataReturnsTheXmpPacketOrSaysThereIsNone(ctx context.Context) error {
	packet, err := pdf().Document(fixture(metadataPdf)).Metadata(ctx)
	if err != nil {
		return fmt.Errorf("Metadata: %w", err)
	}
	for _, want := range []string{"<?xpacket", "x:xmpmeta", "dc:title", xmpTitleMarker} {
		if !strings.Contains(packet, want) {
			return fmt.Errorf("expected the packet to contain %q, got:\n%s", want, packet)
		}
	}
	// The Info dictionary's title is what pdfinfo prints without -meta, so its
	// presence would mean the ordinary report came back under this name.
	if strings.Contains(packet, infoTitleMarker) {
		return fmt.Errorf("expected the XMP packet rather than pdfinfo's ordinary report, got:\n%s", packet)
	}

	absent, err := pdf().Document(fixture(ledgerPdf)).Metadata(ctx)
	if err != nil {
		return fmt.Errorf("Metadata of a document without XMP: %w", err)
	}
	// A document with no XMP is a perfectly good document, so this is a result
	// and not a failure — and it has to say something, an empty string being what
	// poppler returns and what a caller cannot act on.
	for _, want := range []string{"No XMP metadata", "Info"} {
		if !strings.Contains(absent, want) {
			return fmt.Errorf("expected the absence to be reported with %q, got:\n%s", want, absent)
		}
	}
	// Whatever it says, it must not read as a packet: a caller looking for XMP
	// has to be able to tell there is none.
	if strings.Contains(absent, "<?xpacket") {
		return fmt.Errorf("expected no XMP packet, got:\n%s", absent)
	}
	return nil
}

// SignaturesReportsAnUnsignedDocumentInsteadOfFailing asserts the case almost
// every document is: no signatures, reported as a result.
//
// It is the assertion the function's exit-code handling exists for. pdfsig exits
// 2 for a document carrying no signatures, having printed exactly what it found,
// and the module's usual treatment of a non-zero exit — an error naming the
// failure — would turn the ordinary answer to an ordinary question into a broken
// pipeline. Reserving failure for the runs that failed is what makes this
// callable on documents whose signing status is what the caller is asking about.
//
// The NSS check is the other half. pdfsig writes `NSS_Init failed` to stderr in
// an image carrying no certificate database, which is every image this module
// builds, and a report assembled from both streams would carry that line into
// every caller's output as though it were something the document said.
func (t *Tests) SignaturesReportsAnUnsignedDocumentInsteadOfFailing(ctx context.Context) error {
	report, err := pdf().Document(fixture(ledgerPdf)).Signatures(ctx)
	if err != nil {
		return fmt.Errorf("Signatures: %w", err)
	}
	if !strings.Contains(report, "does not contain any signatures") {
		return fmt.Errorf("expected the report to say the document carries no signatures, got:\n%s", report)
	}
	if strings.Contains(report, "NSS") {
		return fmt.Errorf("expected the report to carry the document's own report and not poppler's diagnostics, got:\n%s", report)
	}
	return nil
}

// ReportsOpenAnEncryptedDocumentWithThePassword asserts the document's password
// reaches all three reporting tools, and that each says the same thing without
// one.
//
// Each tool opens the document itself, so none of this follows from extraction
// or rendering already working: a password threaded into pdftotext's invocation
// and not into pdffonts' produces a module where a report on an encrypted
// document fails while everything else about it succeeds. Both branches are
// asserted for each, because the refusal is the half a caller reads — poppler
// reports every wrong password as `Incorrect password` whether one was supplied
// or not, so the message has to distinguish what the module knows and poppler
// does not.
func (t *Tests) ReportsOpenAnEncryptedDocumentWithThePassword(ctx context.Context) error {
	userPw, err := generatedPassword(ctx)
	if err != nil {
		return fmt.Errorf("generating the user password: %w", err)
	}
	ownerPw, err := generatedPassword(ctx)
	if err != nil {
		return fmt.Errorf("generating the owner password: %w", err)
	}
	user := dag.SetSecret("pdf-tests-reports-user-password", userPw)
	owner := dag.SetSecret("pdf-tests-reports-owner-password", ownerPw)
	encrypted := encryptedLedger(user, owner)

	reports := []struct {
		name string
		read func(doc *dagger.PdfDocument) (string, error)
		want string
	}{
		{"Fonts", func(doc *dagger.PdfDocument) (string, error) { return doc.Fonts(ctx) }, fontsHelvetica},
		// The encrypted fixture is ledgerPdf, which carries no XMP packet, so the
		// opened document's answer here is the absence — reported, not failed.
		{"Metadata", func(doc *dagger.PdfDocument) (string, error) { return doc.Metadata(ctx) }, "No XMP metadata"},
		{"Signatures", func(doc *dagger.PdfDocument) (string, error) { return doc.Signatures(ctx) }, "does not contain any signatures"},
	}

	for _, report := range reports {
		if _, err := report.read(pdf().Document(encrypted)); err == nil {
			return fmt.Errorf("%s: expected an encrypted document to be refused without a password", report.name)
		} else {
			for _, want := range []string{"encrypted", "no password was supplied", "WithUserPassword"} {
				if !strings.Contains(err.Error(), want) {
					return fmt.Errorf("%s: expected the refusal to mention %q, got: %v", report.name, want, err)
				}
			}
		}

		for _, opened := range []struct {
			name string
			doc  *dagger.PdfDocument
		}{
			{"WithUserPassword", pdf().Document(encrypted).WithUserPassword(user)},
			{"WithOwnerPassword", pdf().Document(encrypted).WithOwnerPassword(owner)},
		} {
			got, err := report.read(opened.doc)
			if err != nil {
				return fmt.Errorf("%s with %s: %w", report.name, opened.name, err)
			}
			if !strings.Contains(got, report.want) {
				return fmt.Errorf("%s with %s: expected the report to contain %q, got:\n%s",
					report.name, opened.name, report.want, got)
			}
		}
	}
	return nil
}

// fontRow picks one face's row out of pdffonts' table and splits it into fields.
func fontRow(report, face string) ([]string, error) {
	for _, line := range strings.Split(report, "\n") {
		if strings.HasPrefix(line, face) {
			return strings.Fields(line), nil
		}
	}
	return nil, fmt.Errorf("expected a row for %s, got:\n%s", face, report)
}

// fontEmbedded reads the `emb` column out of a row of pdffonts' table.
//
// It counts from the end because the columns before it can each carry a space —
// `Type 1` is one column, and a font name may hold one too — while the five that
// follow never do: emb, sub, uni, and the two halves of the object reference.
func fontEmbedded(fields []string) string {
	const embFromEnd = 5
	if len(fields) < embFromEnd {
		return ""
	}
	return fields[len(fields)-embFromEnd]
}

// TextReproducesTextLayerExactly asserts Text returns the document's text
// layer byte for byte, in page order, with a form feed between pages.
//
// The comparison is against the whole string rather than a per-page Contains
// sweep on purpose. Extraction has no tolerance to spend: the text is already in
// the file, and anything this module does to it — a lost page break, a stray
// leading newline, pages in the wrong order — is a defect and not a recognition
// error. A fixture whose content streams are readable is what makes an exact
// expectation writable at all.
func (t *Tests) TextReproducesTextLayerExactly(ctx context.Context) error {
	got, err := pdf().Document(fixture(ledgerPdf)).Convert().Text(ctx)
	if err != nil {
		return fmt.Errorf("Text: %w", err)
	}
	if want := ledgerText(1, ledgerPages); got != want {
		return fmt.Errorf("expected text:\n%q\ngot:\n%q", want, got)
	}
	return nil
}

// TextOnImageOnlyPdfReturnsNothing asserts the boundary with the tesseract
// module: a PDF carrying an image and no text layer extracts to nothing, and
// does not fail.
//
// This is the whole reason both modules exist. From poppler's point of view a
// page with no text is not an error — there is nothing wrong with the document
// and nothing wrong with the extraction — so the empty result is the only signal
// a caller gets that this document needs rasterizing and handing to OCR. A
// module that failed here instead would make the two paths impossible to choose
// between programmatically.
func (t *Tests) TextOnImageOnlyPdfReturnsNothing(ctx context.Context) error {
	got, err := pdf().Document(fixture(scanPdf)).Convert().Text(ctx)
	if err != nil {
		return fmt.Errorf("Text: %w", err)
	}
	// The page break survives: it says a page was read, which is exactly the
	// distinction between an image-only page and no page at all.
	if !strings.Contains(got, "\f") {
		return fmt.Errorf("expected a page break for the page that was read, got %q", got)
	}
	if text := strings.TrimSpace(strings.ReplaceAll(got, "\f", "")); text != "" {
		return fmt.Errorf("expected no text from an image-only PDF, got %q", text)
	}
	return nil
}

// TxtMatchesText asserts the file Txt returns carries exactly the bytes Text
// returns, so choosing between them is a plumbing decision and nothing more.
//
// The file is round-tripped through the module's own workdir rather than read
// straight off the handle: the point of Txt is that the bytes reach a filesystem
// intact, and File.Contents would confirm the engine's copy while saying nothing
// about the export a real consumer performs.
func (t *Tests) TxtMatchesText(ctx context.Context) error {
	conv := pdf().Document(fixture(ledgerPdf)).Convert()

	text, err := conv.Text(ctx)
	if err != nil {
		return fmt.Errorf("Text: %w", err)
	}
	// Txt returns an object, so the generated binding is lazy: its own error —
	// an unknown layout mode, an out-of-document page range — surfaces here,
	// when the file is finally resolved.
	const exported = "txt-matches-text.txt"
	if _, err := conv.Txt().Export(ctx, exported); err != nil {
		return fmt.Errorf("export Txt: %w", err)
	}
	got, err := dag.CurrentModule().WorkdirFile(exported).Contents(ctx)
	if err != nil {
		return fmt.Errorf("read exported Txt: %w", err)
	}
	if got != text {
		return fmt.Errorf("expected Txt to match Text\nText:\n%q\nTxt:\n%q", text, got)
	}
	return nil
}

// PageRangeNarrowsText asserts WithPageRange narrows extraction to the pages it
// names, and that the bounds are the 1-based inclusive ones poppler uses rather
// than an offset and a length.
func (t *Tests) PageRangeNarrowsText(ctx context.Context) error {
	const first, last = 4, 6

	got, err := pdf().Document(fixture(ledgerPdf)).
		WithPageRange(first, last).
		Convert().
		Text(ctx)
	if err != nil {
		return fmt.Errorf("Text: %w", err)
	}
	if want := ledgerText(first, last); got != want {
		return fmt.Errorf("expected text:\n%q\ngot:\n%q", want, got)
	}
	return nil
}

// PageRangeOpenEndedRunsToTheLastPage asserts a zero last means "to the end",
// which is the only way to name an open-ended range without first asking how
// many pages the document has.
func (t *Tests) PageRangeOpenEndedRunsToTheLastPage(ctx context.Context) error {
	const first = 10

	got, err := pdf().Document(fixture(ledgerPdf)).
		WithPageRange(first, 0).
		Convert().
		Text(ctx)
	if err != nil {
		return fmt.Errorf("Text: %w", err)
	}
	if want := ledgerText(first, ledgerPages); got != want {
		return fmt.Errorf("expected text:\n%q\ngot:\n%q", want, got)
	}
	return nil
}

// PageRangeRejectsInvalidBounds asserts every way of getting a range wrong is
// reported by naming the bound that was wrong.
//
// The out-of-document cases are the ones that justify checking at all: poppler
// renders nothing for them and exits 0, so a caller who asked for page 20 of a
// 12-page document would otherwise get an empty result indistinguishable from a
// document with no text in it.
func (t *Tests) PageRangeRejectsInvalidBounds(ctx context.Context) error {
	doc := pdf().Document(fixture(ledgerPdf))

	for _, tc := range []struct {
		name  string
		first int
		last  int
		want  []string
	}{
		{"zero first", 0, 3, []string{"first", "at least 1"}},
		{"negative last", 1, -2, []string{"last", "negative"}},
		{"inverted", 5, 3, []string{"last (3)", "first (5)"}},
		{"first past the end", 20, 0, []string{"first (20)", "12 pages"}},
		{"last past the end", 1, 13, []string{"last (13)", "12 pages"}},
	} {
		_, err := doc.WithPageRange(tc.first, tc.last).Convert().Text(ctx)
		if err == nil {
			return fmt.Errorf("%s: expected WithPageRange(%d, %d) to be rejected", tc.name, tc.first, tc.last)
		}
		for _, want := range tc.want {
			if !strings.Contains(err.Error(), want) {
				return fmt.Errorf("%s: expected the rejection to mention %q, got: %v", tc.name, want, err)
			}
		}
	}
	return nil
}

// DisablePageBreaksControlsFormFeeds asserts the page break is there by default
// and gone when asked for.
//
// The option is spelled as a disable rather than as a `pageBreaks bool`
// defaulting to true because a `+default=true` bool cannot be set false from the
// Go SDK — the zero value is dropped before it reaches the API — so the
// affirmative spelling would have produced an option no caller could turn off.
// That makes the second half of this test the one that would have caught it.
func (t *Tests) DisablePageBreaksControlsFormFeeds(ctx context.Context) error {
	conv := pdf().Document(fixture(ledgerPdf)).Convert()

	kept, err := conv.Text(ctx)
	if err != nil {
		return fmt.Errorf("Text: %w", err)
	}
	if got := strings.Count(kept, "\f"); got != ledgerPages {
		return fmt.Errorf("expected %d page breaks by default, got %d", ledgerPages, got)
	}

	dropped, err := conv.Text(ctx, dagger.PdfConvertTextOpts{DisablePageBreaks: true})
	if err != nil {
		return fmt.Errorf("Text with page breaks disabled: %w", err)
	}
	if got := strings.Count(dropped, "\f"); got != 0 {
		return fmt.Errorf("expected no page breaks, got %d", got)
	}
	// Dropping the separator must not drop the text: the two differ by exactly
	// the form feeds and nothing else.
	if want := strings.ReplaceAll(kept, "\f", ""); dropped != want {
		return fmt.Errorf("expected the same text without page breaks:\n%q\ngot:\n%q", want, dropped)
	}
	return nil
}

// LayoutModesProduceDifferentOrderings asserts the three layout modes are three
// different answers rather than degrees of one, on a page where they have to
// disagree.
//
// columns.pdf is built so they cannot coincide: two columns of prose, with the
// content stream written row-major — left cell, right cell, next row — while the
// page reads column-major. So reading order visits the whole left column first,
// content-stream order alternates between the columns from the first line, and
// physical layout puts the two columns side by side on one output line. A fixture
// whose stream order matched its reading order would let two of these three pass
// on the same output.
func (t *Tests) LayoutModesProduceDifferentOrderings(ctx context.Context) error {
	conv := pdf().Document(fixture(columnsPdf)).Convert()

	reading, err := conv.Text(ctx, dagger.PdfConvertTextOpts{Layout: dagger.PdfLayoutModeReading})
	if err != nil {
		return fmt.Errorf("Text in reading order: %w", err)
	}
	// The left column is read to its end before the right one begins.
	if strings.Index(reading, columnsLeftLast) > strings.Index(reading, columnsRightFirst) {
		return fmt.Errorf("expected %s before %s in reading order, got:\n%s",
			columnsLeftLast, columnsRightFirst, reading)
	}

	raw, err := conv.Text(ctx, dagger.PdfConvertTextOpts{Layout: dagger.PdfLayoutModeRaw})
	if err != nil {
		return fmt.Errorf("Text in raw order: %w", err)
	}
	// Content-stream order crosses to the right column on the first row, so the
	// right column's first word precedes the left column's last.
	if strings.Index(raw, columnsRightFirst) > strings.Index(raw, columnsLeftLast) {
		return fmt.Errorf("expected %s before %s in raw order, got:\n%s",
			columnsRightFirst, columnsLeftLast, raw)
	}

	physical, err := conv.Text(ctx, dagger.PdfConvertTextOpts{Layout: dagger.PdfLayoutModePhysical})
	if err != nil {
		return fmt.Errorf("Text in physical order: %w", err)
	}
	// Physical layout is the only one that puts both columns on one line, held
	// apart by the run of spaces standing in for the gutter.
	if !hasSideBySideLine(physical, columnsLeftFirst, columnsRightFirst) {
		return fmt.Errorf("expected %s and %s on one line in physical order, got:\n%s",
			columnsLeftFirst, columnsRightFirst, physical)
	}
	if hasSideBySideLine(reading, columnsLeftFirst, columnsRightFirst) {
		return fmt.Errorf("expected reading order not to place the columns side by side, got:\n%s", reading)
	}
	if hasSideBySideLine(raw, columnsLeftFirst, columnsRightFirst) {
		return fmt.Errorf("expected raw order not to place the columns side by side, got:\n%s", raw)
	}
	return nil
}

// hasSideBySideLine reports whether any one line carries both words separated by
// the run of spaces physical layout pads a gutter with. A single space between
// them is content-stream order joining two columns into one line, which is a
// different arrangement and must not match.
func hasSideBySideLine(text, left, right string) bool {
	for _, line := range strings.Split(text, "\n") {
		l := strings.Index(line, left)
		r := strings.Index(line, right)
		if l < 0 || r <= l {
			continue
		}
		if strings.Contains(line[l+len(left):r], "  ") {
			return true
		}
	}
	return false
}

// PngNamesEveryPageWithFourDigits asserts the page-naming contract on a
// document long enough for pdftoppm to disagree with it.
//
// pdftoppm pads a page number to the width of the document's page count, so a
// twelve-page document is named `page-01.png` through `page-12.png` and a
// one-page one `page-1.png`. Neither shape is the contract, and the widths are
// what make a bare sort across two documents' output wrong while a sort within
// either one is right — the tesseract module's Batch sorts what it is handed and
// has no way to know which width it is holding. Asserting the full ordered list
// covers both halves of the promise: the names, and that lexicographic order is
// page order.
func (t *Tests) PngNamesEveryPageWithFourDigits(ctx context.Context) error {
	got, err := pdf().Document(fixture(ledgerPdf)).Convert().Png().Entries(ctx)
	if err != nil {
		return fmt.Errorf("Png: %w", err)
	}
	return assertPageNames(got, pageNames("png", 1, ledgerPages))
}

// PngNarrowedRangeKeepsFourDigitNames asserts a page range narrows the render
// and leaves the naming alone.
//
// Nine pages is the interesting count: pdftoppm would name a nine-page
// *document* `page-1.png`, and the same nine pages taken out of this twelve-page
// one `page-01.png`, so the width it chose depends on the document rather than on
// what was rendered. Both normalize to the same four digits.
//
// The numbers are the source document's page numbers and not positions within
// the range, which is why the second case starts at `page-0004.png`. That keeps a
// rendered page traceable back to the page it came from.
func (t *Tests) PngNarrowedRangeKeepsFourDigitNames(ctx context.Context) error {
	doc := pdf().Document(fixture(ledgerPdf))

	for _, tc := range []struct{ first, last int }{
		{1, 9},
		{4, 6},
	} {
		got, err := doc.WithPageRange(tc.first, tc.last).Convert().Png().Entries(ctx)
		if err != nil {
			return fmt.Errorf("Png for pages %d-%d: %w", tc.first, tc.last, err)
		}
		if err := assertPageNames(got, pageNames("png", tc.first, tc.last)); err != nil {
			return fmt.Errorf("pages %d-%d: %s", tc.first, tc.last, err.Error())
		}
	}
	return nil
}

// PngSinglePageIsStillNumbered asserts a render of one page is `page-0001.png`
// and not `page.png`.
//
// Both routes to a single page are checked, because pdftoppm treats them
// differently: a one-page document is where it drops to a single-digit number,
// and historically to no number at all, while a one-page range of a longer
// document keeps the longer document's width. A consumer globbing for
// `page-*.png` finds nothing in the first case and a caller indexing by name
// finds the wrong file in the second.
func (t *Tests) PngSinglePageIsStillNumbered(ctx context.Context) error {
	onePageDoc, err := pdf().Document(fixture(scanPdf)).Convert().Png().Entries(ctx)
	if err != nil {
		return fmt.Errorf("Png of a one-page document: %w", err)
	}
	if err := assertPageNames(onePageDoc, pageNames("png", 1, 1)); err != nil {
		return fmt.Errorf("one-page document: %s", err.Error())
	}

	onePageRange, err := pdf().Document(fixture(ledgerPdf)).
		WithPageRange(1, 1).Convert().Png().Entries(ctx)
	if err != nil {
		return fmt.Errorf("Png of a one-page range: %w", err)
	}
	if err := assertPageNames(onePageRange, pageNames("png", 1, 1)); err != nil {
		return fmt.Errorf("one-page range: %s", err.Error())
	}
	return nil
}

// DpiDefaultsToOneFiftyAndScalesWithTheSetting asserts the default resolution is
// the documented 150 and that WithDpi multiplies the pixels accordingly.
//
// The default is asserted against the page's own geometry rather than against a
// remembered pixel count, so the assertion says "150 dpi of a US Letter page"
// rather than "1275 by 1650" — which is the claim the documentation actually
// makes.
func (t *Tests) DpiDefaultsToOneFiftyAndScalesWithTheSetting(ctx context.Context) error {
	conv := pdf().Document(fixture(ledgerPdf)).WithPageRange(1, 1).Convert()

	deflt, err := firstPageConfig(ctx, conv.Png(), "dpi-default")
	if err != nil {
		return fmt.Errorf("Png at the default dpi: %w", err)
	}
	wantW, wantH := pixelsAt(defaultDpi)
	if deflt.Width != wantW || deflt.Height != wantH {
		return fmt.Errorf("expected %dx%d at the default %d dpi, got %dx%d",
			wantW, wantH, defaultDpi, deflt.Width, deflt.Height)
	}

	doubled, err := firstPageConfig(ctx, conv.WithDpi(2*defaultDpi).Png(), "dpi-300")
	if err != nil {
		return fmt.Errorf("Png at %d dpi: %w", 2*defaultDpi, err)
	}
	wantW, wantH = pixelsAt(2 * defaultDpi)
	if doubled.Width != wantW || doubled.Height != wantH {
		return fmt.Errorf("expected %dx%d at %d dpi, got %dx%d",
			wantW, wantH, 2*defaultDpi, doubled.Width, doubled.Height)
	}
	return nil
}

// ScaleToOverridesDpi asserts WithScaleTo fixes the output's pixel size and wins
// over a resolution set alongside it.
//
// Both are set here on purpose: the override is implemented by leaving the
// resolution flag off the command line rather than by relying on poppler's own
// precedence, so this is the assertion that would catch the two being emitted
// together and whichever poppler happened to prefer winning silently.
func (t *Tests) ScaleToOverridesDpi(ctx context.Context) error {
	const scaleTo = 500

	cfg, err := firstPageConfig(ctx, pdf().Document(fixture(ledgerPdf)).
		WithPageRange(1, 1).
		Convert().
		WithDpi(2*defaultDpi).
		WithScaleTo(scaleTo).
		Png(), "scale-to")
	if err != nil {
		return fmt.Errorf("Png: %w", err)
	}
	// The page is scaled to fit the box while keeping its aspect ratio, so the
	// long side lands on the bound exactly and the short side follows from it.
	wantWidth := scaleTo*ledgerPageWidth/ledgerPageHeight + 1
	if cfg.Height != scaleTo || cfg.Width != wantWidth {
		return fmt.Errorf("expected %dx%d scaled to fit %d, got %dx%d",
			wantWidth, scaleTo, scaleTo, cfg.Width, cfg.Height)
	}
	if _, h := pixelsAt(2 * defaultDpi); cfg.Height == h {
		return fmt.Errorf("expected WithScaleTo to override WithDpi, got the %d dpi height %d",
			2*defaultDpi, h)
	}
	return nil
}

// RenderSettingsRejectNonPositiveValues asserts a resolution or a pixel bound
// that cannot mean anything is refused by naming the argument.
//
// Left to poppler these arrive much later as a complaint about `-r` or
// `-scale-to`, which names a flag the caller never wrote.
func (t *Tests) RenderSettingsRejectNonPositiveValues(ctx context.Context) error {
	conv := pdf().Document(fixture(ledgerPdf)).Convert()

	for _, tc := range []struct {
		name string
		conv *dagger.PdfConvert
		want []string
	}{
		{"zero dpi", conv.WithDpi(0), []string{"WithDpi", "dpi", "positive"}},
		{"negative dpi", conv.WithDpi(-72), []string{"WithDpi", "dpi", "positive"}},
		{"zero scale", conv.WithScaleTo(0), []string{"WithScaleTo", "pixels", "positive"}},
		{"negative scale", conv.WithScaleTo(-500), []string{"WithScaleTo", "pixels", "positive"}},
	} {
		if _, err := tc.conv.Png().Entries(ctx); err == nil {
			return fmt.Errorf("%s: expected the render to be rejected", tc.name)
		} else {
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					return fmt.Errorf("%s: expected the rejection to mention %q, got: %v", tc.name, want, err)
				}
			}
		}
	}
	return nil
}

// ColorModesProduceDifferentPixels asserts the three colour modes render three
// different images.
//
// It measures pixels rather than the PNG header, because the header does not
// move: poppler's PNG writer emits 8-bit RGB for all three modes, so a grayscale
// render is an RGB image whose channels happen to be equal and a monochrome one
// an RGB image whose pixels happen to be pure black or white. Asserting on bit
// depth would pass identically for all three and prove nothing.
//
// The three properties are mutually exclusive by construction: colour keeps
// chroma, grey drops chroma but keeps mid-tones, and mono drops both — flat tones
// are dithered into black and white rather than averaged into grey.
func (t *Tests) ColorModesProduceDifferentPixels(ctx context.Context) error {
	conv := pdf().Document(fixture(swatchPdf)).Convert().WithDpi(36)

	color, err := firstPagePixels(ctx, conv.Png(), "color-mode-color")
	if err != nil {
		return fmt.Errorf("Png in colour: %w", err)
	}
	if color.chroma == 0 {
		return fmt.Errorf("expected a colour render to keep chromatic pixels, got %+v", color)
	}

	gray, err := firstPagePixels(ctx, conv.WithColorMode(dagger.PdfColorModeGray).Png(), "color-mode-gray")
	if err != nil {
		return fmt.Errorf("Png in grayscale: %w", err)
	}
	if gray.chroma != 0 {
		return fmt.Errorf("expected a grayscale render to have no chromatic pixels, got %+v", gray)
	}
	if gray.midtone == 0 {
		return fmt.Errorf("expected a grayscale render to keep mid-tones, got %+v", gray)
	}

	mono, err := firstPagePixels(ctx, conv.WithColorMode(dagger.PdfColorModeMono).Png(), "color-mode-mono")
	if err != nil {
		return fmt.Errorf("Png in monochrome: %w", err)
	}
	if mono.chroma != 0 {
		return fmt.Errorf("expected a monochrome render to have no chromatic pixels, got %+v", mono)
	}
	if mono.midtone != 0 {
		return fmt.Errorf("expected a monochrome render to have no mid-tones, got %+v", mono)
	}
	return nil
}

// WithoutAnnotationsRemovesTheAnnotationLayer asserts the annotation layer is
// drawn by default and gone when asked for.
//
// annotated.pdf's page draws nothing but black text, so every chromatic pixel in
// a render of it is the annotation's appearance stream and nothing else. Without
// that separation the assertion could not tell an annotation that was not drawn
// from one that was drawn somewhere else on the page.
func (t *Tests) WithoutAnnotationsRemovesTheAnnotationLayer(ctx context.Context) error {
	conv := pdf().Document(fixture(annotatedPdf)).Convert().WithDpi(36)

	with, err := firstPagePixels(ctx, conv.Png(), "annotations-kept")
	if err != nil {
		return fmt.Errorf("Png with annotations: %w", err)
	}
	if with.chroma == 0 {
		return fmt.Errorf("expected the annotation to be drawn by default, got %+v", with)
	}

	without, err := firstPagePixels(ctx, conv.WithoutAnnotations().Png(), "annotations-hidden")
	if err != nil {
		return fmt.Errorf("Png without annotations: %w", err)
	}
	if without.chroma != 0 {
		return fmt.Errorf("expected no annotation pixels, got %+v", without)
	}
	// The page itself must survive the annotation being dropped: the text is
	// still there, antialiased into mid-tones.
	if without.midtone == 0 {
		return fmt.Errorf("expected the page's own content to survive, got %+v", without)
	}
	return nil
}

// JpegAndTiffFollowTheSameContract asserts the other two raster formats honour
// the naming contract and the geometry PNG does, differing only in the extension
// poppler chose for them.
//
// `-jpeg` writes `.jpg` and `-tiff` writes `.tif`, which are poppler's spellings
// and not this module's: a caller building a path from the function's name would
// get them wrong, so they are part of the documented contract.
func (t *Tests) JpegAndTiffFollowTheSameContract(ctx context.Context) error {
	conv := pdf().Document(fixture(ledgerPdf)).WithPageRange(1, 3).Convert()

	for _, tc := range []struct {
		name string
		ext  string
		dir  *dagger.Directory
	}{
		{"Jpeg", "jpg", conv.Jpeg()},
		{"Tiff", "tif", conv.Tiff()},
	} {
		got, err := tc.dir.Entries(ctx)
		if err != nil {
			return fmt.Errorf("%s: %w", tc.name, err)
		}
		if err := assertPageNames(got, pageNames(tc.ext, 1, 3)); err != nil {
			return fmt.Errorf("%s: %s", tc.name, err.Error())
		}
	}

	// Geometry is a property of the render and not of the container format, so
	// every format at the same resolution has to agree on it. JPEG is the one
	// checked because Go decodes it out of the box; TIFF's names above are the
	// half of the contract that can drift.
	cfg, err := firstPageConfig(ctx, conv.Jpeg(), "jpeg-geometry")
	if err != nil {
		return fmt.Errorf("Jpeg geometry: %w", err)
	}
	wantW, wantH := pixelsAt(defaultDpi)
	if cfg.Width != wantW || cfg.Height != wantH {
		return fmt.Errorf("expected %dx%d, got %dx%d", wantW, wantH, cfg.Width, cfg.Height)
	}
	return nil
}

// SplitWritesOnePdfPerPage asserts Split turns a twelve-page document into
// twelve one-page PDFs named to the same contract every render family member
// honours.
//
// The names alone would pass against a splitter that wrote twelve copies of page
// one, so each file is opened and read: one page in it, and that page's own
// marker in the text. ledgerPdf's markers are what make that check possible —
// twelve pages of identical text would extract the same however they were
// shuffled.
//
// pdfseparate numbers with the width the caller's pattern asks for rather than
// with the document's page count, so the four-digit contract here is this
// module's and not a coincidence of the fixture's length.
func (t *Tests) SplitWritesOnePdfPerPage(ctx context.Context) error {
	dir := pdf().Document(fixture(ledgerPdf)).Split()

	entries, err := dir.Entries(ctx)
	if err != nil {
		return fmt.Errorf("Split: %w", err)
	}
	if err := assertPageNames(entries, pageNames("pdf", 1, ledgerPages)); err != nil {
		return err
	}

	for _, page := range []int{1, 7, ledgerPages} {
		name := fmt.Sprintf("page-%04d.pdf", page)
		one := pdf().Document(dir.File(name))

		count, err := one.PageCount(ctx)
		if err != nil {
			return fmt.Errorf("PageCount of %s: %w", name, err)
		}
		if count != 1 {
			return fmt.Errorf("expected %s to hold exactly one page, got %d", name, count)
		}

		got, err := one.Convert().Text(ctx)
		if err != nil {
			return fmt.Errorf("Text of %s: %w", name, err)
		}
		if want := ledgerText(page, page); got != want {
			return fmt.Errorf("expected %s to carry page %d:\n%q\ngot:\n%q", name, page, want, got)
		}
	}
	return nil
}

// SplitNarrowsToThePageRange asserts the page bounds reach pdfseparate in all
// three of the shapes a range comes in, and that a range it cannot honour is
// refused before it runs.
//
// The refusal is the half worth asserting. Left to pdfseparate, a last bound past
// the end of the document is not caught up front at all: it separates every page
// it can and *then* fails with `Internal Error: Illegal pageNo: 13(12)`, so the
// caller gets a message about poppler's internals attached to a directory that
// was half written. Checking the bounds against the document first is what turns
// that into a sentence naming the builder and the page count.
func (t *Tests) SplitNarrowsToThePageRange(ctx context.Context) error {
	doc := pdf().Document(fixture(ledgerPdf))

	for _, tc := range []struct {
		name        string
		first, last int
		wantFirst   int
		wantLast    int
	}{
		{"a span", 4, 6, 4, 6},
		// A one-page range is where pdfseparate writes a single-digit number, so
		// this is also the case the padding contract has to survive.
		{"one page", 2, 2, 2, 2},
		{"open ended", 10, 0, 10, ledgerPages},
	} {
		got, err := doc.WithPageRange(tc.first, tc.last).Split().Entries(ctx)
		if err != nil {
			return fmt.Errorf("%s: Split: %w", tc.name, err)
		}
		if err := assertPageNames(got, pageNames("pdf", tc.wantFirst, tc.wantLast)); err != nil {
			return fmt.Errorf("%s: %s", tc.name, err.Error())
		}
	}

	if _, err := doc.WithPageRange(1, ledgerPages+1).Split().Entries(ctx); err == nil {
		return fmt.Errorf("expected a range past the end of the document to be rejected")
	} else {
		for _, want := range []string{"last (13)", "12 pages"} {
			if !strings.Contains(err.Error(), want) {
				return fmt.Errorf("expected the rejection to mention %q, got: %v", want, err)
			}
		}
	}
	return nil
}

// MergePreservesTheOrderOfItsSources asserts the merged document's pages come
// out in the order the slice named them, which is the whole reason Merge takes
// an ordered slice rather than a directory.
//
// The sources are deliberately handed over out of page order — 3, 1, 2 — because
// any order-preserving implementation and any order-losing one agree on a slice
// that was already sorted. Reading the text back is what says the pages landed
// where they were put: the page count alone would pass on a merge that shuffled
// them.
func (t *Tests) MergePreservesTheOrderOfItsSources(ctx context.Context) error {
	pages := pdf().Document(fixture(ledgerPdf)).WithPageRange(1, 3).Split()

	order := []int{3, 1, 2}
	sources := make([]*dagger.File, 0, len(order))
	var want strings.Builder
	for _, page := range order {
		sources = append(sources, pages.File(fmt.Sprintf("page-%04d.pdf", page)))
		want.WriteString(ledgerText(page, page))
	}

	merged := pdf().Document(pdf().Merge(sources))
	count, err := merged.PageCount(ctx)
	if err != nil {
		return fmt.Errorf("PageCount of the merged document: %w", err)
	}
	if count != len(order) {
		return fmt.Errorf("expected %d pages, got %d", len(order), count)
	}
	got, err := merged.Convert().Text(ctx)
	if err != nil {
		return fmt.Errorf("Text of the merged document: %w", err)
	}
	if got != want.String() {
		return fmt.Errorf("expected the pages in the order given:\n%q\ngot:\n%q", want.String(), got)
	}

	// One source is a legal merge and produces that source's document, so a
	// caller merging a computed list does not have to special-case its length.
	one := pdf().Document(pdf().Merge([]*dagger.File{pages.File("page-0002.pdf")}))
	got, err = one.Convert().Text(ctx)
	if err != nil {
		return fmt.Errorf("Text of a one-source merge: %w", err)
	}
	if want := ledgerText(2, 2); got != want {
		return fmt.Errorf("expected a one-source merge to carry page 2:\n%q\ngot:\n%q", want, got)
	}
	return nil
}

// MergeRejectsWhatItCannotMerge asserts the two ways a merge goes wrong are
// reported by naming the argument that was wrong.
//
// The empty slice is a caller error with no useful answer — there is no document
// to return and no empty PDF worth inventing — and pdfunite's own answer to it is
// its usage text, which describes a command line the caller never wrote. The
// module refuses it before the container starts, for that reason.
//
// The unreadable source is the case the mount legend exists for. pdfunite names
// the file it could not read, and the name it uses is this module's mount path,
// so without the legend the one piece of information identifying which argument
// was at fault is a path the caller has never seen.
func (t *Tests) MergeRejectsWhatItCannotMerge(ctx context.Context) error {
	_, err := pdf().Merge(nil).Contents(ctx)
	if err == nil {
		return fmt.Errorf("expected a merge of no sources to be rejected")
	}
	for _, want := range []string{"Merge", "at least one PDF", "order"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the rejection to mention %q, got: %v", want, err)
		}
	}
	// pdfunite answers an empty command line with its usage text, which is a
	// description of a command line the caller did not write.
	if strings.Contains(err.Error(), "Usage: pdfunite") {
		return fmt.Errorf("expected the module's own rejection rather than poppler's usage text, got: %v", err)
	}

	notPdf := dag.Directory().WithNewFile("notes.txt", "this is not a PDF").File("notes.txt")
	page := pdf().Document(fixture(ledgerPdf)).WithPageRange(1, 1).Split().File("page-0001.pdf")

	_, err = pdf().Merge([]*dagger.File{page, notPdf}).Contents(ctx)
	if err == nil {
		return fmt.Errorf("expected a merge of an unreadable source to be rejected")
	}
	for _, want := range []string{"Merge", "source-0002", "numbered from 1"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the failure to mention %q, got: %v", want, err)
		}
	}
	return nil
}

// SplitThenMergeRoundTripsTheDocument asserts the two halves of this pair
// compose back into the document they started from.
//
// It is the assertion that says these are structural operations rather than
// conversions. Each one alone could pass its own tests while quietly dropping
// what it does not understand — a page's annotations, its size, the text layer
// under it — and the round trip is where that shows up: twelve separated pages
// put back together have to extract to the same text, in the same order, on
// pages of the same size.
//
// The names the split wrote are what the merge is driven by, in page order, which
// is also the practical shape of the pair: split, do something to the pages,
// merge the ones that survived.
func (t *Tests) SplitThenMergeRoundTripsTheDocument(ctx context.Context) error {
	original := pdf().Document(fixture(ledgerPdf))
	pages := original.Split()

	sources := make([]*dagger.File, 0, ledgerPages)
	for page := 1; page <= ledgerPages; page++ {
		sources = append(sources, pages.File(fmt.Sprintf("page-%04d.pdf", page)))
	}
	rebuilt := pdf().Document(pdf().Merge(sources))

	count, err := rebuilt.PageCount(ctx)
	if err != nil {
		return fmt.Errorf("PageCount of the rebuilt document: %w", err)
	}
	if count != ledgerPages {
		return fmt.Errorf("expected %d pages after the round trip, got %d", ledgerPages, count)
	}

	got, err := rebuilt.Convert().Text(ctx)
	if err != nil {
		return fmt.Errorf("Text of the rebuilt document: %w", err)
	}
	want, err := original.Convert().Text(ctx)
	if err != nil {
		return fmt.Errorf("Text of the original: %w", err)
	}
	if got != want {
		return fmt.Errorf("expected the round trip to preserve the text:\n%q\ngot:\n%q", want, got)
	}

	// The pages keep their geometry, which is the property a re-render would lose
	// while still extracting to the same text.
	info, err := rebuilt.Info(ctx)
	if err != nil {
		return fmt.Errorf("Info of the rebuilt document: %w", err)
	}
	if wantSize := fmt.Sprintf("%d x %d pts", ledgerPageWidth, ledgerPageHeight); !strings.Contains(info, wantSize) {
		return fmt.Errorf("expected the pages to keep their size %q, got:\n%s", wantSize, info)
	}
	return nil
}

// SplitAndMergeCannotOpenAnEncryptedDocument asserts the limitation these two
// carry that nothing else in the module does, and asserts it is reported as that
// rather than as a wrong password.
//
// pdfseparate and pdfunite take no `-upw` and no `-opw` — passing one is a usage
// error, not a wrong password — so an encrypted document is one they cannot be
// made to open. What poppler says about it is `Incorrect password`, the same
// sentence every other tool in the suite produces for a password that did not
// work, and the module's usual reading of that line names WithUserPassword. Here
// that would send a caller to a builder that changes nothing, which is why the
// password case is asserted alongside the passwordless one: both have to arrive
// at the same message.
func (t *Tests) SplitAndMergeCannotOpenAnEncryptedDocument(ctx context.Context) error {
	userPw, err := generatedPassword(ctx)
	if err != nil {
		return fmt.Errorf("generating the user password: %w", err)
	}
	ownerPw, err := generatedPassword(ctx)
	if err != nil {
		return fmt.Errorf("generating the owner password: %w", err)
	}
	user := dag.SetSecret("pdf-tests-pages-user-password", userPw)
	owner := dag.SetSecret("pdf-tests-pages-owner-password", ownerPw)
	encrypted := encryptedLedger(user, owner)

	for _, tc := range []struct {
		name string
		doc  *dagger.PdfDocument
	}{
		{"without a password", pdf().Document(encrypted)},
		{"with the password", pdf().Document(encrypted).WithUserPassword(user)},
	} {
		_, err := tc.doc.Split().Entries(ctx)
		if err == nil {
			return fmt.Errorf("%s: expected Split of an encrypted document to be refused", tc.name)
		}
		for _, want := range []string{
			"Split", "encrypted", "pdfseparate has no password option", "qpdf --decrypt",
		} {
			if !strings.Contains(err.Error(), want) {
				return fmt.Errorf("%s: expected the refusal to mention %q, got: %v", tc.name, want, err)
			}
		}
	}

	page := pdf().Document(fixture(ledgerPdf)).WithPageRange(1, 1).Split().File("page-0001.pdf")
	_, err = pdf().Merge([]*dagger.File{page, encrypted}).Contents(ctx)
	if err == nil {
		return fmt.Errorf("expected a merge with an encrypted source to be refused")
	}
	for _, want := range []string{
		"Merge", "encrypted", "pdfunite has no password option", "source-0002",
	} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the refusal to mention %q, got: %v", want, err)
		}
	}
	return nil
}

// EncryptedDocumentNeedsThePassword asserts an encrypted document opens with the
// password it was encrypted under, fails without one, and says so by naming the
// encryption.
//
// The message matters as much as the failure. poppler reports every wrong
// password identically — `Incorrect password`, whether one was supplied or not,
// because it tries the empty password when given none — so passing that through
// would tell a caller who supplied nothing that what they supplied was wrong.
// Both branches are asserted here for that reason.
//
// The passwords are generated per run with dag.Random().Sha256 rather than
// written down, so no password literal ever enters git, and reach qpdf and
// poppler alike through the environment rather than argv: a password in argv is
// visible in every Dagger trace, which is exactly what the module's own posture
// avoids.
func (t *Tests) EncryptedDocumentNeedsThePassword(ctx context.Context) error {
	userPw, err := generatedPassword(ctx)
	if err != nil {
		return fmt.Errorf("generating the user password: %w", err)
	}
	ownerPw, err := generatedPassword(ctx)
	if err != nil {
		return fmt.Errorf("generating the owner password: %w", err)
	}
	wrongPw, err := generatedPassword(ctx)
	if err != nil {
		return fmt.Errorf("generating the wrong password: %w", err)
	}

	user := dag.SetSecret("pdf-tests-user-password", userPw)
	owner := dag.SetSecret("pdf-tests-owner-password", ownerPw)
	wrong := dag.SetSecret("pdf-tests-wrong-password", wrongPw)
	encrypted := encryptedLedger(user, owner)

	// A document that is encrypted at all is one poppler will not read, so the
	// page count is as far as it gets without a password.
	_, err = pdf().Document(encrypted).PageCount(ctx)
	if err == nil {
		return fmt.Errorf("expected an encrypted document to be refused without a password")
	}
	for _, want := range []string{"encrypted", "no password was supplied", "WithUserPassword"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the refusal to mention %q, got: %v", want, err)
		}
	}

	// The per-page formats reach poppler by a different route than PageCount
	// does — they resolve the document's page count inside the same exec that
	// renders, from a shell rather than from Go — and an encrypted document has
	// to be refused just as clearly along it. Without this the whole per-page
	// family's encryption behaviour would rest on the raster path's assertion,
	// which does not exercise that shell at all.
	_, err = pdf().Document(encrypted).Convert().Svg().Entries(ctx)
	if err == nil {
		return fmt.Errorf("expected Svg on an encrypted document to be refused without a password")
	}
	for _, want := range []string{"encrypted", "no password was supplied"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected Svg's refusal to mention %q, got: %v", want, err)
		}
	}

	_, err = pdf().Document(encrypted).WithUserPassword(wrong).PageCount(ctx)
	if err == nil {
		return fmt.Errorf("expected the wrong password to be refused")
	}
	for _, want := range []string{"encrypted", "did not open it"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the refusal to mention %q, got: %v", want, err)
		}
	}

	// The user password grants reading, and the owner password grants it too —
	// it additionally clears the permission bits, which is why a caller entitled
	// to ignore `copy:no` reaches for it.
	for _, tc := range []struct {
		name string
		doc  *dagger.PdfDocument
	}{
		{"WithUserPassword", pdf().Document(encrypted).WithUserPassword(user)},
		{"WithOwnerPassword", pdf().Document(encrypted).WithOwnerPassword(owner)},
	} {
		count, err := tc.doc.PageCount(ctx)
		if err != nil {
			return fmt.Errorf("%s: PageCount: %w", tc.name, err)
		}
		if count != ledgerPages {
			return fmt.Errorf("%s: expected PageCount %d, got %d", tc.name, ledgerPages, count)
		}
		got, err := tc.doc.Convert().Text(ctx)
		if err != nil {
			return fmt.Errorf("%s: Text: %w", tc.name, err)
		}
		if want := ledgerText(1, ledgerPages); got != want {
			return fmt.Errorf("%s: expected text:\n%q\ngot:\n%q", tc.name, want, got)
		}
	}
	return nil
}

// generatedPassword returns a fresh password for one encryption round-trip, so
// no password literal ever enters git.
//
// It is truncated to 32 characters because that is as much of a password as
// poppler reads — it cuts one to the 32 bytes PDF revisions before 2.0 allowed,
// even for AES-256, whose own limit is 127. qpdf honours the full length, so a
// longer password here produces a document qpdf is happy with and poppler
// reports `Incorrect password` for however correctly it is passed. That is
// documented on WithUserPassword; the sharp end of it is that the test would
// fail in a way that looks exactly like a defect in this module's secret
// handling.
//
// 32 hex characters is 128 bits of entropy, which is not the part of this that
// matters — the fixture lives for the length of one test.
func generatedPassword(ctx context.Context) (string, error) {
	const popplerPasswordLimit = 32

	hex, err := dag.Random().Sha256(ctx, dagger.RandomSha256Opts{N: 32})
	if err != nil {
		return "", err
	}
	if len(hex) < popplerPasswordLimit {
		return "", fmt.Errorf("expected at least %d characters, got %q", popplerPasswordLimit, hex)
	}
	return hex[:popplerPasswordLimit], nil
}

// encryptedLedger returns ledgerPdf encrypted under the given passwords.
//
// qpdf is installed into the module's own image rather than pulled as a separate
// one so the fixture is encrypted by the same Alpine release that then reads it,
// and the passwords reach it through the environment for the same reason the
// module does it that way.
func encryptedLedger(user, owner *dagger.Secret) *dagger.File {
	const (
		plain  = "/tmp/plain.pdf"
		cipher = "/tmp/encrypted.pdf"
	)
	return pdf().Container().
		WithExec([]string{"apk", "add", "--no-cache", "qpdf"}).
		WithMountedFile(plain, fixture(ledgerPdf)).
		WithSecretVariable("QPDF_USER_PASSWORD", user).
		WithSecretVariable("QPDF_OWNER_PASSWORD", owner).
		WithExec([]string{"sh", "-c",
			`qpdf --encrypt --user-password="$QPDF_USER_PASSWORD"` +
				` --owner-password="$QPDF_OWNER_PASSWORD" --bits=256 -- ` +
				plain + " " + cipher}).
		File(cipher)
}

// pixelsAt is what a ledgerPdf page measures in pixels when rendered at the given
// resolution.
func pixelsAt(dpi int) (width, height int) {
	return ledgerPageWidth * dpi / pointsPerInch, ledgerPageHeight * dpi / pointsPerInch
}

// pageNames is the ordered list of file names a render of pages first..last has
// to produce.
func pageNames(ext string, first, last int) []string {
	names := make([]string, 0, last-first+1)
	for page := first; page <= last; page++ {
		names = append(names, fmt.Sprintf("page-%04d.%s", page, ext))
	}
	return names
}

// assertPageNames compares a rendered directory's listing against the contract,
// in order. Directory.Entries is sorted, so equality here is simultaneously an
// assertion that lexicographic order is page order.
func assertPageNames(got, want []string) error {
	if !slices.Equal(got, want) {
		return fmt.Errorf("expected pages %v, got %v", want, got)
	}
	return nil
}

// exportedDir writes a rendered directory to the module's workdir and returns the
// path plus the cleanup for it.
//
// Rendered pages are read off a real filesystem rather than through
// File.Contents, which is a GraphQL String and so cannot carry arbitrary bytes
// intact. The directory name is per-test so the suite's parallel fan-out cannot
// have two tests writing over each other.
func exportedDir(ctx context.Context, dir *dagger.Directory, prefix string) (string, func(), error) {
	tmp, err := os.MkdirTemp(".", "pdf-"+prefix+"-")
	if err != nil {
		return "", nil, fmt.Errorf("temp dir: %w", err)
	}
	cleanup := func() { os.RemoveAll(tmp) }
	if _, err := dir.Export(ctx, tmp); err != nil {
		cleanup()
		return "", nil, err
	}
	return tmp, cleanup, nil
}

// firstPageConfig decodes the header of the first page of a rendered directory,
// which is all the geometry assertions need.
func firstPageConfig(ctx context.Context, dir *dagger.Directory, prefix string) (image.Config, error) {
	tmp, cleanup, err := exportedDir(ctx, dir, prefix)
	if err != nil {
		return image.Config{}, err
	}
	defer cleanup()

	path, err := firstPagePath(tmp)
	if err != nil {
		return image.Config{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return image.Config{}, err
	}
	defer f.Close()

	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return image.Config{}, fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return cfg, nil
}

// pixelStats is what a colour-mode assertion measures: whether any pixel carries
// colour, and whether any carries a tone that is neither black nor white.
type pixelStats struct {
	total   int
	chroma  int
	midtone int
}

// firstPagePixels decodes the first page of a rendered directory and summarises
// its pixels.
func firstPagePixels(ctx context.Context, dir *dagger.Directory, prefix string) (pixelStats, error) {
	tmp, cleanup, err := exportedDir(ctx, dir, prefix)
	if err != nil {
		return pixelStats{}, err
	}
	defer cleanup()

	path, err := firstPagePath(tmp)
	if err != nil {
		return pixelStats{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return pixelStats{}, err
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		return pixelStats{}, fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}

	var stats pixelStats
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			// RGBA returns 16-bit alpha-premultiplied channels; the top byte is
			// the 8-bit value poppler actually wrote.
			r8, g8, b8 := r>>8, g>>8, b>>8
			stats.total++
			switch {
			case r8 != g8 || g8 != b8:
				stats.chroma++
			case r8 != 0 && r8 != 0xff:
				stats.midtone++
			}
		}
	}
	if stats.total == 0 {
		return stats, fmt.Errorf("%s decoded to an empty image", filepath.Base(path))
	}
	return stats, nil
}

// firstPagePath is the lexicographically first rendered page in a directory,
// which the naming contract makes the document's first rendered page.
func firstPagePath(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("%s holds no rendered pages", dir)
	}
	slices.Sort(names)
	return filepath.Join(dir, names[0]), nil
}

// ledgerText is what ledgerPdf's pages first..last extract to, inclusive.
//
// pdftotext ends every page with a form feed, the last one included, and leaves
// the blank line the fixture's trailing line advance produces. Both are
// poppler's conventions rather than this module's, and reproducing them here is
// what makes the expectation an assertion about the module instead of about
// poppler.
func ledgerText(first, last int) string {
	var b strings.Builder
	for page := first; page <= last; page++ {
		fmt.Fprintf(&b, "Page %d of the ledger.\nMarker %s.\n\n\f", page, ledgerMarkers[page-1])
	}
	return b.String()
}

// pdf is the module under test, assembled with its defaults.
func pdf() *dagger.Pdf {
	return dag.Pdf()
}

// fixture resolves a committed PDF out of this module's own source.
func fixture(name string) *dagger.File {
	return dag.CurrentModule().Source().File("fixtures/" + name)
}
