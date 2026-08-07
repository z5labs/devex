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
	"regexp"
	"sort"
	"strconv"
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

	// ledgerPdf is a three-page PDF carrying different text on every page. It
	// is hand-authored rather than produced by a generator: the content
	// streams are uncompressed, so the text a page is supposed to render is
	// readable in the fixture itself, and the whole file is 1.4KB.
	//
	// This module cannot read it — that is the point — so it is the fixture for
	// exactly one test, the cross-module flow that renders it with the pdf
	// module before recognising the pages here.
	ledgerPdf = "ledger.pdf"

	// ledgerDpi is the resolution the ledger's pages are rendered at before
	// recognition. 300 is what tesseract's own quality documentation asks for;
	// the pdf module's default is 150, a screen-reading resolution, so an OCR
	// render always names it.
	ledgerDpi = 300

	// ompThreadLimitEnv is the OpenMP variable New's ompThreadLimit sets on
	// the assembled image.
	ompThreadLimitEnv = "OMP_THREAD_LIMIT"

	// packagedTessdataDir is where the apk packages drop their models. It is
	// the source the tessdata tests round-trip a model out of, never a path
	// the module itself asks anyone to know about.
	packagedTessdataDir = "/usr/share/tessdata"

	// floatModelURL is the English model from tesseract-ocr/tessdata_best,
	// which is the float build of the same language Alpine packages
	// integerized. It is the only kind of model lstmtraining will fine-tune
	// from, so it is what the training tests start from. The URL names a
	// release tag rather than a branch: the base model a fine-tune starts from
	// decides what it produces, and that should not change because upstream
	// pushed to main.
	floatModelURL = "https://github.com/tesseract-ocr/tessdata_best/raw/4.1.0/eng.traineddata"

	// floatModelLang is the name that model is mounted and fine-tuned under.
	// It is deliberately not "eng": a distinct name is what makes the tests
	// able to tell the float model from the packaged one they sit beside.
	floatModelLang = "best"

	// blankPageWidth and blankPageHeight size the generated blank page. They
	// are a page rather than a swatch because tesseract declines to analyse a
	// layout it considers too small to have one, and a refusal to look is not
	// the same result as looking and finding nothing.
	blankPageWidth  = 300
	blankPageHeight = 200

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

// ledgerPages is what ledger.pdf renders, one entry per page in document
// order. Every page leads with its own ordinal, which is what makes a dropped
// or reordered page visible: without it, three pages of the same text would
// recognise identically however they were shuffled.
var ledgerPages = [][]string{
	{"Page one of the ledger.", "The quick brown fox jumps over the lazy dog."},
	{"Page two of the ledger.", "Pack my box with five dozen liquor jugs today."},
	{"Page three of the ledger.", "How vexingly quick daft zebras jump about now."},
}

// ledgerPageMarkers is one word per page of ledger.pdf, in page order.
//
// The structured renderers wrap every word in its own element, so no whole
// line is ever contiguous text in their output and the line-level assertions
// cannot be used on them. A single word always is contiguous, and these three
// each occur exactly once in the document, so their relative positions say
// what page order says.
var ledgerPageMarkers = []string{"lazy", "liquor", "zebras"}

// confidencePattern matches the measured percentage in a Ci gate error. See
// measuredConfidence.
var confidencePattern = regexp.MustCompile(`([0-9]+\.[0-9]+)%`)

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

	jobs = jobs.WithJob("PdfModulePagesBatchIntoOneSearchablePdf", t.PdfModulePagesBatchIntoOneSearchablePdf)

	jobs = jobs.WithJob("BatchMirrorsInputLayout", t.BatchMirrorsInputLayout)
	jobs = jobs.WithJob("BatchDefaultGlobSkipsNonImages", t.BatchDefaultGlobSkipsNonImages)
	jobs = jobs.WithJob("BatchGlobSelectsFiles", t.BatchGlobSelectsFiles)
	jobs = jobs.WithJob("BatchExportProducesEveryFormatPerImage", t.BatchExportProducesEveryFormatPerImage)
	jobs = jobs.WithJob("BatchSharesDocumentOptions", t.BatchSharesDocumentOptions)
	jobs = jobs.WithJob("BatchRejectsAmbiguousInput", t.BatchRejectsAmbiguousInput)
	jobs = jobs.WithJob("BatchConcurrencyMatchesSerialOutput", t.BatchConcurrencyMatchesSerialOutput)
	jobs = jobs.WithJob("BatchConcurrencyReportsFailingImage", t.BatchConcurrencyReportsFailingImage)

	jobs = jobs.WithJob("CiRunProducesEnabledFormats", t.CiRunProducesEnabledFormats)
	jobs = jobs.WithJob("CiMinConfidenceGatesTheRun", t.CiMinConfidenceGatesTheRun)
	jobs = jobs.WithJob("CiCheckRunsTheGateWithoutArtifacts", t.CiCheckRunsTheGateWithoutArtifacts)
	jobs = jobs.WithJob("CiGateKeepsItsTsvOutOfTheOutput", t.CiGateKeepsItsTsvOutOfTheOutput)
	jobs = jobs.WithJob("CiLanguageReachesRecognition", t.CiLanguageReachesRecognition)
	jobs = jobs.WithJob("CiRejectsUnusableThreshold", t.CiRejectsUnusableThreshold)

	jobs = jobs.WithJob("ApkRepositoryInstallsFromPrivateMirror", t.ApkRepositoryInstallsFromPrivateMirror)
	jobs = jobs.WithJob("ApkRepositoryReplacesImageDefaults", t.ApkRepositoryReplacesImageDefaults)
	jobs = jobs.WithJob("UntrustedIndexIsRejectedWithoutApkKey", t.UntrustedIndexIsRejectedWithoutApkKey)
	jobs = jobs.WithJob("ApkAuthInstallsFromAuthenticatedRepository", t.ApkAuthInstallsFromAuthenticatedRepository)
	jobs = jobs.WithJob("AuthenticatedRepositoryIsRejectedWithoutApkAuth", t.AuthenticatedRepositoryIsRejectedWithoutApkAuth)
	jobs = jobs.WithJob("DefaultApkConfigurationIsUntouched", t.DefaultApkConfigurationIsUntouched)

	jobs = jobs.WithJob("TessdataModelIsSelectable", t.TessdataModelIsSelectable)
	jobs = jobs.WithJob("TessdataSuppliesOsdModel", t.TessdataSuppliesOsdModel)

	jobs = jobs.WithJob("BoxReportsCharacterBoxes", t.BoxReportsCharacterBoxes)
	jobs = jobs.WithJob("ProcessedImagesReturnsThresholdedTiff", t.ProcessedImagesReturnsThresholdedTiff)
	jobs = jobs.WithJob("LstmTrainBuildsTrainingSample", t.LstmTrainBuildsTrainingSample)

	jobs = jobs.WithJob("TrainingPairsImagesWithGroundTruth", t.TrainingPairsImagesWithGroundTruth)
	jobs = jobs.WithJob("TrainingRejectsUnusableInput", t.TrainingRejectsUnusableInput)
	jobs = jobs.WithJob("TrainingRequiresFloatBaseModel", t.TrainingRequiresFloatBaseModel)
	jobs = jobs.WithJob("TrainingProducesUsableModel", t.TrainingProducesUsableModel)

	jobs = jobs.WithJob("SingleWordPageSegReturnsFewerWords", t.SingleWordPageSegReturnsFewerWords)
	jobs = jobs.WithJob("LstmEngineRecognizesFixture", t.LstmEngineRecognizesFixture)
	jobs = jobs.WithJob("UnknownParameterFails", t.UnknownParameterFails)
	jobs = jobs.WithJob("UserWordsFileIsAccepted", t.UserWordsFileIsAccepted)
	jobs = jobs.WithJob("OsdDetectsRotation", t.OsdDetectsRotation)

	jobs = jobs.WithJob("UnknownLanguageIsRejected", t.UnknownLanguageIsRejected)
	jobs = jobs.WithJob("TessdataDoesNotAdmitUnknownLanguage", t.TessdataDoesNotAdmitUnknownLanguage)
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

// ------------------------------------------------------------------- tessdata

