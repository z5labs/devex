// Package main implements the test module for the tesseract Dagger module.
// Each test is exposed as a standalone dagger function so it can be invoked
// individually during TDD; All wires them up for parallel execution under
// `dagger call all`.
//
// The fixtures under fixtures/ are committed rather than generated in-container:
// bare Alpine ships no fonts, so rendering text inside the toolchain image would
// mean pulling in fontconfig and a font package purely to make the tests run.
package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	par "github.com/dagger/dagger/util/parallel"

	"dagger/tests/internal/dagger"
)

const (
	// sentencePng is a four-line pangram paragraph rendered upright;
	// sentenceRot90Png is the same image rotated a quarter turn clockwise,
	// which is what makes orientation detection have something to report.
	sentencePng      = "sentence.png"
	sentenceRot90Png = "sentence-rot90.png"

	// ompThreadLimitEnv is the OpenMP variable New's ompThreadLimit sets on
	// the assembled image.
	ompThreadLimitEnv = "OMP_THREAD_LIMIT"

	// suiteOmpThreadLimit is the bound every test here builds its image with.
	// The module itself leaves OpenMP alone, so tesseract takes one thread per
	// available CPU — right for a caller who owns the machine, wrong for this
	// suite, which fires ~20 recognitions at once onto a shared 4-vCPU runner.
	// Unbounded, each of them claims all four cores and the suite goes from
	// 22s to over 9m (#226).
	suiteOmpThreadLimit = 1
)

// sentenceLines is what sentence.png renders, and therefore what recognition
// has to reproduce. Assertions match line by line so a single mis-recognised
// word names itself instead of dumping the whole paragraph.
var sentenceLines = []string{
	"The quick brown fox jumps over the lazy dog.",
	"Pack my box with five dozen liquor jugs today.",
	"How vexingly quick daft zebras jump about now.",
	"Sphinx of black quartz, judge my vow tonight.",
}

type Tests struct{}

// All runs every tesseract-module test in parallel.
//
// parallel caps how many tests run concurrently inside this suite. Defaults to
// 0 (unbounded fan-out), which used to be justified with "in-runner
// parallelism is bounded by the VM's CPU/memory, not by the scheduler". That
// was false: an unbounded tesseract sizes its OpenMP teams by CPU count, so a
// four-core runner bounded nothing — it multiplied. Twenty concurrent jobs
// each fanning out to four threads is eighty threads over four cores, and the
// suite took 9m5s on a runner it takes 22s to finish on locally (#226).
//
// What makes the fan-out safe is suiteOmpThreadLimit, not this cap: with one
// thread per pass the claim finally holds, and jobs contend for cores the way
// any other oversubscribed workload does. The cap stays available for a host
// that wants a narrower slice.
//
// +check
// +cache="session"
func (t *Tests) All(
	ctx context.Context,
	// +default=0
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}

	jobs = jobs.WithJob("VersionReportsTesseractFive", t.VersionReportsTesseractFive)
	jobs = jobs.WithJob("DefaultLanguagesInstallEnglish", t.DefaultLanguagesInstallEnglish)
	jobs = jobs.WithJob("RequestedLanguagesAreInstalled", t.RequestedLanguagesAreInstalled)
	jobs = jobs.WithJob("OmpThreadLimitBoundsOpenMp", t.OmpThreadLimitBoundsOpenMp)

	jobs = jobs.WithJob("TextRecognizesFixture", t.TextRecognizesFixture)
	jobs = jobs.WithJob("TxtFileMatchesText", t.TxtFileMatchesText)
	jobs = jobs.WithJob("HocrContainsWordBoxes", t.HocrContainsWordBoxes)
	jobs = jobs.WithJob("AltoIsValidXml", t.AltoIsValidXml)
	jobs = jobs.WithJob("TsvHasHeaderAndWordRows", t.TsvHasHeaderAndWordRows)
	jobs = jobs.WithJob("PdfHasPdfMagic", t.PdfHasPdfMagic)
	jobs = jobs.WithJob("ExportProducesEveryRequestedFormat", t.ExportProducesEveryRequestedFormat)

	jobs = jobs.WithJob("SingleWordPageSegReturnsFewerWords", t.SingleWordPageSegReturnsFewerWords)
	jobs = jobs.WithJob("LstmEngineRecognizesFixture", t.LstmEngineRecognizesFixture)
	jobs = jobs.WithJob("UnknownParameterFails", t.UnknownParameterFails)
	jobs = jobs.WithJob("UserWordsFileIsAccepted", t.UserWordsFileIsAccepted)
	jobs = jobs.WithJob("OsdDetectsRotation", t.OsdDetectsRotation)

	jobs = jobs.WithJob("UnknownLanguageIsRejected", t.UnknownLanguageIsRejected)
	jobs = jobs.WithJob("OsdWithoutOsdDataIsRejected", t.OsdWithoutOsdDataIsRejected)
	jobs = jobs.WithJob("PdfInputIsRejected", t.PdfInputIsRejected)
	jobs = jobs.WithJob("MalformedParameterNameIsRejected", t.MalformedParameterNameIsRejected)
	jobs = jobs.WithJob("NonPositiveDpiIsRejected", t.NonPositiveDpiIsRejected)

	return jobs.Run(ctx)
}

