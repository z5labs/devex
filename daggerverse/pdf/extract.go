package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"dagger/pdf/internal/dagger"
)

const (
	// imagesRoot is the OUTPUTBASE pdfimages is handed, and is deliberately not
	// the name the returned files carry. pdfimages appends `-<page>-<num>.<ext>`
	// to it — hence no trailing separator here — and the rename pass that follows
	// globs what it wrote, so the two names have to differ or the pass would find
	// the files it had already renamed and rename them again.
	imagesRoot      = outputDir + "/" + imagesRawName
	imagesRawName   = "raw"
	imagesRawPrefix = imagesRawName + "-"

	// imageNamePrefix and imagePageInfix spell the naming contract an extracted
	// image comes back under: `image-0000-page-0001.png`. See EmbeddedImages for
	// what each number is.
	imageNamePrefix = "image-"
	imagePageInfix  = "-page-"

	// pageNumbersFlag makes pdfimages put the source page in each file name,
	// which is the only place that information survives at all — the extracted
	// image itself carries no record of the page it was drawn on.
	pageNumbersFlag = "-p"

	// printFilenamesFlag makes pdfimages report what it wrote on stdout, which is
	// how this module tells "the document carries no images" apart from a run that
	// wrote none for some other reason. pdfimages exits 0 either way.
	printFilenamesFlag = "-print-filenames"

	// listFlag makes pdfdetach report the document's embedded files instead of
	// saving them, and is run alongside -saveall in the same exec: the report is
	// the only thing that says how many files there were, pdfdetach being silent
	// about what it saved.
	listFlag = "-list"

	// saveAllFlag saves every embedded file, under the name the document gives it,
	// into the working directory.
	saveAllFlag = "-saveall"

	// embeddedFilesSuffix ends pdfdetach's count line — `2 embedded files` — and
	// noEmbeddedFilesCount is what that line reads for a document carrying none.
	embeddedFilesSuffix  = "embedded files"
	noEmbeddedFilesCount = 0

	// traversalMarker is what poppler says when an attachment's own name has a
	// path in it. It refuses the whole -saveall rather than that one file, which
	// is why it earns a message of its own.
	traversalMarker = "Preventing directory traversal"
)

// imageNormalizeScript renames every image pdfimages just wrote so both numbers
// in its name are zero-padded to a fixed width, and is this family's version of
// the pass the raster renders go through.
//
// It exists for the same reason: pdfimages pads with `%03d`, so the thousandth
// image of a document widens the field and lexicographic order stops being
// document order in the middle of one directory. The width here is the greater of
// the module's minimum and whatever the widest number needs, so a document past
// what four digits can express stays uniform.
//
// The extension is read off each file rather than passed in, unlike the raster
// pass. It has to be: ORIGINAL and ALL write a directory of mixed extensions,
// because which one an image gets depends on how that image was encoded.
var imageNormalizeScript = strings.Join([]string{
	// POSIX printf reads a leading-zero argument as an octal constant, so `010`
	// would be 8 and `009` a diagnostic. Stripping the zeros first is the same
	// dance normalizeScript does, factored out here because two numbers need it.
	`strip0() {`,
	`	s=$(printf '%s' "$1" | sed 's/^0*//')`,
	`	[ -n "$s" ] || s=0`,
	`	printf '%s' "$s"`,
	`}`,
	`width=` + strconv.Itoa(minPageNumberWidth),
	`for f in ` + imagesGlob + `; do`,
	`	[ -e "$f" ] || continue`,
	`	rest=${f##*/` + imagesRawPrefix + `}`,
	`	page=${rest%%-*}`,
	`	num=${rest#*-}`,
	`	num=${num%%.*}`,
	`	if [ ${#page} -gt "$width" ]; then width=${#page}; fi`,
	`	if [ ${#num} -gt "$width" ]; then width=${#num}; fi`,
	`done`,
	`for f in ` + imagesGlob + `; do`,
	`	[ -e "$f" ] || continue`,
	`	rest=${f##*/` + imagesRawPrefix + `}`,
	`	page=${rest%%-*}`,
	`	num=${rest#*-}`,
	`	ext=${num#*.}`,
	`	num=${num%%.*}`,
	`	pp=$(printf "%0${width}d" "$(strip0 "$page")")`,
	`	nn=$(printf "%0${width}d" "$(strip0 "$num")")`,
	`	mv "$f" "` + outputDir + `/` + imageNamePrefix + `$nn` + imagePageInfix + `$pp.$ext"`,
	`done`,
}, "\n")

// imagesGlob matches every file pdfimages wrote, whatever its extension and
// whatever width the tool numbered it to.
const imagesGlob = outputDir + `/` + imagesRawPrefix + `*`