// TessdataModelIsSelectable asserts a caller-supplied model is a first-class
// language: Langs lists it alongside the packaged set, WithLanguage accepts it,
// and recognition under that name reproduces the fixture.
//
// The model is the image's own eng.traineddata lifted back out and re-mounted
// under a different stem. That needs no committed binary fixture and still
// proves the whole path: "custom" is a name no Alpine package could ever
// install, so recognising English text under it can only mean the caller's
// directory reached tesseract.
//
// The packaged language and the PDF render are asserted through the same
// module because the caller's directory has to be merged with the packaged one
// rather than swapped for it. `--tessdata-dir` moves the whole datadir, and
// that directory carries more than models: `configs/` holds the renderer
// configfiles, and pdf.ttf is what the PDF renderer draws its invisible text
// layer with. Pointed at the caller's directory alone, every renderer breaks
// and every packaged language disappears.
func (t *Tests) TessdataModelIsSelectable(ctx context.Context) error {
	custom := ocr().WithTessdata(
		dag.Directory().WithFile("custom.traineddata", traineddata(ocr(), "eng")),
	)

	langs, err := custom.Langs(ctx)
	if err != nil {
		return fmt.Errorf("Langs: %w", err)
	}
	if want := []string{"custom", "eng"}; !reflect.DeepEqual(langs, want) {
		return fmt.Errorf("expected Langs to report the union %v, got %v", want, langs)
	}

	got, err := custom.Document(fixture(sentencePng)).WithLanguage("custom").Text(ctx)
	if err != nil {
		return fmt.Errorf("Text under the caller-supplied model: %w", err)
	}
	if err := assertSentenceLines(got); err != nil {
		return fmt.Errorf("caller-supplied model: %w", err)
	}

	got, err = custom.Document(fixture(sentencePng)).WithLanguage("eng").Text(ctx)
	if err != nil {
		return fmt.Errorf("Text under the packaged model with tessdata mounted: %w", err)
	}
	if err := assertSentenceLines(got); err != nil {
		return fmt.Errorf("packaged model with tessdata mounted: %w", err)
	}

	pdf, err := exportBytes(ctx, custom.Document(fixture(sentencePng)).WithLanguage("custom").Pdf(), "result.pdf")
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		return fmt.Errorf("expected the PDF renderer to still work with tessdata mounted, got %q", firstBytes(pdf, 8))
	}
	return nil
}

