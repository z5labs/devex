package main

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

	"dagger/tesseract/internal/dagger"
)

// Document is one unit of recognition plus the options that apply to it. It is
// immutable: every With* returns a copy, so a configured Document can be
// branched into several outputs without the branches interfering.
//
// The unit comes in two shapes, and a document holds whichever it was built
// as: a single image in Source, or the rasterized pages of a PDF in Pages.
// They are alternatives rather than layers — exactly one is ever set — and
// everything downstream of validate is written against the resolved FILE
// argument, so the outputs, the options and the error paths are shared rather
// than reimplemented per shape.
//
// The options themselves live on the shared options type, which Batch carries
// too — the builders here are forwarders, so a new recognition option reaches
// both units of work at once instead of being implemented twice.
type Document struct {
	// +private
	Tesseract *Tesseract
	// +private
	Source *dagger.File
	// +private
	Pages *dagger.Directory
	// +private
	PdfDpi int
	// +private
	Options options
}

// WithLanguage selects the recognition language (`-l`). Several languages can
// be combined with `+`, as in "eng+deu", in which case tesseract loads all of
// them for one pass.
//
// The value picks from what the image carries — the packages New installed and
// any model WithTessdata supplied — and cannot itself add a language, since
// both of those are baked in when the image is assembled. An unknown value is
// rejected at output time with the available set listed. Unset, recognition
// runs in the first language New installed.
func (d *Document) WithLanguage(lang string) *Document {
	return d.with(d.Options.withLanguage(lang))
}

// WithPageSegmentation sets how much layout analysis precedes recognition
// (`--psm`). Unset, tesseract uses fully automatic segmentation without
// orientation detection.
func (d *Document) WithPageSegmentation(mode PageSegMode) *Document {
	return d.with(d.Options.withPageSegmentation(mode))
}

// WithEngine selects the OCR engine (`--oem`). Unset, tesseract picks based on
// what the language data provides.
func (d *Document) WithEngine(mode EngineMode) *Document {
	return d.with(d.Options.withEngine(mode))
}

// WithDpi declares the source image's resolution (`--dpi`), which tesseract
// otherwise guesses from the image metadata. Guessing wrong hurts recognition
// on images that carry no resolution at all. A non-positive value is rejected
// at output time.
func (d *Document) WithDpi(dpi int) *Document {
	return d.with(d.Options.withDpi(dpi))
}

// WithParameter sets one of tesseract's control variables (`-c name=value`);
// Parameters lists every name and its default.
//
// It takes a name and a value separately rather than a map because Dagger
// functions cannot accept map parameters. An unknown name is rejected at
// output time: tesseract itself only warns and carries on, so a typo would
// otherwise silently do nothing.
func (d *Document) WithParameter(name string, value string) *Document {
	return d.with(d.Options.withParameter(name, value))
}

// WithUserWords supplies a word list (`--user-words`): one word per line,
// which recognition then favours. Useful for jargon and proper nouns the
// packaged dictionary does not know.
func (d *Document) WithUserWords(words *dagger.File) *Document {
	return d.with(d.Options.withUserWords(words))
}

// WithUserPatterns supplies a pattern list (`--user-patterns`): one pattern
// per line describing the shape of expected strings, such as part numbers.
func (d *Document) WithUserPatterns(patterns *dagger.File) *Document {
	return d.with(d.Options.withUserPatterns(patterns))
}