// EmbeddedImages extracts the image objects the document *contains* and returns
// the directory holding them, named `image-0000-page-0001.png` and so on.
//
// This is not Convert().Png(), and the difference is the reason the function is
// named this way. Convert().Png() *rasterizes a page*: it draws everything on
// the page — text, vectors, annotations, images — into a new bitmap at whatever
// resolution WithDpi names, and the pixels it produces have never existed before.
// This returns the image objects themselves, decoded no further than it has to
// be: for a scanned document that is the scan, at the resolution it was scanned
// at, with no resampling in between. Which is why it is the better OCR input of
// the two — every render is a resampling of an image that was already pixels, and
// recognition accuracy is exactly what that costs.
//
// It follows that WithDpi, WithColorMode, WithScaleTo and WithoutAnnotations have
// no meaning here and are not reachable from this function at all: they live on
// Convert, which is where rendering decisions belong. There is nothing to choose
// about the resolution of an image that already has one. By the same token this
// returns *nothing* for a page whose content is text and vectors — there is no
// image object on it to extract, and Convert().Png() is what draws such a page.
//
// The format is the one real choice, and it is required rather than defaulted
// because every member either keeps the document's encoding or replaces it, and
// no one of those is the obviously right thing to do quietly. See ImageFormat.
// ORIGINAL is worth reading twice: it keeps JPEG, JPEG 2000, JBIG2 and CCITT G4
// streams byte for byte, and for an image encoded in none of those — a raw or
// Flate-compressed bitmap, which is most PDFs that were not born as scans — it
// falls back to netpbm, so a `.ppm` or `.pgm` in the result is not a failure but
// an image that had no encoding to keep. ALL is the member whose fallback is PNG
// instead, and is what to reach for when every image has to be readable by
// something ordinary.
//
// The names carry both numbers poppler reports, padded and reordered but not
// renumbered. The one after `image-` is the image's index in this extraction,
// 0-based, which for an unnarrowed run is the `num` column of
// `pdfimages -list`; the one after `-page-` is the 1-based source page it was
// drawn on. The index comes first so lexicographic order is document order —
// which it is, images being extracted in page order — and the word `image` comes
// first of all so a directory of these is never mistaken for a directory of
// rendered pages.
//
// A page can carry several images and most pages carry none, so there is no
// one-file-per-page contract here of the kind every render honours. A document
// carrying no images at all is refused rather than returned as an empty
// directory: pdfimages exits 0 for it, so the directory would be indistinguishable
// from an extraction that failed to write anything, and the overwhelmingly common
// thing a caller does with this function is export it.
//
// WithPageRange narrows it. Note that it renumbers nothing but does restart the
// image index, that index being poppler's count for the run rather than for the
// document: pages 2 through 3 of a document whose first image is on page 1 come
// back starting at `image-0000-page-0002`. The page number stays the source
// document's, the same promise Split makes.
func (d *Document) EmbeddedImages(
	ctx context.Context,
	// Encoding to write each extracted image in.
	format ImageFormat,
) (*dagger.Directory, error) {
	formatFlags, ok := format.flags()
	if !ok {
		return nil, fmt.Errorf(
			"EmbeddedImages: unknown image format %q: must be one of %s",
			string(format), imageFormatNames())
	}
	if err := d.validateRange(ctx); err != nil {
		return nil, err
	}

	flags := append([]string(nil), formatFlags...)
	flags = append(flags, pageNumbersFlag, printFilenamesFlag)
	flags = append(flags, d.rangeArgs()...)

	// set -e is what makes a document poppler cannot open fail here, carrying its
	// own message, rather than turning into an empty directory a caller would read
	// as a document with no images in it.
	script := strings.Join([]string{
		"set -e",
		"mkdir -p " + outputDir,
		d.command("pdfimages", flags, sourcePath, imagesRoot),
		imageNormalizeScript,
	}, "\n")

	res, err := d.runScript(ctx, "EmbeddedImages", script)
	if err != nil {
		return nil, err
	}
	// -print-filenames named every file it wrote, so an empty stdout is the
	// document saying it carries no images. Nothing else about the run says so:
	// pdfimages exits 0 for a document with no images and for a document with a
	// hundred alike.
	if strings.TrimSpace(res.stdout) == "" {
		return nil, fmt.Errorf(
			"EmbeddedImages: this document carries no image objects%s: there is nothing to extract, and a page of text and vectors is drawn rather than extracted — Convert().Png() is what renders one. `pdfimages -list`, through Container, answers the same question without failing",
			d.rangeSuffix())
	}
	return res.container.Directory(outputDir), nil
}