// TessdataSuppliesOsdModel asserts orientation detection accepts an osd model
// that arrived through WithTessdata rather than through its apk package.
//
// Osd is the one place that asks whether a specific model is present, and it
// answered from the requested package set alone. A supplied osd.traineddata
// would have been refused by this module while sitting right there in the
// image, which is the failure mode this pins.
func (t *Tests) TessdataSuppliesOsdModel(ctx context.Context) error {
	supplied := ocr().WithTessdata(
		dag.Directory().WithFile("osd.traineddata", traineddata(ocr("eng", "osd"), "osd")),
	)

	got, err := supplied.Document(fixture(sentenceRot90Png)).Osd(ctx)
	if err != nil {
		return fmt.Errorf("Osd with a supplied osd model: %w", err)
	}
	if !strings.Contains(got, "Orientation in degrees: 90") {
		return fmt.Errorf("expected the OSD report to read the quarter turn, got:\n%s", got)
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

// TessdataDoesNotAdmitUnknownLanguage asserts the union is still a closed set:
// mounting a tessdata directory adds the models it holds and nothing else, so a
// language neither half carries is rejected the same way it was before, with
// both halves listed and both ways of adding one named.
func (t *Tests) TessdataDoesNotAdmitUnknownLanguage(ctx context.Context) error {
	custom := ocr().WithTessdata(
		dag.Directory().WithFile("custom.traineddata", traineddata(ocr(), "eng")),
	)

	_, err := custom.Document(fixture(sentencePng)).WithLanguage("fra").Text(ctx)
	if err == nil {
		return fmt.Errorf("expected a language in neither the package set nor the tessdata directory to be rejected, got nil")
	}
	for _, want := range []string{`"fra"`, "custom, eng", "New(languages:)", "WithTessdata"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to mention %q, got: %v", want, err)
		}
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
//
// The error has to name the pdf module and Batch, which is the whole difference
// between an error that ends the caller's afternoon and one that ends their
// next line of code: it names the two calls that fix it rather than leaving
// "render this first" as an errand.
func (t *Tests) PdfInputIsRejected(ctx context.Context) error {
	_, err := ocr().Document(textFile("scan.pdf", "%PDF-1.7\n")).Text(ctx)
	if err == nil {
		return fmt.Errorf("expected a PDF source to be rejected, got nil")
	}
	for _, want := range []string{"scan.pdf", "leptonica", "pdf module", "Batch"} {
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

// ----------------------------------------------------------------- pdf module

// PdfModulePagesBatchIntoOneSearchablePdf runs the flow this module's PDF story
// is now made of, end to end and across two modules: the pdf module renders a
// document's pages, Batch recognises them concurrently, and the per-page
// searchable PDFs are merged back into one.
//
// It is one test rather than three because the seam is what is under test. Each
// module's own suite already covers its half; what neither can check alone is
// that the page names the pdf module writes sort into page order here, that the
// render resolution has to be carried across by the caller because nothing in
// the images states it, and that what Batch returns is in a shape the pdf
// module will take back.
//
// The merge is the half worth spelling out. Batch gives one single-page
// searchable PDF per image — that is what "a directory of independent inputs"
// means, and it is the price of recognising the pages concurrently instead of
// as one serial document — so reassembling the document is the caller's job.
// This pins that the job is one call and not a project.
//
// The TSV is the other half of the primary use case, and the assertion on it is
// per page: each file is named after the page it describes, which is the whole
// reason reconciling word positions back to pages is possible.
func (t *Tests) PdfModulePagesBatchIntoOneSearchablePdf(ctx context.Context) error {
	pages := dag.Pdf().Document(fixture(ledgerPdf)).Convert().WithDpi(ledgerDpi).Png()

	out := ocr().Batch(pages).
		WithDpi(ledgerDpi).
		WithConcurrency(len(ledgerPages)).
		Export([]dagger.TesseractFormat{
			dagger.TesseractFormatTsv,
			dagger.TesseractFormatPdf,
		})

	// The page names are the pdf module's, not this module's, and Batch
	// mirrors them — so the contract that makes the rest of this work is
	// visible in the entry list before anything is read.
	entries, err := out.Entries(ctx)
	if err != nil {
		return fmt.Errorf("Batch over pdf-module pages: %w", err)
	}
	var want []string
	for i := range ledgerPages {
		want = append(want, fmt.Sprintf("page-%04d.pdf", i+1), fmt.Sprintf("page-%04d.tsv", i+1))
	}
	sort.Strings(entries)
	if !reflect.DeepEqual(entries, want) {
		return fmt.Errorf("expected exactly %v, got %v", want, entries)
	}

	// Every page's TSV has to carry that page's own text and no other's, which
	// is what says the pages were recognised in the order their names put them
	// and each artifact landed beside the image that produced it.
	for i, marker := range ledgerPageMarkers {
		name := fmt.Sprintf("page-%04d.tsv", i+1)
		tsv, err := out.File(name).Contents(ctx)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if !strings.Contains(tsv, marker) {
			return fmt.Errorf("expected %s to carry page %d's word %q, got:\n%s", name, i+1, marker, tsv)
		}
		for j, other := range ledgerPageMarkers {
			if j != i && strings.Contains(tsv, other) {
				return fmt.Errorf("expected %s to hold page %d alone, it also carries page %d's %q", name, i+1, j+1, other)
			}
		}
	}

	// And back the other way: the per-page PDFs merge into the document the
	// pages came from, with the page count they went in with.
	var recognised []*dagger.File
	for i := range ledgerPages {
		recognised = append(recognised, out.File(fmt.Sprintf("page-%04d.pdf", i+1)))
	}
	// The page count is read with the pdf module rather than counted out of
	// the bytes. pdfunite rewrites the page objects when it concatenates, so
	// grepping for `/Type /Page` finds none of them — and asking poppler is
	// the stronger assertion anyway: a document it can open and count is a
	// document, where a byte pattern is only a byte pattern.
	merged, err := dag.Pdf().Document(dag.Pdf().Merge(recognised)).PageCount(ctx)
	if err != nil {
		return fmt.Errorf("merge the recognised pages: %w", err)
	}
	if merged != len(ledgerPages) {
		return fmt.Errorf("expected the merged searchable PDF to carry %d pages, got %d", len(ledgerPages), merged)
	}
	return nil
}

// ---------------------------------------------------------------------- batch

// BatchMirrorsInputLayout asserts a directory in gives a directory out with the
// same shape: one artifact per image, at the input's own path with the
// renderer's extension, nested folders and all.
//
// Mirroring is the whole point of the return type. A batch that returned a flat
// directory of `result-1.txt`, `result-2.txt` would force every caller to
// rebuild the correspondence between page and text that the input directory
// already expressed.
func (t *Tests) BatchMirrorsInputLayout(ctx context.Context) error {
	out := ocr().Batch(batchSource()).Export([]dagger.TesseractFormat{dagger.TesseractFormatTxt})

	entries, err := out.Glob(ctx, "**/*")
	if err != nil {
		return fmt.Errorf("Export: %w", err)
	}
	want := []string{"scans/", "scans/deep/", "scans/deep/page-3.txt", "scans/page-1.txt", "scans/page-2.txt"}
	sort.Strings(entries)
	if !reflect.DeepEqual(entries, want) {
		return fmt.Errorf("expected the output to mirror the input layout as %v, got %v", want, entries)
	}

	// Every mirrored artifact has to hold that image's own recognised text,
	// not just exist at the right path.
	for _, name := range []string{"scans/page-1.txt", "scans/page-2.txt", "scans/deep/page-3.txt"} {
		got, err := out.File(name).Contents(ctx)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if err := assertSentenceLines(got); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// BatchDefaultGlobSkipsNonImages asserts the default glob takes the images out
// of a real scan folder and leaves the rest alone.
//
// A folder of scans collects README files, manifests and checksums, and none of
// them are pages. Handing one to tesseract is not a no-op — leptonica fails to
// decode it and the run dies — so "ignored by default" is what makes pointing
// Batch at an existing directory work at all.
func (t *Tests) BatchDefaultGlobSkipsNonImages(ctx context.Context) error {
	got, err := ocr().Batch(batchSource()).Files(ctx)
	if err != nil {
		return fmt.Errorf("Files: %w", err)
	}
	want := []string{"scans/deep/page-3.png", "scans/page-1.png", "scans/page-2.png"}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("expected the default glob to select %v, got %v", want, got)
	}

	// Scanner software writes .JPG as readily as .jpg, so the extension match
	// is case-insensitive.
	got, err = ocr().Batch(dag.Directory().WithFile("PAGE.PNG", fixture(sentencePng))).Files(ctx)
	if err != nil {
		return fmt.Errorf("Files with an upper-case extension: %w", err)
	}
	if want := []string{"PAGE.PNG"}; !reflect.DeepEqual(got, want) {
		return fmt.Errorf("expected %v, got %v", want, got)
	}

	// A directory with nothing recognisable in it is an error naming the
	// filter that rejected everything, not an empty result.
	_, err = ocr().Batch(dag.Directory().
		WithNewFile("notes.md", "x").
		WithNewFile("checksums.txt", "y")).
		Files(ctx)
	if err == nil {
		return fmt.Errorf("expected a directory holding no images to be rejected, got nil")
	}
	for _, want := range []string{"holds no images", "2 file(s)", ".png", "WithGlob"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to mention %q, got: %v", want, err)
		}
	}
	return nil
}

// BatchGlobSelectsFiles asserts WithGlob decides what takes part, and that a
// pattern matching nothing fails the call.
//
// The empty case is the one worth pinning. Returning an empty directory would
// be indistinguishable from a batch that ran and found no text, so a typo in a
// pattern would surface much later as missing output rather than here as a bad
// glob.
func (t *Tests) BatchGlobSelectsFiles(ctx context.Context) error {
	batch := ocr().Batch(batchSource())

	// A single-level pattern stays in its folder; ** crosses into deep/.
	got, err := batch.WithGlob("scans/*.png").Files(ctx)
	if err != nil {
		return fmt.Errorf("Files with a single-level glob: %w", err)
	}
	if want := []string{"scans/page-1.png", "scans/page-2.png"}; !reflect.DeepEqual(got, want) {
		return fmt.Errorf("expected a single-level glob to select %v, got %v", want, got)
	}

	got, err = batch.WithGlob("**/deep/*.png").Files(ctx)
	if err != nil {
		return fmt.Errorf("Files with a nested glob: %w", err)
	}
	if want := []string{"scans/deep/page-3.png"}; !reflect.DeepEqual(got, want) {
		return fmt.Errorf("expected a nested glob to select %v, got %v", want, got)
	}

	// The narrowed set is what gets recognised, so the glob reaches the exec
	// rather than only the listing.
	entries, err := batch.WithGlob("**/deep/*.png").
		Export([]dagger.TesseractFormat{dagger.TesseractFormatTxt}).
		Glob(ctx, "**/*")
	if err != nil {
		return fmt.Errorf("Export with a nested glob: %w", err)
	}
	sort.Strings(entries)
	if want := []string{"scans/", "scans/deep/", "scans/deep/page-3.txt"}; !reflect.DeepEqual(entries, want) {
		return fmt.Errorf("expected the glob to narrow the output to %v, got %v", want, entries)
	}

	_, err = batch.WithGlob("**/*.tif").Files(ctx)
	if err == nil {
		return fmt.Errorf("expected a glob matching nothing to be rejected, got nil")
	}
	for _, want := range []string{`"**/*.tif"`, "matched no files"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to mention %q, got: %v", want, err)
		}
	}
	return nil
}

// BatchExportProducesEveryFormatPerImage asserts each image gets its own full
// artifact set, named off its own path rather than off a shared output base.
//
// This is where the per-image design earns itself: tesseract's list-file mode
// would render one concatenated artifact per *format* — a single .txt with
// form-feed page breaks and a single multi-page PDF — with no way to tell which
// page produced what.
func (t *Tests) BatchExportProducesEveryFormatPerImage(ctx context.Context) error {
	source := dag.Directory().
		WithFile("a.png", fixture(sentencePng)).
		WithFile("nested/b.png", fixture(sentencePng))

	out := ocr().Batch(source).Export([]dagger.TesseractFormat{
		dagger.TesseractFormatTxt,
		dagger.TesseractFormatHocr,
		dagger.TesseractFormatAlto,
		dagger.TesseractFormatTsv,
		dagger.TesseractFormatPdf,
		dagger.TesseractFormatPage,
	})
	entries, err := out.Glob(ctx, "**/*")
	if err != nil {
		return fmt.Errorf("Export: %w", err)
	}
	want := []string{
		"a.hocr", "a.page.xml", "a.pdf", "a.tsv", "a.txt", "a.xml",
		"nested/", "nested/b.hocr", "nested/b.page.xml", "nested/b.pdf",
		"nested/b.tsv", "nested/b.txt", "nested/b.xml",
	}
	sort.Strings(entries)
	if !reflect.DeepEqual(entries, want) {
		return fmt.Errorf("expected every format for every image as %v, got %v", want, entries)
	}

	// The PDF is a real one rather than an empty file left behind by a
	// renderer that ran against the wrong output base.
	pdf, err := exportBytes(ctx, out.File("nested/b.pdf"), "b.pdf")
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		return fmt.Errorf("expected a PDF header in the nested artifact, got %q", firstBytes(pdf, 8))
	}
	return nil
}

// BatchSharesDocumentOptions asserts the recognition options behave the same on
// a batch as on a single document, which is the reason both hold one shared
// option set rather than two parallel copies.
//
// It covers both halves: an option that has to reach tesseract for every image
// in the run, and the deferred validation that has to reject a bad option
// before any of them are recognised.
func (t *Tests) BatchSharesDocumentOptions(ctx context.Context) error {
	batch := ocr().Batch(batchSource())

	// A digits-only whitelist suppresses every word, and has to do so in each
	// image's own artifact — so `-c` reached every iteration of the loop, not
	// just the first.
	out := batch.
		WithParameter("tessedit_char_whitelist", "0123456789").
		Export([]dagger.TesseractFormat{dagger.TesseractFormatTxt})
	for _, name := range []string{"scans/page-1.txt", "scans/page-2.txt", "scans/deep/page-3.txt"} {
		got, err := out.File(name).Contents(ctx)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if strings.Contains(got, "quick") {
			return fmt.Errorf("expected the whitelist to suppress words in %s, got:\n%s", name, got)
		}
	}

	// A word list still has to mount and recognise, which is what catches a
	// flag emitted in the wrong position for the batch argv.
	got, err := batch.
		WithUserWords(textFile("user-words.txt", "vexingly\nSphinx\n")).
		Export([]dagger.TesseractFormat{dagger.TesseractFormatTxt}).
		File("scans/page-1.txt").
		Contents(ctx)
	if err != nil {
		return fmt.Errorf("Export with user words: %w", err)
	}
	if err := assertSentenceLines(got); err != nil {
		return fmt.Errorf("batch with user words: %w", err)
	}

	// The deferred checks are the same ones Document defers, with the same
	// messages, and they fire before anything is recognised.
	for _, tc := range []struct {
		name  string
		build func(*dagger.TesseractBatch) *dagger.TesseractBatch
		want  string
	}{
		{"WithLanguage", func(b *dagger.TesseractBatch) *dagger.TesseractBatch { return b.WithLanguage("deu") }, "is not installed"},
		{"WithDpi", func(b *dagger.TesseractBatch) *dagger.TesseractBatch { return b.WithDpi(0) }, "dpi must be positive"},
		{"WithParameter", func(b *dagger.TesseractBatch) *dagger.TesseractBatch {
			return b.WithParameter("not_a_real_parameter", "1")
		}, "unknown parameter"},
	} {
		_, err := tc.build(batch).
			Export([]dagger.TesseractFormat{dagger.TesseractFormatTxt}).
			Entries(ctx)
		if err == nil {
			return fmt.Errorf("expected batch %s to be rejected, got nil", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			return fmt.Errorf("expected batch %s to fail with %q, got: %v", tc.name, tc.want, err)
		}
	}
	return nil
}

// BatchRejectsAmbiguousInput asserts the two ways a matched set cannot be
// recognised are refused before the run, rather than producing quietly wrong
// output.
//
// A collision is the subtle one: `a.png` and `a.jpg` in one folder both render
// onto `a.txt`, so the second silently overwrites the first and the batch looks
// like it succeeded with one page missing.
func (t *Tests) BatchRejectsAmbiguousInput(ctx context.Context) error {
	_, err := ocr().Batch(dag.Directory().
		WithFile("scans/page.png", fixture(sentencePng)).
		WithFile("scans/page.jpg", fixture(sentencePng))).
		Files(ctx)
	if err == nil {
		return fmt.Errorf("expected two images sharing an output base to be rejected, got nil")
	}
	for _, want := range []string{"scans/page.jpg", "scans/page.png", "scans/page"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the collision error to name %q, got: %v", want, err)
		}
	}

	// A PDF reached by an explicit glob is refused with the same explanation
	// Document gives, since leptonica is the thing that cannot read it either
	// way.
	_, err = ocr().Batch(dag.Directory().WithNewFile("scan.pdf", "%PDF-1.7\n")).
		WithGlob("**/*.pdf").
		Files(ctx)
	if err == nil {
		return fmt.Errorf("expected a PDF reached by an explicit glob to be rejected, got nil")
	}
	for _, want := range []string{"scan.pdf", "leptonica", "pdf module"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to mention %q, got: %v", want, err)
		}
	}
	return nil
}

// BatchConcurrencyMatchesSerialOutput asserts a batch recognised several
// images at a time produces exactly what the same batch produces one at a
// time, and that a bound that would recognise nothing is refused.
//
// Byte equality is the whole promise of the knob: concurrency is allowed to
// change how long a batch takes and nothing else. What it pins now that the
// bound also decides how the images are *sliced* into execs is that the slicing
// is invisible in the answer — the same four images come back whether they were
// recognised by one exec, two, three, four or sixteen, assembled from
// directories that finished in whatever order they finished in. The digest
// covers the mirrored layout too, nested directories and all.
//
// Three is in the list because four does not divide by it: the slices come out
// 2, 1, 1, which is the shape an off-by-one in the partition drops an image
// from. A bound wider than the batch is there because it is the ordinary case
// for a small scan folder on a large machine — the default is one recognition
// per CPU, and most folders are smaller than the core count — and it is the
// shape that must not produce empty execs. One is the whole batch in a single
// exec, which before #371 was still four containers.
func (t *Tests) BatchConcurrencyMatchesSerialOutput(ctx context.Context) error {
	source := dag.Directory().
		WithFile("scans/page-1.png", fixture(sentencePng)).
		WithFile("scans/page-2.png", fixture(sentencePng)).
		WithFile("scans/deep/page-3.png", fixture(sentencePng)).
		WithFile("scans/deep/page-4.png", fixture(sentencePng))
	batch := ocr().Batch(source)
	formats := []dagger.TesseractFormat{dagger.TesseractFormatTxt, dagger.TesseractFormatTsv}

	serial := batch.WithConcurrency(1).Export(formats)
	concurrent := batch.WithConcurrency(4).Export(formats)

	entries, err := concurrent.Glob(ctx, "**/*")
	if err != nil {
		return fmt.Errorf("Export with concurrency: %w", err)
	}
	want := []string{
		"scans/", "scans/deep/",
		"scans/deep/page-3.tsv", "scans/deep/page-3.txt",
		"scans/deep/page-4.tsv", "scans/deep/page-4.txt",
		"scans/page-1.tsv", "scans/page-1.txt",
		"scans/page-2.tsv", "scans/page-2.txt",
	}
	sort.Strings(entries)
	if !reflect.DeepEqual(entries, want) {
		return fmt.Errorf("expected a concurrent batch to mirror the input as %v, got %v", want, entries)
	}

	serialDigest, err := serial.Digest(ctx)
	if err != nil {
		return fmt.Errorf("Export one image at a time: %w", err)
	}
	for _, tc := range []struct {
		name string
		dir  *dagger.Directory
	}{
		{"unset, one per CPU", batch.Export(formats)},
		{"two at a time", batch.WithConcurrency(2).Export(formats)},
		{"three at a time, which four does not divide by", batch.WithConcurrency(3).Export(formats)},
		{"one exec per image", concurrent},
		{"wider than the batch", batch.WithConcurrency(16).Export(formats)},
	} {
		digest, err := tc.dir.Digest(ctx)
		if err != nil {
			return fmt.Errorf("Export %s: %w", tc.name, err)
		}
		if digest != serialDigest {
			return fmt.Errorf(
				"expected a batch recognised %s to produce byte-identical artifacts, got digest %s against the one-exec run's %s",
				tc.name, digest, serialDigest)
		}
	}

	// Zero workers would recognise nothing at all, which is a directory of
	// missing artifacts rather than a failure, so it is refused by name.
	for _, n := range []int{0, -2} {
		_, err := batch.WithConcurrency(n).Export(formats).Entries(ctx)
		if err == nil {
			return fmt.Errorf("expected concurrency %d to be rejected, got nil", n)
		}
		for _, want := range []string{"WithConcurrency", "concurrency must be positive", strconv.Itoa(n)} {
			if !strings.Contains(err.Error(), want) {
				return fmt.Errorf("expected the error for concurrency %d to mention %q, got: %v", n, want, err)
			}
		}
	}
	return nil
}

// BatchConcurrencyReportsFailingImage asserts a page tesseract cannot read
// fails the whole batch — however the images were sliced into execs — with a
// message that names the page and carries tesseract's own complaint about it.
//
// Failing loudly is the half of a parallel rewrite that is easy to lose: a
// runner that reports only its own exit status turns an unreadable page into a
// batch that "succeeded" with one artifact quietly missing from a thousand.
//
// Naming the page has to be the batch's own doing, and since #371 that means
// naming it out of an exec that recognised several. tesseract handed a file
// leptonica will not decode falls back to reading it as a list of image paths,
// so its message names the file's first *line* rather than the file — here,
// the words in the fake PNG. Good pages recognise fine either side of it, so
// what is asserted is a failure that survives its siblings succeeding.
//
// The bounds are chosen to put the torn page at three different positions in
// its slice. Sorted, the five images are page-3, page-1, page-2, torn, zz-page-5;
// at a bound of one that is a single exec failing on its *fourth* invocation
// with a fifth still to come, at two it is the first invocation of the second
// slice, and at five it is an exec of its own. A slice that reported its own
// name, or the first image in it, would pass the last of those and fail the
// other two.
func (t *Tests) BatchConcurrencyReportsFailingImage(ctx context.Context) error {
	source := dag.Directory().
		WithFile("scans/page-1.png", fixture(sentencePng)).
		WithFile("scans/page-2.png", fixture(sentencePng)).
		WithFile("scans/deep/page-3.png", fixture(sentencePng)).
		WithFile("scans/zz-page-5.png", fixture(sentencePng)).
		WithNewFile("scans/torn.png", "not an image at all\n")
	batch := ocr().Batch(source)

	for _, tc := range []struct {
		name  string
		batch *dagger.TesseractBatch
	}{
		{"in one exec, fourth of five", batch.WithConcurrency(1)},
		{"first of its slice", batch.WithConcurrency(2)},
		{"in an exec of its own", batch.WithConcurrency(5)},
		{"at the default bound", batch},
	} {
		_, err := tc.batch.
			Export([]dagger.TesseractFormat{dagger.TesseractFormatTxt}).
			Entries(ctx)
		if err == nil {
			return fmt.Errorf("expected an unreadable image to fail the batch %s, got nil", tc.name)
		}
		for _, want := range []string{"scans/torn.png", "cannot be read", "Error during processing"} {
			if !strings.Contains(err.Error(), want) {
				return fmt.Errorf("expected the failure %s to mention %q, got: %v", tc.name, want, err)
			}
		}
		// The slice's other images must not be mistaken for the failing one,
		// which is what a message built from the slice rather than from the
		// script would do.
		for _, unwanted := range []string{"scans/deep/page-3.png", "scans/zz-page-5.png"} {
			if strings.Contains(err.Error(), "Batch: "+strconv.Quote(unwanted)+":") {
				return fmt.Errorf("expected the failure %s to name only the torn page, got: %v", tc.name, err)
			}
		}
	}
	return nil
}

// ------------------------------------------------------------------------- ci

// CiRunProducesEnabledFormats asserts the pipeline's output is the batch it
// wraps: every enabled format for every matched image, at the input's own path.
//
// The default is the half worth pinning. A pipeline that produced nothing until
// WithFormats was called would look like a broken directory rather than an
// unconfigured one, so an unconfigured Ci renders plain text — the format
// tesseract itself produces when no renderer is named.
func (t *Tests) CiRunProducesEnabledFormats(ctx context.Context) error {
	entries, err := ocr().Ci(batchSource()).Run().Glob(ctx, "**/*")
	if err != nil {
		return fmt.Errorf("Run: %w", err)
	}
	want := []string{"scans/", "scans/deep/", "scans/deep/page-3.txt", "scans/page-1.txt", "scans/page-2.txt"}
	sort.Strings(entries)
	if !reflect.DeepEqual(entries, want) {
		return fmt.Errorf("expected an unconfigured Ci to render %v, got %v", want, entries)
	}

	out := ocr().Ci(batchSource()).
		WithFormats([]dagger.TesseractFormat{
			dagger.TesseractFormatTxt,
			dagger.TesseractFormatHocr,
			dagger.TesseractFormatPdf,
		}).
		Run()
	entries, err = out.Glob(ctx, "**/*")
	if err != nil {
		return fmt.Errorf("Run with three formats: %w", err)
	}
	want = []string{
		"scans/", "scans/deep/",
		"scans/deep/page-3.hocr", "scans/deep/page-3.pdf", "scans/deep/page-3.txt",
		"scans/page-1.hocr", "scans/page-1.pdf", "scans/page-1.txt",
		"scans/page-2.hocr", "scans/page-2.pdf", "scans/page-2.txt",
	}
	sort.Strings(entries)
	if !reflect.DeepEqual(entries, want) {
		return fmt.Errorf("expected every enabled format per image as %v, got %v", want, entries)
	}

	// The artifacts have to be this image's own recognised text, not merely
	// files at the right paths.
	got, err := out.File("scans/deep/page-3.txt").Contents(ctx)
	if err != nil {
		return fmt.Errorf("read scans/deep/page-3.txt: %w", err)
	}
	return assertSentenceLines(got)
}

// CiMinConfidenceGatesTheRun asserts the quality gate: recognition that came
// back worse than the threshold fails the run, and the failure names both the
// value measured and the page that measured it.
//
// Reporting the value is what makes the threshold adjustable. "Confidence too
// low" leaves the caller guessing whether they are one point short or thirty,
// and the only way to find out would be to re-run the whole batch by hand
// asking for TSV.
//
// Naming the page is what makes it actionable, and the mixed directory is where
// that has to be earned: one clean scan and one blank page under a threshold
// only the blank page misses. A gate that failed the batch as a whole would be
// telling the caller to go and diff a hundred TSVs.
func (t *Tests) CiMinConfidenceGatesTheRun(ctx context.Context) error {
	clean := dag.Directory().WithFile("scans/page-1.png", fixture(sentencePng))

	_, err := ocr().Ci(clean).WithMinConfidence(95).Run().Entries(ctx)
	if err == nil {
		return fmt.Errorf("expected a threshold above what the fixture measures to fail the run, got nil")
	}
	for _, want := range []string{"scans/page-1.png", "95"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the gate error to mention %q, got: %v", want, err)
		}
	}
	measured, ok := measuredConfidence(err.Error())
	if !ok {
		return fmt.Errorf("expected the gate error to report the measured confidence, got: %v", err)
	}
	if measured <= 0 || measured >= 95 {
		return fmt.Errorf("expected a measured confidence in (0, 95), got %v from: %v", measured, err)
	}

	// A blank page recognises nothing at all, so there is no confidence to
	// measure on it — which is a scan worth failing a build over, and worth
	// saying so rather than reporting it as zero.
	mixed := clean.WithFile("scans/blank.pbm", blankPage("blank.pbm"))
	_, err = ocr().Ci(mixed).WithMinConfidence(80).Run().Entries(ctx)
	if err == nil {
		return fmt.Errorf("expected a blank page to fail an 80%% gate, got nil")
	}
	if !strings.Contains(err.Error(), "scans/blank.pbm") {
		return fmt.Errorf("expected the gate error to name the offending page, got: %v", err)
	}
	if strings.Contains(err.Error(), "scans/page-1.png") {
		return fmt.Errorf("expected the gate error to name only the page below the threshold, got: %v", err)
	}
	if !strings.Contains(err.Error(), "no words") {
		return fmt.Errorf("expected the gate error to say the page recognised nothing, got: %v", err)
	}

	// A threshold the scan clears produces the artifacts as usual, so the gate
	// is a gate rather than a tax.
	got, err := ocr().Ci(clean).WithMinConfidence(80).Run().File("scans/page-1.txt").Contents(ctx)
	if err != nil {
		return fmt.Errorf("Run under a threshold the fixture clears: %w", err)
	}
	return assertSentenceLines(got)
}

// CiCheckRunsTheGateWithoutArtifacts asserts Check reaches the same verdict
// Run's gate does, and that it is a real pass over the scans rather than a
// signature that returns nil.
//
// Its return type is the whole reason it exists — an error and nothing else, so
// a PR gate that never wants the archive cannot accidentally be handed one —
// and that makes "did it actually look?" the thing worth testing. A Check that
// short-circuited when no threshold was set would be a green build over a
// directory of files tesseract cannot read at all.
func (t *Tests) CiCheckRunsTheGateWithoutArtifacts(ctx context.Context) error {
	clean := dag.Directory().WithFile("scans/page-1.png", fixture(sentencePng))

	if err := ocr().Ci(clean).WithMinConfidence(80).Check(ctx); err != nil {
		return fmt.Errorf("expected a threshold the fixture clears to pass Check: %w", err)
	}

	err := ocr().Ci(clean).WithMinConfidence(95).Check(ctx)
	if err == nil {
		return fmt.Errorf("expected a threshold above what the fixture measures to fail Check, got nil")
	}
	if !strings.Contains(err.Error(), "scans/page-1.png") {
		return fmt.Errorf("expected Check to name the offending page, got: %v", err)
	}

	// With no threshold there is still a bar: every matched file has to be an
	// image tesseract can read.
	err = ocr().Ci(dag.Directory().WithNewFile("scans/page-1.png", "not an image\n")).Check(ctx)
	if err == nil {
		return fmt.Errorf("expected Check to recognise the scans even with no threshold set, got nil")
	}
	if !strings.Contains(err.Error(), "tesseract failed") {
		return fmt.Errorf("expected the error to come from recognition, got: %v", err)
	}
	return nil
}

// CiGateKeepsItsTsvOutOfTheOutput asserts the output is what WithFormats asked
// for, whether or not a threshold was set.
//
// The gate measures a TSV, and Run renders that TSV in the same pass as the
// artifacts rather than paying for a second one. That is an implementation
// decision and it has to stay one: a caller who enabled a threshold and asked
// for PDFs did not ask for a TSV beside every page, and would find one in the
// archive they published. A caller who did ask for TSV keeps it, which is the
// half that catches a filter written as "drop every TSV".
func (t *Tests) CiGateKeepsItsTsvOutOfTheOutput(ctx context.Context) error {
	source := dag.Directory().
		WithFile("scans/page-1.png", fixture(sentencePng)).
		WithFile("scans/deep/page-2.png", fixture(sentencePng))

	entries, err := ocr().Ci(source).
		WithFormats([]dagger.TesseractFormat{dagger.TesseractFormatTxt}).
		WithMinConfidence(80).
		Run().
		Glob(ctx, "**/*")
	if err != nil {
		return fmt.Errorf("Run with a gate: %w", err)
	}
	want := []string{"scans/", "scans/deep/", "scans/deep/page-2.txt", "scans/page-1.txt"}
	sort.Strings(entries)
	if !reflect.DeepEqual(entries, want) {
		return fmt.Errorf("expected a gated run to produce only %v, got %v", want, entries)
	}

	entries, err = ocr().Ci(source).
		WithFormats([]dagger.TesseractFormat{dagger.TesseractFormatTxt, dagger.TesseractFormatTsv}).
		WithMinConfidence(80).
		Run().
		Glob(ctx, "**/*")
	if err != nil {
		return fmt.Errorf("Run with a gate and TSV enabled: %w", err)
	}
	want = []string{
		"scans/", "scans/deep/",
		"scans/deep/page-2.tsv", "scans/deep/page-2.txt",
		"scans/page-1.tsv", "scans/page-1.txt",
	}
	sort.Strings(entries)
	if !reflect.DeepEqual(entries, want) {
		return fmt.Errorf("expected an enabled TSV to survive the gate as %v, got %v", want, entries)
	}
	return nil
}

// CiLanguageReachesRecognition asserts the pipeline's language is the batch's
// language, in both directions: a selected one recognises, and one the image
// does not carry is rejected with the same explanation a bare batch gives.
//
// Ci owns no option handling of its own — that is what makes it a bundle of
// calls rather than a second implementation — and the rejection is the half that
// proves it, because the check that produces it lives on the shared option set
// and could only fire if the value got there. It also fires before recognition,
// so a wrong-language pipeline costs a message rather than a batch.
func (t *Tests) CiLanguageReachesRecognition(ctx context.Context) error {
	source := dag.Directory().WithFile("scans/page-1.png", fixture(sentencePng))

	got, err := ocr("eng", "deu").Ci(source).
		WithLanguage("eng").
		Run().
		File("scans/page-1.txt").
		Contents(ctx)
	if err != nil {
		return fmt.Errorf("Run with a selected language: %w", err)
	}
	if err := assertSentenceLines(got); err != nil {
		return err
	}

	_, err = ocr().Ci(source).WithLanguage("fra").Run().Entries(ctx)
	if err == nil {
		return fmt.Errorf("expected an uninstalled language to be rejected, got nil")
	}
	for _, want := range []string{"WithLanguage", "fra", "eng"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to mention %q, got: %v", want, err)
		}
	}
	return nil
}