// ---------------------------------------------------------------- toolchain

// VersionReportsTesseractFive asserts the assembled image ships the tesseract
// release Alpine's community repository carries, so a base-tag bump that
// silently changes major version fails here rather than in recognition.
func (t *Tests) VersionReportsTesseractFive(ctx context.Context) error {
	got, err := ocr().Version(ctx)
	if err != nil {
		return fmt.Errorf("Version: %w", err)
	}
	if !strings.HasPrefix(got, "5.") {
		return fmt.Errorf("expected a 5.x tesseract version, got %q", got)
	}
	return nil
}

// DefaultLanguagesInstallEnglish asserts New with no languages installs
// English and nothing else. The base apk package carries no language data at
// all, so an empty default would produce an image that cannot recognise
// anything.
func (t *Tests) DefaultLanguagesInstallEnglish(ctx context.Context) error {
	got, err := ocr().Langs(ctx)
	if err != nil {
		return fmt.Errorf("Langs: %w", err)
	}
	if len(got) != 1 || got[0] != "eng" {
		return fmt.Errorf("expected exactly [eng] installed by default, got %v", got)
	}
	return nil
}

// RequestedLanguagesAreInstalled asserts every requested language lands in the
// image as its own apk package, including "osd", which is a detection model
// rather than a recognition language.
func (t *Tests) RequestedLanguagesAreInstalled(ctx context.Context) error {
	got, err := ocr("eng", "deu", "osd").Langs(ctx)
	if err != nil {
		return fmt.Errorf("Langs: %w", err)
	}
	for _, want := range []string{"deu", "eng", "osd"} {
		if !contains(got, want) {
			return fmt.Errorf("expected %q to be installed, got %v", want, got)
		}
	}
	return nil
}