// Attachments extracts the files embedded in the document and returns the
// directory holding them, each under the name the document gives it.
//
// This is the other half of what a PDF can carry that is not a page. An embedded
// file is a whole file attached to the document — the machine-readable XML a
// ZUGFeRD or Factur-X invoice carries alongside its human-readable page, a
// spreadsheet behind a report, a CAD file behind a drawing — and it is not drawn,
// not rendered and not part of any page's content. Neither Convert nor
// EmbeddedImages will ever produce it: this is the only function that reaches it.
//
// The names are the document's own, not this module's, and that is the one place
// this function departs from every other directory-returning function here. An
// attachment's name is *data* — it is what the producer called the file and what
// the consumer expects to find — so normalizing it to a contract the way the page
// families do would destroy the thing that makes the result usable. A caller
// wanting the invoice XML asks for `invoice.xml`.
//
// The flip side is that poppler polices those names, and polices them coarsely:
// an attachment whose name carries a path — `../elsewhere.xml` — makes pdfdetach
// refuse the *whole* extraction with `Preventing directory traversal` rather than
// skip that one file, so one such name means no attachments at all. That is
// reported as what it is, carrying the list of names the document holds, because
// those names are what `pdfdetach -savefile` needs to fetch them one at a time
// through Container.
//
// A document carrying no embedded files is refused rather than returned as an
// empty directory, for the reason EmbeddedImages gives: pdfdetach exits 0 for it,
// so the directory would be indistinguishable from an extraction that wrote
// nothing. `pdfdetach -list` is the report that answers "does this carry any"
// without failing, and the message names it.
//
// WithPageRange does not narrow it. An embedded file hangs off the document's
// catalog rather than off any page — pdfdetach has no page bounds at all — which
// is the same reason it does not narrow Metadata or Signatures. The passwords do
// reach it, unlike Split and Merge: pdfdetach takes both.
func (d *Document) Attachments(ctx context.Context) (*dagger.Directory, error) {
	// The report and the save run in one exec, in that order, because the report
	// is the only thing that says how many files there were — pdfdetach says
	// nothing at all about what it saved — and two execs would mean opening the
	// document twice to learn one number.
	script := strings.Join([]string{
		"set -e",
		"mkdir -p " + outputDir,
		// Saved from inside the output directory rather than to a named one:
		// -o names a single output file for -savefile, and its meaning for
		// -saveall is not something poppler documents.
		"cd " + outputDir,
		d.command("pdfdetach", []string{listFlag}, sourcePath),
		d.command("pdfdetach", []string{saveAllFlag}, sourcePath),
	}, "\n")

	res, code, err := d.capture(ctx, script)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		if strings.Contains(res.stderr, traversalMarker) {
			// The list report is the useful half of this failure: it survived, and
			// the names in it are what -savefile has to be given.
			return nil, fmt.Errorf(
				"Attachments: this document names an attachment with a path in it, and poppler refuses the whole extraction rather than that one file: nothing was saved. The names are the document's own, so nothing passed to this module changes that — fetch them one at a time with `pdfdetach -savefile <name> -o <path>` through Container\n%s\n%s",
				strings.TrimSpace(res.stdout), strings.TrimSpace(res.stderr))
		}
		return nil, d.failure("Attachments", code, res.stdout, res.stderr)
	}

	count, err := parseEmbeddedFileCount(res.stdout)
	if err != nil {
		return nil, fmt.Errorf("Attachments: %s", err.Error())
	}
	if count == noEmbeddedFilesCount {
		return nil, fmt.Errorf(
			"Attachments: this document carries no embedded files: nothing is attached to it, so there is nothing to extract. `pdfdetach -list`, through Container, answers the same question without failing")
	}
	return res.container.Directory(outputDir), nil
}

// rangeSuffix names the page range in a message about a document that came back
// empty, so a caller who narrowed to the wrong pages is not told the whole
// document is empty.
func (d *Document) rangeSuffix() string {
	if !d.HasRange {
		return ""
	}
	if d.LastPage == endOfDocument {
		return fmt.Sprintf(" from page %d on", d.FirstPage)
	}
	return fmt.Sprintf(" on pages %d through %d", d.FirstPage, d.LastPage)
}

// parseEmbeddedFileCount reads the count out of pdfdetach's report, which opens
// with a `<n> embedded files` line whatever n is — including zero, and including
// one, poppler not troubling itself about the plural.
func parseEmbeddedFileCount(report string) (int, error) {
	for _, line := range strings.Split(report, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasSuffix(line, embeddedFilesSuffix) {
			continue
		}
		field := strings.TrimSpace(strings.TrimSuffix(line, embeddedFilesSuffix))
		count, err := strconv.Atoi(field)
		if err != nil {
			return 0, fmt.Errorf("pdfdetach reported an unreadable file count %q", field)
		}
		return count, nil
	}
	return 0, fmt.Errorf("pdfdetach reported no file count:\n%s", report)
}