// CiRejectsUnusableThreshold asserts a bar no page could miss, or none could
// clear, is refused before anything is recognised.
//
// Zero is the one worth spelling out. It is not a lenient gate, it is no gate at
// all — every page clears it — so a build configured that way reports a passing
// quality check it never made. Accepting it silently would make the difference
// between a gate and a decoration invisible.
//
// It is checked through Check rather than Run because that is where a
// misconfiguration costs the most to discover late: Check is the call a PR gate
// makes, and its whole answer is the error it returns.
func (t *Tests) CiRejectsUnusableThreshold(ctx context.Context) error {
	source := dag.Directory().WithFile("scans/page-1.png", fixture(sentencePng))

	for _, percent := range []int{0, -10, 101} {
		err := ocr().Ci(source).WithMinConfidence(percent).Check(ctx)
		if err == nil {
			return fmt.Errorf("expected a threshold of %d to be rejected, got nil", percent)
		}
		for _, want := range []string{"WithMinConfidence", "1 and 100"} {
			if !strings.Contains(err.Error(), want) {
				return fmt.Errorf("expected the error for %d to mention %q, got: %v", percent, want, err)
			}
		}
	}
	return nil
}

// -------------------------------------------------------- training-adjacent

// BoxReportsCharacterBoxes asserts the box renderer descends to the character
// level, which is the level nothing else this module offers reaches: hOCR and
// TSV stop at the word.
func (t *Tests) BoxReportsCharacterBoxes(ctx context.Context) error {
	got, err := ocr().Document(fixture(sentencePng)).Box().Contents(ctx)
	if err != nil {
		return fmt.Errorf("Box: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) < len(strings.Join(sentenceLines, ""))/2 {
		return fmt.Errorf("expected a row per recognised character, got %d rows:\n%s", len(lines), got)
	}

	// Every row is `<char> <left> <bottom> <right> <top> <page>`, and the
	// characters in order spell what the page says once the spaces the format
	// does not record are put back.
	var chars []string
	for _, line := range lines {
		cols := strings.Split(line, " ")
		if len(cols) != 6 {
			return fmt.Errorf("expected 6 columns in %q, got %d", line, len(cols))
		}
		for _, col := range cols[1:] {
			if _, err := strconv.Atoi(col); err != nil {
				return fmt.Errorf("expected numeric box coordinates in %q: %w", line, err)
			}
		}
		chars = append(chars, cols[0])
	}
	if want := strings.ReplaceAll(sentenceLines[0], " ", ""); !strings.HasPrefix(strings.Join(chars, ""), want) {
		return fmt.Errorf("expected the boxes to spell %q, got %q", want, strings.Join(chars, ""))
	}
	return nil
}

// ProcessedImagesReturnsThresholdedTiff asserts the image tesseract actually
// recognised comes back, and that it is the processed one rather than the
// source: the fixture goes in as a PNG and this comes out as a TIFF, which is
// the observable half of "this is a derivative, not your file".
func (t *Tests) ProcessedImagesReturnsThresholdedTiff(ctx context.Context) error {
	got, err := exportBytes(ctx, ocr().Document(fixture(sentencePng)).ProcessedImages(), "result.tif")
	if err != nil {
		return err
	}
	// TIFF leads with a byte-order mark and the magic number 42 in that order.
	if !bytes.HasPrefix(got, []byte("II\x2a\x00")) && !bytes.HasPrefix(got, []byte("MM\x00\x2a")) {
		return fmt.Errorf("expected a TIFF header, got %q", firstBytes(got, 8))
	}
	return nil
}

// LstmTrainBuildsTrainingSample asserts one image plus one line of ground truth
// becomes a training sample carrying that line, and that the two ways of
// asking for a sample that cannot exist are refused.
//
// The transcription is checked inside the `.lstmf` rather than by training on
// it, because that is what a sample is *for*: the file pairs the line's pixels
// with the characters they are supposed to be, and a sample built against the
// wrong text trains the model to be wrong without ever failing.
func (t *Tests) LstmTrainBuildsTrainingSample(ctx context.Context) error {
	got, err := exportBytes(ctx,
		ocr().Document(trainingFixture("line-1.png")).LstmTrain(sentenceLines[0]),
		"line-1.lstmf")
	if err != nil {
		return err
	}
	if !bytes.Contains(got, []byte(sentenceLines[0])) {
		return fmt.Errorf("expected the sample to carry the ground truth %q, got %d bytes", sentenceLines[0], len(got))
	}

	for _, tc := range []struct {
		name  string
		build func() *dagger.File
		want  string
	}{
		{
			"empty ground truth",
			func() *dagger.File { return ocr().Document(trainingFixture("line-1.png")).LstmTrain("  ") },
			"groundTruth is required",
		},
		{
			"multi-line ground truth",
			func() *dagger.File {
				return ocr().Document(trainingFixture("line-1.png")).LstmTrain(strings.Join(sentenceLines, "\n"))
			},
			"more than one line",
		},
	} {
		if _, err := tc.build().Contents(ctx); err == nil {
			return fmt.Errorf("expected %s to be rejected, got nil", tc.name)
		} else if !strings.Contains(err.Error(), tc.want) {
			return fmt.Errorf("expected %s to fail with %q, got: %v", tc.name, tc.want, err)
		}
	}
	return nil
}

// ------------------------------------------------------------------- training

// TrainingPairsImagesWithGroundTruth asserts the source directory is read as
// pairs, and that every way it can fail to be a training set is named by the
// file responsible.
//
// Naming the file is the whole point. A training directory is assembled by
// script — crop the lines, write the transcriptions — and the failures are
// off-by-one ones: the run stops one image short, or one transcription is
// saved under the wrong stem. "Something is unpaired" sends the caller to diff
// two file listings; "line-3.png has no ground truth" does not.
func (t *Tests) TrainingPairsImagesWithGroundTruth(ctx context.Context) error {
	got, err := ocr().Training(trainingSource()).Files(ctx)
	if err != nil {
		return fmt.Errorf("Files: %w", err)
	}
	want := []string{"line-1.png", "line-2.png", "line-3.png", "line-4.png"}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("expected the fixture pairs %v, got %v", want, got)
	}

	for _, tc := range []struct {
		name   string
		source *dagger.Directory
		want   []string
	}{
		{
			"image with no ground truth",
			trainingSource().WithoutFile("line-3.gt.txt"),
			[]string{"line-3.png", "line-3.gt.txt", "has no ground truth"},
		},
		{
			"ground truth with no image",
			trainingSource().WithoutFile("line-2.png"),
			[]string{"line-2.gt.txt", "no image"},
		},
		{
			"two images sharing one ground truth",
			trainingSource().WithFile("line-1.jpg", trainingFixture("line-1.png")),
			[]string{"line-1.jpg", "line-1.png", "line-1.gt.txt"},
		},
		{
			"nothing usable at all",
			dag.Directory().WithNewFile("README.md", "transcriptions pending\n"),
			[]string{"no image and ground-truth pairs", "1 file(s)"},
		},
		{
			"nothing at all",
			dag.Directory(),
			[]string{"source directory is empty"},
		},
	} {
		_, err := ocr().Training(tc.source).Files(ctx)
		if err == nil {
			return fmt.Errorf("expected %s to be rejected, got nil", tc.name)
		}
		for _, want := range tc.want {
			if !strings.Contains(err.Error(), want) {
				return fmt.Errorf("expected %s to name %q, got: %v", tc.name, want, err)
			}
		}
	}
	return nil
}

