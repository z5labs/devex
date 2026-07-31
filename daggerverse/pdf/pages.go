package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"dagger/pdf/internal/dagger"
)

const (
	// splitPattern is the destination pdfseparate is handed. It is a printf
	// format and not a path: the tool substitutes the page number for `%d` and
	// refuses a pattern without one, which is how a multi-page document is
	// allowed to write more than one file.
	//
	// The number goes in unpadded and is padded afterwards by normalizeScript.
	// pdfseparate would honour a `%04d` here, but the width would then be fixed
	// rather than a floor, and a document with more than 9999 pages would come
	// out numbered to two different widths.
	splitPattern = outputDir + "/" + pageNamePrefix + "%d.pdf"

	// splitArgv0 is the `$0` the split script runs under, so the extension
	// normalizeScript matches on reaches it as `$1`.
	splitArgv0 = "pdf-split"

	// pdfExt is the extension both halves of this file work in.
	pdfExt = "pdf"

	// mergeInputDir is where Merge mounts its sources and mergedPath the file it
	// writes. The sources are named by their position in the slice, padded like
	// everything else this module names, because poppler quotes the path of a
	// source it could not read and that path is the only thing tying its
	// complaint back to an argument the caller passed.
	mergeInputDir = "/in"
	mergedPath    = outputDir + "/merged.pdf"
)

// normalizeScript renames every page pdfseparate just wrote so its number is
// zero-padded to a fixed width, and is the reason this directory can be handed
// to another module at all.
//
// It is Split's alone. The render family reaches the same contract from the
// other end: it drives the page loop from Go, so it *names* each page outright
// and has nothing to rename — see pageName. Split cannot, pdfseparate writing
// every page of a range in one invocation, and the number it writes being as
// wide as the caller's destination pattern asked for.
//
// Padding to a fixed minimum is what makes the shape a contract instead of a
// consequence. Lexicographic order is otherwise page order within a single
// document and nothing more: a consumer that sorts what it is given — the
// tesseract module's Batch does a bare sort.Strings — has no way to know which
// width it is holding, and a caller who hardcodes one name shape breaks on the
// next document.
//
// The width is the greater of the module's minimum and whatever the tool chose,
// so a document longer than the minimum can express stays uniform rather than
// being truncated into ambiguity.
//
// It runs in the same exec as the split, so the directory Split returns has
// never existed under the other names.
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

// pageGlob matches every page the split wrote, whatever width pdfseparate
// numbered it to. The extension comes from `$1` rather than being interpolated
// into the script text.
const pageGlob = outputDir + `/` + pageNamePrefix + `*."$ext"`

// Split writes each page of the document to its own PDF and returns the
// directory holding them, named `page-0001.pdf`, `page-0002.pdf`, and so on.
//
// This is page extraction and not rendering: each file carries the original
// page's objects — its text layer, its fonts, its annotations — so a split page
// is still a PDF a reader can search, and splitting is lossless in a way that
// rendering to PNG is not.
//
// The names are the same contract the render family honours, and for the same
// reason: a consumer that sorts this directory has to get page order. They are
// this module's and not the tool's — pdfseparate numbers to whatever width the
// destination pattern asks for, so the four digits are a promise rather than a
// consequence of the document's length. The numbers are the source document's
// page numbers, so pages 4 through 6 come out `page-0004.pdf` through
// `page-0006.pdf` and stay traceable to the page they came from.
//
// WithPageRange narrows it. The passwords do not reach it: pdfseparate has no
// password option at all, so an encrypted document is one it cannot open however
// the document was bound — see the message a run against one carries.
func (d *Document) Split(ctx context.Context) (*dagger.Directory, error) {
	if err := d.validateRange(ctx); err != nil {
		return nil, err
	}

	words := append([]string{"pdfseparate"}, d.rangeArgs()...)
	words = append(words, sourcePath, splitPattern)

	// set -e is what makes a document poppler cannot open fail here, carrying
	// its own message, rather than turning into an empty directory a caller
	// would read as a document with no pages in it.
	script := strings.Join([]string{
		"set -e",
		"mkdir -p " + outputDir,
		strings.Join(words, " "),
		normalizeScript,
	}, "\n")

	res, code, err := d.capture(ctx, script, splitArgv0, pdfExt)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, pageToolFailure("Split", "pdfseparate", "the document", code, res.stdout, res.stderr)
	}
	return res.container.Directory(outputDir), nil
}