// Text recognises the document and returns the plain text directly, by asking
// tesseract to write to stdout instead of a file. This is the shortest path
// for the common case; Txt is the same content as a *dagger.File.
func (d *Document) Text(ctx context.Context) (string, error) {
	exec, err := d.run(ctx, stdoutBase)
	if err != nil {
		return "", err
	}
	out, err := exec.Stdout(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(out, "\n"), nil
}

// Txt recognises the document and returns the plain text as a file. It is the
// same content Text returns; take this when the next step wants a file rather
// than a string.
func (d *Document) Txt(ctx context.Context) (*dagger.File, error) {
	return d.render(ctx, FormatTxt)
}

// Hocr returns hOCR: HTML in which every recognised word carries its bounding
// box and confidence, which is what layout-aware post-processing reads.
func (d *Document) Hocr(ctx context.Context) (*dagger.File, error) {
	return d.render(ctx, FormatHocr)
}

// Alto returns ALTO XML, the layout schema libraries and archives ingest.
func (d *Document) Alto(ctx context.Context) (*dagger.File, error) {
	return d.render(ctx, FormatAlto)
}

// Tsv returns tab-separated recognition results: one row per layout element,
// descending from page to word, each with its box and confidence.
func (d *Document) Tsv(ctx context.Context) (*dagger.File, error) {
	return d.render(ctx, FormatTsv)
}

// Pdf returns a searchable PDF: the source image with an invisible text layer
// positioned behind it, so the page looks untouched but selects and greps.
func (d *Document) Pdf(ctx context.Context) (*dagger.File, error) {
	return d.render(ctx, FormatPdf)
}

// Page returns PAGE XML, the PRImA layout-analysis schema — the alternative to
// ALTO for tools built around that ecosystem.
func (d *Document) Page(ctx context.Context) (*dagger.File, error) {
	return d.render(ctx, FormatPage)
}

// Export runs one recognition pass and returns a directory holding every
// requested format.
//
// It exists alongside the single-artifact functions because tesseract accepts
// several renderers per invocation: asking for text, hOCR and PDF together is
// one pass over the image, not three. Each artifact is named `result` plus the
// renderer's own extension.
func (d *Document) Export(
	ctx context.Context,
	// Output formats to render in the single recognition pass.
	formats []Format,
) (*dagger.Directory, error) {
	configs, err := selectFormats(formats)
	if err != nil {
		return nil, err
	}
	exec, err := d.run(ctx, outputBase, configs...)
	if err != nil {
		return nil, err
	}
	return exec.Directory(outputDir), nil
}

// Osd detects the document's orientation and script without recognising any
// text (`--psm 0`), reporting the rotation needed to make the page upright.
//
// It builds its own invocation rather than reusing the document's recognition
// options: orientation detection runs off the osd model alone, so the selected
// language, engine and page-segmentation mode have nothing to say about it.
// The osd model has to be in the image: either as the package New installs for
// it, or as an osd.traineddata WithTessdata supplied.
func (d *Document) Osd(ctx context.Context) (string, error) {
	ok, err := d.Tesseract.hasModel(ctx, osdLanguage)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf(
			"Osd: the %q model is not installed: pass it to New(languages:) alongside the recognition languages, or supply it with WithTessdata (installed packages: %s)",
			osdLanguage, strings.Join(d.Tesseract.Languages, ", "))
	}
	source, err := d.validate(ctx)
	if err != nil {
		return "", err
	}
	args := []string{"tesseract", source, stdoutBase}
	args = append(args, d.Tesseract.tessdataArgs()...)
	args = append(args, "-l", osdLanguage, "--psm", pageSegTokens[PageSegModeOsdOnly])
	if d.Options.HasDpi {
		args = append(args, "--dpi", strconv.Itoa(d.Options.Dpi))
	}
	exec, err := execTesseract(ctx, d.container(source), args)
	if err != nil {
		return "", err
	}
	out, err := exec.Stdout(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(out, "\n"), nil
}

// render runs one recognition pass for a single format and lifts the artifact
// off the finished exec.
func (d *Document) render(ctx context.Context, format Format) (*dagger.File, error) {
	spec := formatTable[format]
	exec, err := d.run(ctx, outputBase, spec.config)
	if err != nil {
		return nil, err
	}
	return exec.File(outputBase + spec.ext), nil
}

// selectFormats validates the requested set and returns the CONFIGFILE words
// in a fixed order, de-duplicated so the same format twice does not render
// twice.
func selectFormats(formats []Format) ([]string, error) {
	if len(formats) == 0 {
		return nil, fmt.Errorf("Export: at least one format is required: must be one of %s", formatNames())
	}
	want := make(map[Format]struct{}, len(formats))
	for _, f := range formats {
		if _, ok := formatTable[f]; !ok {
			return nil, fmt.Errorf("Export: invalid format %q: must be one of %s", string(f), formatNames())
		}
		want[f] = struct{}{}
	}
	configs := make([]string, 0, len(want))
	for _, f := range formatOrder {
		if _, ok := want[f]; ok {
			configs = append(configs, formatTable[f].config)
		}
	}
	return configs, nil
}

// with returns a copy of the document carrying a new option set, which is what
// keeps every builder immutable.
func (d *Document) with(opts options) *Document {
	out := *d
	out.Options = opts
	return &out
}

// run validates the document, assembles the recognition container, executes
// tesseract once, and returns the finished exec so the caller can read its
// stdout or lift artifacts off its filesystem.
//
// configs are the trailing CONFIGFILE words that select renderers. They must
// come last: tesseract stops parsing flags at the first configfile, so a `-l`
// after one is read as a file name to open.
func (d *Document) run(ctx context.Context, output string, configs ...string) (*dagger.Container, error) {
	source, err := d.validate(ctx)
	if err != nil {
		return nil, err
	}
	flags, err := d.Options.flags(d.Tesseract)
	if err != nil {
		return nil, err
	}
	args := append([]string{"tesseract", source, output}, flags...)
	return execTesseract(ctx, d.container(source), append(args, configs...))
}

// execTesseract runs one tesseract invocation, turning a non-zero exit into an
// error carrying tesseract's own output. Expect=ReturnTypeAny keeps the failed
// exec on the value path so both streams stay readable.
func execTesseract(ctx context.Context, ctr *dagger.Container, args []string) (*dagger.Container, error) {
	exec := ctr.WithExec(args, dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny})
	code, err := exec.ExitCode(ctx)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("tesseract failed (exit %d):\n%s", code, combinedOutput(ctx, exec))
	}
	return exec, nil
}