// TrainingRejectsUnusableInput asserts every training run that could only fail
// is refused before it starts, and refused by whatever is wrong with it.
//
// A training run is the most expensive thing this module does, so the cost of
// finding out late is not a slow error message — it is minutes of a machine
// arriving at a failure that was visible from the outside the whole time.
func (t *Tests) TrainingRejectsUnusableInput(ctx context.Context) error {
	for _, tc := range []struct {
		name  string
		build func() *dagger.TesseractTraining
		want  []string
	}{
		{
			"no base model at all",
			func() *dagger.TesseractTraining { return ocr().Training(trainingSource()) },
			[]string{"base model is required", "WithBaseModel", "tessdata_best"},
		},
		{
			"a base model the image does not carry",
			func() *dagger.TesseractTraining {
				return ocr().Training(trainingSource()).WithBaseModel("deu")
			},
			[]string{`"deu"`, "not installed", "eng", "WithTessdata"},
		},
		{
			"a negative iteration count",
			func() *dagger.TesseractTraining {
				return ocr().Training(trainingSource()).WithBaseModel("eng").WithIterations(-1)
			},
			[]string{"iterations must be positive"},
		},
		{
			"ground truth of more than one line",
			func() *dagger.TesseractTraining {
				return ocr().Training(trainingSource().
					WithNewFile("line-1.gt.txt", strings.Join(sentenceLines, "\n"))).
					WithBaseModel("eng")
			},
			[]string{"line-1.gt.txt", "holds 4 lines", "one file per line"},
		},
		{
			"ground truth with nothing in it",
			func() *dagger.TesseractTraining {
				return ocr().Training(trainingSource().WithNewFile("line-2.gt.txt", "\n  \n")).
					WithBaseModel("eng")
			},
			[]string{"line-2.gt.txt", "is empty"},
		},
	} {
		_, err := tc.build().Traineddata().Size(ctx)
		if err == nil {
			return fmt.Errorf("expected %s to be rejected, got nil", tc.name)
		}
		for _, want := range tc.want {
			if !strings.Contains(err.Error(), want) {
				return fmt.Errorf("expected %s to name %q, got: %v", tc.name, want, err)
			}
		}
	}
	return nil
}