// Merge concatenates several PDFs into one, in the order they are given, and
// returns the result.
//
// It hangs off the root rather than off Document because it takes several
// sources and no single one of them is "the" document: a bound document carries
// passwords and a page range, and neither has any meaning for a merge.
//
// The sources are an ordered slice rather than a directory and a glob because
// merge order is meaning. A directory has no order of its own — a caller would
// be relying on the file names it happens to carry, which is a contract nobody
// wrote down — so the slice is where the caller states what the result should
// read like.
//
// One source is legal and produces that source's document, so a caller merging a
// computed list does not have to special-case its length. An empty slice is not:
// there is no document to return and no sensible empty PDF to invent, and
// pdfunite handed no sources answers with its own usage text.
//
// Nothing else about the sources is narrowed or transformed. Whole documents go
// in whole — Split is what produces single pages to merge — and the pages keep
// their own size, so merging US Letter with A4 yields a document whose pages
// differ in size, which is what a concatenation should do.
//
// Encrypted sources are the one shape this cannot take: pdfunite has no password
// option, so there is no argument that would let it read one. See the message a
// run against one carries.
func (p *Pdf) Merge(
	ctx context.Context,
	// The PDFs to concatenate, in the order their pages should appear in the
	// result.
	sources []*dagger.File,
) (*dagger.File, error) {
	if len(sources) == 0 {
		return nil, fmt.Errorf(
			"Merge: no sources to merge: pass at least one PDF, in the order the pages should appear in the result")
	}

	ctr := p.Container()
	words := []string{"pdfunite"}
	for i, source := range sources {
		path := mergeSourcePath(i)
		ctr = ctr.WithMountedFile(path, source)
		words = append(words, path)
	}
	words = append(words, mergedPath)

	script := strings.Join([]string{
		"set -e",
		"mkdir -p " + outputDir,
		strings.Join(words, " "),
	}, "\n")

	res, code, err := capture(ctx, ctr, script)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, mergeFailure(code, len(sources), res.stdout, res.stderr)
	}
	return res.container.File(mergedPath), nil
}

// mergeSourcePath is where the i-th source of a merge is mounted, numbered from
// 1 and padded like everything else this module names.
func mergeSourcePath(i int) string {
	return fmt.Sprintf("%s/source-%0*d.pdf", mergeInputDir, minPageNumberWidth, i+1)
}

// mergeFailure builds the error a failed pdfunite run becomes, with a legend for
// the paths in it.
//
// The legend is the whole reason this is not pageToolFailure directly. pdfunite
// names the source it could not read — `Could not merge damaged documents
// ('/in/source-0002.pdf')` — and that path is this module's mount rather than
// anything the caller wrote, so without the legend the one piece of information
// that identifies *which* argument was wrong is unreadable.
func mergeFailure(code, count int, stdout, stderr string) error {
	err := pageToolFailure("Merge", "pdfunite", "a source document", code, stdout, stderr)
	return fmt.Errorf(
		"%s\nany %s/source-NNNN.pdf above is this module's mount for the NNNNth of the %d sources, numbered from 1 in the order they were passed",
		err.Error(), mergeInputDir, count)
}

// pageToolFailure builds the error a failed pdfseparate or pdfunite run becomes.
//
// The encrypted case gets its own message and cannot reuse Document.failure's,
// which names the two builders that supply a password. Neither of poppler's page
// tools takes one — there is no `-upw` on either, and passing one is a usage
// error rather than a wrong password — so an encrypted document is not a
// document these functions can be made to open. Pointing at WithUserPassword
// here would send a caller to a builder that changes nothing.
func pageToolFailure(label, tool, subject string, code int, stdout, stderr string) error {
	out := strings.TrimSpace(strings.TrimSpace(stdout) + "\n" + strings.TrimSpace(stderr))
	if strings.Contains(stderr, incorrectPasswordMarker) {
		return fmt.Errorf(
			"%s: %s is encrypted, and %s has no password option: WithUserPassword and WithOwnerPassword reach the tools that take one, and this is not one of them. Decrypt it first — `qpdf --decrypt` — and %s the result\n%s",
			label, subject, tool, strings.ToLower(label), out)
	}
	return fmt.Errorf("%s failed (exit %d):\n%s", label, code, out)
}