// OmpThreadLimitBoundsOpenMp asserts the OpenMP bound is absent by default and
// otherwise reaches the environment tesseract runs in.
//
// The default is as much the point as the override. Alpine's tesseract links
// libgomp and reports `Found OpenMP 201511`, so unbounded it takes one thread
// per available CPU — right for a caller who owns the machine, and the reason
// the bound is opt-in rather than baked in. What this pins is that opting in
// works at all: without it, anything running several recognitions at once has
// no way to stop each pass claiming every core, which cost this very suite
// nine minutes on a four-core runner (#226).
func (t *Tests) OmpThreadLimitBoundsOpenMp(ctx context.Context) error {
	got, err := readOmpThreadLimit(ctx, dag.Tesseract())
	if err != nil {
		return err
	}
	if got != "" {
		return fmt.Errorf("expected %s to be unset by default, got %q", ompThreadLimitEnv, got)
	}

	got, err = readOmpThreadLimit(ctx, dag.Tesseract(dagger.TesseractOpts{OmpThreadLimit: 3}))
	if err != nil {
		return err
	}
	if got != "3" {
		return fmt.Errorf("expected %s=3 in the image, got %q", ompThreadLimitEnv, got)
	}

	// A negative cap is refused on New rather than handed to libgomp, which
	// reads an unparseable OMP_THREAD_LIMIT as absent and silently goes back
	// to one thread per CPU — the exact opposite of what was asked for.
	_, err = dag.Tesseract(dagger.TesseractOpts{OmpThreadLimit: -1}).Langs(ctx)
	if err == nil {
		return fmt.Errorf("expected a negative ompThreadLimit to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "must not be negative") {
		return fmt.Errorf("expected the error to name the negative rule, got: %v", err)
	}
	return nil
}

// ---------------------------------------------------------------- recognition

// TextRecognizesFixture asserts the shortest path — image in, string out —
// reproduces every line the fixture renders.
func (t *Tests) TextRecognizesFixture(ctx context.Context) error {
	got, err := ocr().Document(fixture(sentencePng)).Text(ctx)
	if err != nil {
		return fmt.Errorf("Text: %w", err)
	}
	return assertSentenceLines(got)
}

// TxtFileMatchesText asserts the txt renderer and the stdout path agree, so
// choosing a file over a string is purely a plumbing decision.
func (t *Tests) TxtFileMatchesText(ctx context.Context) error {
	doc := ocr().Document(fixture(sentencePng))

	text, err := doc.Text(ctx)
	if err != nil {
		return fmt.Errorf("Text: %w", err)
	}
	contents, err := doc.Txt().Contents(ctx)
	if err != nil {
		return fmt.Errorf("Txt: %w", err)
	}
	if strings.TrimSpace(contents) != strings.TrimSpace(text) {
		return fmt.Errorf("expected Txt to match Text\nText:\n%s\nTxt:\n%s", text, contents)
	}
	return assertSentenceLines(contents)
}

// HocrContainsWordBoxes asserts hOCR carries the per-word geometry that is the
// whole reason to ask for it rather than plain text.
func (t *Tests) HocrContainsWordBoxes(ctx context.Context) error {
	got, err := ocr().Document(fixture(sentencePng)).Hocr().Contents(ctx)
	if err != nil {
		return fmt.Errorf("Hocr: %w", err)
	}
	for _, want := range []string{"ocr_page", "class='ocrx_word'", "bbox ", "x_wconf "} {
		if !strings.Contains(got, want) {
			return fmt.Errorf("expected hOCR to contain %q, got:\n%s", want, got)
		}
	}
	// hOCR wraps each word in its own span, so the assertion is per word
	// rather than per line.
	for _, want := range []string{">The<", ">quick<", ">brown<", ">fox<"} {
		if !strings.Contains(got, want) {
			return fmt.Errorf("expected hOCR to carry the recognised word %q, got:\n%s", want, got)
		}
	}
	return nil
}

// AltoIsValidXml asserts the ALTO renderer emits well-formed XML in the ALTO
// namespace, since its consumers are schema-driven archive tooling that will
// reject anything else outright.
func (t *Tests) AltoIsValidXml(ctx context.Context) error {
	got, err := ocr().Document(fixture(sentencePng)).Alto().Contents(ctx)
	if err != nil {
		return fmt.Errorf("Alto: %w", err)
	}
	if err := xml.Unmarshal([]byte(got), new(struct{})); err != nil {
		return fmt.Errorf("expected well-formed ALTO XML: %w\ngot:\n%s", err, got)
	}
	for _, want := range []string{"http://www.loc.gov/standards/alto/ns-v3#", "<String ", "CONTENT=\"quick\""} {
		if !strings.Contains(got, want) {
			return fmt.Errorf("expected ALTO to contain %q, got:\n%s", want, got)
		}
	}
	return nil
}

// TsvHasHeaderAndWordRows asserts the TSV renderer emits its column header and
// descends all the way to word-level rows (level 5), which is the level
// carrying the text and its confidence.
func (t *Tests) TsvHasHeaderAndWordRows(ctx context.Context) error {
	got, err := ocr().Document(fixture(sentencePng)).Tsv().Contents(ctx)
	if err != nil {
		return fmt.Errorf("Tsv: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "level\tpage_num\t") {
		return fmt.Errorf("expected a TSV header row, got:\n%s", got)
	}
	if !strings.HasSuffix(lines[0], "\tconf\ttext") {
		return fmt.Errorf("expected the header to end in conf/text columns, got %q", lines[0])
	}

	var words []string
	for _, line := range lines[1:] {
		cols := strings.Split(line, "\t")
		if len(cols) != len(strings.Split(lines[0], "\t")) {
			return fmt.Errorf("expected %d columns, got %d in %q", len(strings.Split(lines[0], "\t")), len(cols), line)
		}
		if cols[0] == "5" && cols[len(cols)-1] != "" {
			words = append(words, cols[len(cols)-1])
		}
	}
	for _, want := range []string{"The", "quick", "brown", "fox"} {
		if !contains(words, want) {
			return fmt.Errorf("expected a word row for %q, got %v", want, words)
		}
	}
	return nil
}

// PdfHasPdfMagic asserts the searchable-PDF renderer emits a real PDF. The
// bytes go through the filesystem rather than File.Contents, which mangles
// non-UTF-8 data.
func (t *Tests) PdfHasPdfMagic(ctx context.Context) error {
	got, err := exportBytes(ctx, ocr().Document(fixture(sentencePng)).Pdf(), "result.pdf")
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(got, []byte("%PDF-")) {
		return fmt.Errorf("expected a PDF header, got %q", firstBytes(got, 8))
	}
	if !bytes.Contains(got, []byte("%%EOF")) {
		return fmt.Errorf("expected a complete PDF ending in %%%%EOF, got %d bytes", len(got))
	}
	return nil
}

// ExportProducesEveryRequestedFormat asserts one Export call renders all six
// formats. That the artifacts arrive in a single directory lifted off a single
// exec is what proves they came from one recognition pass rather than six: the
// per-format functions each run their own.
func (t *Tests) ExportProducesEveryRequestedFormat(ctx context.Context) error {
	out := ocr().Document(fixture(sentencePng)).Export([]dagger.TesseractFormat{
		dagger.TesseractFormatTxt,
		dagger.TesseractFormatHocr,
		dagger.TesseractFormatAlto,
		dagger.TesseractFormatTsv,
		dagger.TesseractFormatPdf,
		dagger.TesseractFormatPage,
	})
	entries, err := out.Entries(ctx)
	if err != nil {
		return fmt.Errorf("Export: %w", err)
	}
	want := []string{"result.hocr", "result.page.xml", "result.pdf", "result.tsv", "result.txt", "result.xml"}
	sort.Strings(entries)
	if !reflect.DeepEqual(entries, want) {
		return fmt.Errorf("expected exactly %v, got %v", want, entries)
	}

	// A subset request renders only that subset, so the trailing configfile
	// list really is what drives the renderers.
	entries, err = ocr().Document(fixture(sentencePng)).
		Export([]dagger.TesseractFormat{dagger.TesseractFormatTxt, dagger.TesseractFormatTsv}).
		Entries(ctx)
	if err != nil {
		return fmt.Errorf("Export subset: %w", err)
	}
	sort.Strings(entries)
	if want := []string{"result.tsv", "result.txt"}; !reflect.DeepEqual(entries, want) {
		return fmt.Errorf("expected exactly %v from a two-format Export, got %v", want, entries)
	}
	return nil
}

// ------------------------------------------------------------- recognition options

// SingleWordPageSegReturnsFewerWords asserts --psm actually reaches tesseract.
// Telling it a four-line paragraph is one word suppresses the layout analysis
// that finds the lines, so it returns far less than the default mode does —
// which is the observable proof the flag was passed, without asserting on
// whatever garbage the constrained mode happens to produce.
func (t *Tests) SingleWordPageSegReturnsFewerWords(ctx context.Context) error {
	doc := ocr().Document(fixture(sentencePng))

	full, err := doc.Text(ctx)
	if err != nil {
		return fmt.Errorf("Text: %w", err)
	}
	single, err := doc.
		WithPageSegmentation(dagger.TesseractPageSegModeSingleWord).
		Text(ctx)
	if err != nil {
		return fmt.Errorf("Text with SINGLE_WORD: %w", err)
	}
	if got, want := len(strings.Fields(single)), len(strings.Fields(full)); got >= want {
		return fmt.Errorf("expected SINGLE_WORD to return fewer than %d words, got %d:\n%s", want, got, single)
	}
	return nil
}

// LstmEngineRecognizesFixture asserts --oem reaches tesseract and that every
// EngineMode the enum offers actually recognises text.
//
// It covers all four modes rather than LSTM alone because the enum's promise
// is that no member is dead: LEGACY and LEGACY_LSTM only work because Alpine
// packages the *combined* tessdata models. A rebuild against tessdata_fast or
// tessdata_best would strip the legacy data and this is where that shows up.
func (t *Tests) LstmEngineRecognizesFixture(ctx context.Context) error {
	modes := []dagger.TesseractEngineMode{
		dagger.TesseractEngineModeLstm,
		dagger.TesseractEngineModeLegacy,
		dagger.TesseractEngineModeLegacyLstm,
		dagger.TesseractEngineModeDefault,
	}
	for _, mode := range modes {
		got, err := ocr().Document(fixture(sentencePng)).WithEngine(mode).Text(ctx)
		if err != nil {
			return fmt.Errorf("Text with engine %s: %w", mode, err)
		}
		if err := assertSentenceLines(got); err != nil {
			return fmt.Errorf("engine %s: %w", mode, err)
		}
	}
	return nil
}

// UnknownParameterFails asserts a control variable tesseract does not have is
// an error rather than a silent no-op.
//
// tesseract itself only prints `Warning: The parameter '...' was not found.`
// and exits 0, so a typo would otherwise be indistinguishable from a setting
// that simply had no effect. The same test pins the other half: a real
// parameter still goes through, so the check is not just rejecting everything.
func (t *Tests) UnknownParameterFails(ctx context.Context) error {
	_, err := ocr().Document(fixture(sentencePng)).
		WithParameter("not_a_real_parameter", "1").
		Text(ctx)
	if err == nil {
		return fmt.Errorf("expected an unknown parameter to be rejected, got nil")
	}
	for _, want := range []string{"not_a_real_parameter", "Parameters()"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to mention %q, got: %v", want, err)
		}
	}

	// tessedit_char_whitelist restricts recognition to the listed characters,
	// which is both a real parameter and one whose effect is visible.
	got, err := ocr().Document(fixture(sentencePng)).
		WithParameter("tessedit_char_whitelist", "0123456789").
		Text(ctx)
	if err != nil {
		return fmt.Errorf("Text with a known parameter: %w", err)
	}
	if strings.Contains(got, "quick") {
		return fmt.Errorf("expected a digits-only whitelist to suppress words, got:\n%s", got)
	}
	return nil
}

// UserWordsFileIsAccepted asserts a caller-supplied word list and pattern list
// are mounted where tesseract looks for them and recognition still succeeds.
//
// The assertion is deliberately "still recognises", not "recognises
// differently": tesseract reports a missing list on stderr and exits 0
// regardless, so there is no failure signal to test against, and the effect of
// a dictionary hint on an already-clean fixture is not reliably observable.
// What this does catch is a wrong mount path or a flag emitted in the wrong
// position, either of which turns the whole run into a usage error.
func (t *Tests) UserWordsFileIsAccepted(ctx context.Context) error {
	words := textFile("user-words.txt", "vexingly\nSphinx\nquartz\n")
	patterns := textFile("user-patterns.txt", `\d\d\d-\d\d\d\d`+"\n")

	got, err := ocr().Document(fixture(sentencePng)).
		WithUserWords(words).
		WithUserPatterns(patterns).
		Text(ctx)
	if err != nil {
		return fmt.Errorf("Text with user words and patterns: %w", err)
	}
	return assertSentenceLines(got)
}

// OsdDetectsRotation asserts orientation detection reads the quarter-turn in
// the rotated fixture and reports the rotation that would undo it, while the
// upright fixture reports no rotation at all.
func (t *Tests) OsdDetectsRotation(ctx context.Context) error {
	withOsd := ocr("eng", "osd")

	rotated, err := withOsd.Document(fixture(sentenceRot90Png)).Osd(ctx)
	if err != nil {
		return fmt.Errorf("Osd on the rotated fixture: %w", err)
	}
	for _, want := range []string{"Orientation in degrees: 90", "Rotate: 270", "Script: Latin"} {
		if !strings.Contains(rotated, want) {
			return fmt.Errorf("expected the OSD report to contain %q, got:\n%s", want, rotated)
		}
	}

	upright, err := withOsd.Document(fixture(sentencePng)).Osd(ctx)
	if err != nil {
		return fmt.Errorf("Osd on the upright fixture: %w", err)
	}
	if !strings.Contains(upright, "Orientation in degrees: 0") {
		return fmt.Errorf("expected the upright fixture to report no rotation, got:\n%s", upright)
	}
	return nil
}

// ----------------------------------------------------------------- validation

// UnknownLanguageIsRejected asserts a language the image does not carry is
// rejected with the installed set named. tesseract's own failure talks about
// traineddata paths and TESSDATA_PREFIX, which says nothing about the fact
// that languages are chosen on New.
func (t *Tests) UnknownLanguageIsRejected(ctx context.Context) error {
	_, err := ocr().Document(fixture(sentencePng)).
		WithLanguage("deu").
		Text(ctx)
	if err == nil {
		return fmt.Errorf("expected an uninstalled language to be rejected, got nil")
	}
	for _, want := range []string{`"deu"`, "eng", "New(languages:)"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to mention %q, got: %v", want, err)
		}
	}

	// A `+`-joined selection is validated code by code, so one bad member in
	// an otherwise-installed list is caught too.
	_, err = ocr("eng", "deu").
		Document(fixture(sentencePng)).
		WithLanguage("eng+deu+fra").
		Text(ctx)
	if err == nil {
		return fmt.Errorf("expected a combined selection with one bad member to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), `"fra"`) {
		return fmt.Errorf("expected the error to name the bad member fra, got: %v", err)
	}
	return nil
}

// OsdWithoutOsdDataIsRejected asserts orientation detection on an image built
// without the osd model names the fix rather than failing inside tesseract,
// which would report a missing traineddata file.
func (t *Tests) OsdWithoutOsdDataIsRejected(ctx context.Context) error {
	_, err := ocr().Document(fixture(sentenceRot90Png)).Osd(ctx)
	if err == nil {
		return fmt.Errorf("expected Osd without the osd model to be rejected, got nil")
	}
	for _, want := range []string{`"osd"`, "New(languages:)"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to mention %q, got: %v", want, err)
		}
	}
	return nil
}