// TrainingRequiresFloatBaseModel asserts the one failure this module cannot
// prevent is at least explained: fine-tuning from a packaged model.
//
// Every model Alpine packages comes from tesseract-ocr/tessdata, whose weights
// are quantized to integers so recognition is fast, and lstmtraining will not
// continue from one. That is not a mistake a caller can see coming — the model
// loads, recognises, and lists as a language like any other — and lstmtraining
// says only "eng.lstm is an integer (fast) model", which names neither the
// float models nor how to get one onto the image.
func (t *Tests) TrainingRequiresFloatBaseModel(ctx context.Context) error {
	_, err := ocr().Training(trainingSource()).
		WithBaseModel("eng").
		WithIterations(1).
		Traineddata().
		Size(ctx)
	if err == nil {
		return fmt.Errorf("expected fine-tuning from a packaged model to be rejected, got nil")
	}
	for _, want := range []string{`"eng"`, "quantized", "tessdata_best", "WithTessdata"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to name %q, got: %v", want, err)
		}
	}
	return nil
}

// TrainingProducesUsableModel asserts the whole round trip: transcribed lines
// in, a `.traineddata` out, and that model recognising a page through
// WithTessdata like any other language.
//
// The page it reads is the one the training lines were cut out of, which is
// what makes "usable" checkable at all. A model that came back malformed, or
// assembled without the base model's unicharset, does not read anything —
// while a model that trained on the wrong text reads this page wrong. Both are
// the same assertion here.
//
// The run uses the default iteration count rather than a smaller one, because
// what that default is for is precisely this: a bound low enough that a
// training run belongs in a test suite. If it ever stops being, this test is
// where that shows up.
func (t *Tests) TrainingProducesUsableModel(ctx context.Context) error {
	training := trainable().Training(trainingSource()).WithBaseModel(floatModelLang)

	// The model is renamed on the way in, which is the documented way to call
	// a fine-tuned model something other than what it was trained from: the
	// language is the file's stem and nothing else.
	tuned := ocr().WithTessdata(dag.Directory().WithFile("tuned.traineddata", training.Traineddata()))

	langs, err := tuned.Langs(ctx)
	if err != nil {
		return fmt.Errorf("Langs with the fine-tuned model: %w", err)
	}
	if want := []string{"eng", "tuned"}; !reflect.DeepEqual(langs, want) {
		return fmt.Errorf("expected the fine-tuned model to list as %v, got %v", want, langs)
	}

	got, err := tuned.Document(fixture(sentencePng)).WithLanguage("tuned").Text(ctx)
	if err != nil {
		return fmt.Errorf("Text under the fine-tuned model: %w", err)
	}
	if err := assertSentenceLines(got); err != nil {
		return fmt.Errorf("fine-tuned model: %w", err)
	}

	// Evaluate reads the same finished run rather than training again, and
	// reports both rates lstmeval measures. The value is a training-set error,
	// so it says the fit took — not that the model generalises.
	report, err := training.Evaluate(ctx)
	if err != nil {
		return fmt.Errorf("Evaluate: %w", err)
	}
	for _, want := range []string{"BCER eval=", "BWER eval="} {
		if !strings.Contains(report, want) {
			return fmt.Errorf("expected the evaluation to report %q, got %q", want, report)
		}
	}
	rate, err := strconv.ParseFloat(strings.TrimSpace(strings.SplitN(strings.TrimPrefix(report, "BCER eval="), ",", 2)[0]), 64)
	if err != nil {
		return fmt.Errorf("expected a character error rate in %q: %w", report, err)
	}
	// The training lines are clean renderings of text the base model already
	// reads, so anything but a near-zero fit means the samples were built
	// against the wrong pixels or the wrong text.
	if rate > 1 {
		return fmt.Errorf("expected a character error rate under 1%%, got %v from %q", rate, report)
	}
	return nil
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

// traineddata lifts a packaged model back out of the assembled image so a test
// can re-mount it under a different name. The path is the apk package's own,
// not the one WithTessdata mounts at, and it is spelled out here rather than
// exported by the module: the module's job is to make caller-supplied models
// work, not to publish where Alpine keeps its own.
func traineddata(t *dagger.Tesseract, lang string) *dagger.File {
	return t.Container().File(packagedTessdataDir + "/" + lang + ".traineddata")
}

// batchSource builds the directory the batch tests recognise: three copies of
// the upright fixture at three different depths, plus the non-image files a
// real scan folder collects. It is assembled from the committed fixtures rather
// than adding more of them, so the layout a test needs is visible in the test.
func batchSource() *dagger.Directory {
	return dag.Directory().
		WithFile("scans/page-1.png", fixture(sentencePng)).
		WithFile("scans/page-2.png", fixture(sentencePng)).
		WithFile("scans/deep/page-3.png", fixture(sentencePng)).
		WithNewFile("README.md", "scanned intake, 2026-07\n").
		WithNewFile("scans/manifest.txt", "page-1\npage-2\n")
}

// fixture returns a committed test image by name under fixtures/.
func fixture(name string) *dagger.File {
	return dag.CurrentModule().Source().File("fixtures/" + name)
}

// blankPage builds an all-white page as an ASCII PBM: the "P1" netpbm format,
// whose pixels are the literal characters "0" and "1".
//
// It is written here rather than committed because it is the one image these
// tests need that can be spelled in text. Every other fixture is a PNG for the
// reason the package comment gives — bare Alpine has no fonts, so an image with
// writing on it has to arrive from outside — but a page with nothing on it
// needs no font, and P1 needs no encoder.
//
// The pixel values are space-separated. Leptonica's ASCII netpbm reader takes
// one whitespace-delimited token per pixel, and a P1 raster written as an
// unbroken run of digits — which the format also permits — fails to decode.
func blankPage(name string) *dagger.File {
	row := strings.TrimSpace(strings.Repeat("0 ", blankPageWidth))
	var sb strings.Builder
	fmt.Fprintf(&sb, "P1\n%d %d\n", blankPageWidth, blankPageHeight)
	for i := 0; i < blankPageHeight; i++ {
		sb.WriteString(row)
		sb.WriteString("\n")
	}
	return dag.Directory().WithNewFile(name, sb.String()).File(name)
}

// measuredConfidence pulls the confidence percentage out of a gate error, so
// the assertion is that the number reported is the measured one rather than
// that the message reads a particular way.
//
// It matches only a value carrying a decimal point, which is what separates the
// measurement from the whole-number threshold printed beside it.
func measuredConfidence(msg string) (float64, bool) {
	m := confidencePattern.FindStringSubmatch(msg)
	if m == nil {
		return 0, false
	}
	got, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	return got, true
}

// trainingSource returns the committed training set: fixtures/training holds
// the four lines of sentence.png cut out of it one at a time, each beside the
// `.gt.txt` naming what it renders.
//
// They are cuts of that fixture rather than a fresh rendering so the training
// data and the page the fine-tuned model is then asked to read are the same
// glyphs in the same font — which is what lets one test assert that a model
// trained here reads the page there.
func trainingSource() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/training")
}