// container mounts whichever shape of source the document holds, alongside any
// user word/pattern lists, and stages the writable output directory
// recognition renders into.
//
// A rasterized PDF mounts its whole page directory, at the path the page list
// names its entries by — the list holds absolute paths, so this mount and the
// one the rasterizer wrote them under have to agree.
func (d *Document) container(source string) *dagger.Container {
	ctr := d.Tesseract.Container()
	if d.Pages != nil {
		ctr = ctr.WithMountedDirectory(pdfPagesDir, d.Pages)
	} else {
		ctr = ctr.WithMountedFile(source, d.Source)
	}
	return d.Options.mount(ctr.WithExec([]string{"mkdir", "-p", outputDir}))
}

// validate reports every deferred builder check and returns the path the
// source is mounted at.
func (d *Document) validate(ctx context.Context) (string, error) {
	if err := d.validatePdfDpi(); err != nil {
		return "", err
	}
	source, err := d.sourcePath(ctx)
	if err != nil {
		return "", err
	}
	if err := d.Options.validate(ctx, d.Tesseract); err != nil {
		return "", err
	}
	return source, nil
}

// sourcePath resolves the FILE argument recognition runs against.
//
// For a rasterized PDF that is the page list rather than an image: handed a
// file it cannot identify as one, tesseract reads it as a list of image paths
// and processes them in order as a single document, which is exactly the unit
// of work a PDF is. For a single image it is the mount path, keeping the
// caller's extension so container logs name something recognisable, and PDF
// input is rejected along the way.
func (d *Document) sourcePath(ctx context.Context) (string, error) {
	if d.Pages != nil {
		return pdfPageListPath, nil
	}
	name, err := d.Source.Name(ctx)
	if err != nil {
		return "", fmt.Errorf("read source file name: %w", err)
	}
	if err := rejectPdf(name); err != nil {
		return "", err
	}
	return workDir + "/source" + path.Ext(name), nil
}

// rejectPdf refuses a PDF source up front.
//
// Leptonica — the image library tesseract reads through — has no PDF support
// at all, and says so unhelpfully: it reports the file's first line as if it
// were a file name it could not open. Rejecting the extension here is the
// difference between an actionable error and a confusing one, and now that
// FromPdf exists the fix is a function name rather than an errand.
func rejectPdf(name string) error {
	if !strings.EqualFold(path.Ext(name), ".pdf") {
		return nil
	}
	return fmt.Errorf(
		"source %q is a PDF: tesseract reads images through leptonica, which has no PDF support; call FromPdf instead, which rasterizes the pages first and returns a Document over them",
		name)
}