// PdfInputIsRejected asserts a PDF source is refused up front. Leptonica has
// no PDF support and reports the failure as if the file's first line were a
// file name it could not open, which sends people looking in the wrong place.
func (t *Tests) PdfInputIsRejected(ctx context.Context) error {
	_, err := ocr().Document(textFile("scan.pdf", "%PDF-1.7\n")).Text(ctx)
	if err == nil {
		return fmt.Errorf("expected a PDF source to be rejected, got nil")
	}
	for _, want := range []string{"scan.pdf", "leptonica", "rasterize"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to mention %q, got: %v", want, err)
		}
	}
	return nil
}

// MalformedParameterNameIsRejected asserts an empty parameter name, and one
// carrying its own `=`, are refused. `-c` takes `name=value`, so an embedded
// `=` would quietly set a different variable to a different value.
func (t *Tests) MalformedParameterNameIsRejected(ctx context.Context) error {
	_, err := ocr().Document(fixture(sentencePng)).
		WithParameter("tessedit_char_whitelist=0123", "9").
		Text(ctx)
	if err == nil {
		return fmt.Errorf("expected a parameter name containing = to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), `must not contain "="`) {
		return fmt.Errorf("expected the error to name the = rule, got: %v", err)
	}

	_, err = ocr().Document(fixture(sentencePng)).
		WithParameter("  ", "9").
		Text(ctx)
	if err == nil {
		return fmt.Errorf("expected an empty parameter name to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "parameter name is required") {
		return fmt.Errorf("expected the error to say the name is required, got: %v", err)
	}
	return nil
}

// NonPositiveDpiIsRejected asserts a zero or negative resolution is refused
// rather than handed to tesseract, which would take it as a real measurement
// and scale its analysis by it.
func (t *Tests) NonPositiveDpiIsRejected(ctx context.Context) error {
	for _, dpi := range []int{0, -300} {
		_, err := ocr().Document(fixture(sentencePng)).WithDpi(dpi).Text(ctx)
		if err == nil {
			return fmt.Errorf("expected dpi %d to be rejected, got nil", dpi)
		}
		if !strings.Contains(err.Error(), "dpi must be positive") {
			return fmt.Errorf("expected the error to name the positive rule for dpi %d, got: %v", dpi, err)
		}
	}

	// A positive value still goes through, so the check is not just
	// rejecting every use of the flag.
	got, err := ocr().Document(fixture(sentencePng)).WithDpi(300).Text(ctx)
	if err != nil {
		return fmt.Errorf("Text with dpi 300: %w", err)
	}
	return assertSentenceLines(got)
}

// ------------------------------------------------------------------ helpers

// ocr builds the module under test with the suite's OpenMP bound and the given
// language set; no languages leaves New's own default in place.
//
// Every test that recognises anything goes through this rather than
// dag.Tesseract() directly, so the bound cannot be forgotten on a newly added
// test. The failure mode it guards against is a slow suite, not a red one,
// which is exactly the kind that goes unnoticed for a release.
// OmpThreadLimitBoundsOpenMp is the deliberate exception: it is the test of
// what the unbounded default does.
func ocr(languages ...string) *dagger.Tesseract {
	return dag.Tesseract(dagger.TesseractOpts{
		Languages:      languages,
		OmpThreadLimit: suiteOmpThreadLimit,
	})
}

// readOmpThreadLimit reports the OpenMP bound as the environment inside the
// image sees it rather than as container metadata, because what decides
// recognition's fan-out is what the tesseract process inherits.
func readOmpThreadLimit(ctx context.Context, t *dagger.Tesseract) (string, error) {
	out, err := t.Container().
		WithExec([]string{"sh", "-c", `printf %s "$` + ompThreadLimitEnv + `"`}).
		Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("read %s from the image: %w", ompThreadLimitEnv, err)
	}
	return out, nil
}

// fixture returns a committed test image by name under fixtures/.
func fixture(name string) *dagger.File {
	return dag.CurrentModule().Source().File("fixtures/" + name)
}

// textFile builds an in-memory file for the caller-supplied word and pattern
// lists, which are small enough not to warrant committed fixtures.
func textFile(name string, contents string) *dagger.File {
	return dag.Directory().WithNewFile(name, contents).File(name)
}

// exportBytes exports a file artifact and returns its raw bytes, for the
// binary formats File.Contents would mangle.
func exportBytes(ctx context.Context, f *dagger.File, name string) ([]byte, error) {
	dir, err := os.MkdirTemp(".", "tesseract-")
	if err != nil {
		return nil, fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, name)
	if _, err := f.Export(ctx, path); err != nil {
		return nil, fmt.Errorf("export %s: %w", name, err)
	}
	return os.ReadFile(path)
}

func firstBytes(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[:n]
}

// assertSentenceLines checks recognised text against the fixture's rendered
// lines, in order.
func assertSentenceLines(got string) error {
	for _, want := range sentenceLines {
		if !strings.Contains(got, want) {
			return fmt.Errorf("expected recognised text to contain %q, got:\n%s", want, got)
		}
	}
	return nil
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