// trainingFixture returns one committed line image by name.
func trainingFixture(name string) *dagger.File {
	return dag.CurrentModule().Source().File("fixtures/training/" + name)
}

// trainable returns the module under test carrying a model fine-tuning can
// actually start from.
//
// It is fetched rather than committed and it is not packaged, because there is
// no third option: `lstmtraining` refuses to continue from a quantized model,
// every model Alpine packages is quantized, and the float ones are 15MB
// apiece. The URL is pinned to a tag so the base model a run starts from does
// not change under the suite, and Dagger caches the fetch, so the download is
// paid once per engine rather than once per run.
func trainable() *dagger.Tesseract {
	return ocr().WithTessdata(
		dag.Directory().WithFile(floatModelLang+".traineddata", dag.HTTP(floatModelURL)),
	)
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

// assertLedgerPages checks recognised text against what ledger.pdf renders,
// and — the point of a multi-page source — that the pages appear in document
// order rather than merely all being present.
//
// Order is checked by where each line was found rather than by splitting on
// the page separator, because that separator differs by renderer: plain text
// gets a form feed, the structured formats get their own page elements. A
// monotonic position says the same thing for all of them.
func assertLedgerPages(got string) error {
	prev := -1
	for i, lines := range ledgerPages {
		for _, want := range lines {
			at := strings.Index(got, want)
			if at < 0 {
				return fmt.Errorf("expected page %d to contribute %q, got:\n%s", i+1, want, got)
			}
			if at < prev {
				return fmt.Errorf("expected page %d's %q to follow the preceding page, got:\n%s", i+1, want, got)
			}
			prev = at
		}
	}
	return nil
}

// assertPageOrder checks that each page contributed its own marker word and
// that the markers appear in page order. It is the check assertLedgerPages
// cannot make on a format that puts every word in its own element.
func assertPageOrder(got string) error {
	prev := -1
	for i, want := range ledgerPageMarkers {
		at := strings.Index(got, want)
		if at < 0 {
			return fmt.Errorf("expected page %d to contribute the word %q, got:\n%s", i+1, want, got)
		}
		if at < prev {
			return fmt.Errorf("expected page %d's %q to follow the preceding page, got:\n%s", i+1, want, got)
		}
		prev = at
	}
	return nil
}

// pageTitles pulls the ocr_page title lines out of hOCR, so a geometry
// mismatch reports the handful of lines that carry the geometry instead of the
// whole document.
func pageTitles(hocr string) string {
	var out []string
	for _, line := range strings.Split(hocr, "\n") {
		if strings.Contains(line, "ocr_page") {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return strings.Join(out, "\n")
}

// tsvPageNums returns the distinct page numbers the TSV rows carry, in the
// order they first appear, which is both what pages were recognised and what
// order they arrived in.
func tsvPageNums(tsv string) []string {
	var (
		out  []string
		seen = map[string]struct{}{}
	)
	for i, line := range strings.Split(strings.TrimSpace(tsv), "\n") {
		cols := strings.Split(line, "\t")
		// Row 0 is the column header; page_num is the second column.
		if i == 0 || len(cols) < 2 {
			continue
		}
		if _, dup := seen[cols[1]]; dup {
			continue
		}
		seen[cols[1]] = struct{}{}
		out = append(out, cols[1])
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
