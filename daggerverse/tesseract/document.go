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
// The unit is one image, and that is the whole of it: tesseract resolves
// nothing relative to its input, so a file is the natural boundary. A folder of
// them is Batch, which is built out of these rather than beside them — one
// Document per image — so everything below is what a batch runs too.
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

// Box returns the character-level box file: one row per recognised character,
// giving the character and the box it was found in.
//
// It is the format tesseract's own training tooling corrects by hand — read
// the boxes, fix the characters the model got wrong, feed them back — and the
// most direct way to see where recognition thinks each glyph is. Hocr and Tsv
// carry boxes too, but at the word level and wrapped in a document format.
func (d *Document) Box(ctx context.Context) (*dagger.File, error) {
	return d.renderSpec(ctx, boxSpec)
}

// ProcessedImages returns the image tesseract actually recognised, which is
// not the one it was given: recognition runs on a binarized, deskewed
// derivative, and this is that derivative as a TIFF.
//
// It answers the question a disappointing result always raises — is the model
// wrong, or did the page never survive thresholding? A scan that comes back as
// a field of black has failed before recognition started, and no amount of
// tuning `--psm` will fix it.
func (d *Document) ProcessedImages(ctx context.Context) (*dagger.File, error) {
	return d.renderSpec(ctx, processedImagesSpec)
}

// LstmTrain returns one LSTM training sample — a `.lstmf` — pairing this
// image with the line of text it renders.
//
// It is the unit Training is built out of, exposed on its own for the pipeline
// that wants to build its samples somewhere else: generate them here, keep
// them, and hand the collection to `lstmtraining` on its own terms. Training
// is the shorter path when the whole job is fine-tuning a model.
//
// The ground truth is an argument rather than a file beside the image because
// a Document is one image, not a directory: there is nowhere for a `.gt.txt`
// to sit. It has to be a single line, and the image has to be a single line of
// text, for the same reason Training says so — the sample claims the whole
// image renders exactly this text.
func (d *Document) LstmTrain(
	ctx context.Context,
	// The single line of text this image renders.
	groundTruth string,
) (*dagger.File, error) {
	source, err := d.validate(ctx)
	if err != nil {
		return nil, err
	}
	box, err := groundTruthBox(ctx, d.Source, groundTruth)
	if err != nil {
		return nil, err
	}
	flags, err := d.Options.flags(d.Tesseract)
	if err != nil {
		return nil, err
	}
	// tesseract resolves a training box relative to the image it was handed
	// rather than relative to the output base, so the box is mounted beside
	// the source under the source's own name.
	ctr := d.container(source).WithMountedFile(outputBaseFor(source)+boxExt, box)
	args := append([]string{"tesseract", source, outputBase}, flags...)
	exec, err := execTesseract(ctx, ctr, append(args, lstmTrainSpec.config))
	if err != nil {
		return nil, err
	}
	return exec.File(outputBase + lstmTrainSpec.ext), nil
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
	return d.renderSpec(ctx, formatTable[format])
}

// renderSpec is render over a renderer that is not a Format member, which is
// what the training-adjacent outputs are.
func (d *Document) renderSpec(ctx context.Context, spec formatSpec) (*dagger.File, error) {
	exec, err := d.run(ctx, outputBase, spec.config)
	if err != nil {
		return nil, err
	}
	return exec.File(outputBase + spec.ext), nil
}

// selectedFormats validates the requested set and returns it in a fixed order,
// de-duplicated so the same format twice does not render twice. Batch takes the
// formats themselves, because it names each artifact after the image that
// produced it and therefore needs the extensions as well as the configfiles.
func selectedFormats(formats []Format) ([]Format, error) {
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
	selected := make([]Format, 0, len(want))
	for _, f := range formatOrder {
		if _, ok := want[f]; ok {
			selected = append(selected, f)
		}
	}
	return selected, nil
}

// formatConfigs is the CONFIGFILE word each format is selected by, which is
// what recognition takes as its trailing arguments.
func formatConfigs(formats []Format) []string {
	configs := make([]string, 0, len(formats))
	for _, f := range formats {
		configs = append(configs, formatTable[f].config)
	}
	return configs
}

// selectFormats validates the requested set and returns the CONFIGFILE words
// in a fixed order.
func selectFormats(formats []Format) ([]string, error) {
	selected, err := selectedFormats(formats)
	if err != nil {
		return nil, err
	}
	return formatConfigs(selected), nil
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
// error carrying tesseract's own output.
func execTesseract(ctx context.Context, ctr *dagger.Container, args []string) (*dagger.Container, error) {
	return execTool(ctx, ctr, args, "tesseract")
}

// execTool runs one exec and reports a non-zero exit as an error naming the
// tool that failed and carrying its own output. Expect=ReturnTypeAny keeps the
// failed exec on the value path so both streams stay readable.
func execTool(ctx context.Context, ctr *dagger.Container, args []string, tool string) (*dagger.Container, error) {
	exec := ctr.WithExec(args, dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny})
	code, err := exec.ExitCode(ctx)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("%s failed (exit %d):\n%s", tool, code, combinedOutput(ctx, exec))
	}
	return exec, nil
}

// container mounts whichever shape of source the document holds, alongside any
// user word/pattern lists, and stages the writable output directory
// recognition renders into.
//
// The output directory arrives as an empty directory rather than as a `mkdir`
// exec. tesseract will not create it and the difference is one exec per
// recognition — invisible on one document, and doubled work on a batch, which
// runs one of these per image.
func (d *Document) container(source string) *dagger.Container {
	ctr := d.Tesseract.Container().WithMountedFile(source, d.Source)
	return d.Options.mount(ctr.WithDirectory(outputDir, dag.Directory()))
}

// validate reports every deferred builder check and returns the path the
// source is mounted at.
func (d *Document) validate(ctx context.Context) (string, error) {
	source, err := d.sourcePath(ctx)
	if err != nil {
		return "", err
	}
	if err := d.Options.validate(ctx, d.Tesseract); err != nil {
		return "", err
	}
	return source, nil
}

// sourcePath resolves the FILE argument recognition runs against: the mount
// path, keeping the caller's extension so container logs name something
// recognisable. PDF input is rejected along the way.
func (d *Document) sourcePath(ctx context.Context) (string, error) {
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
// difference between an actionable error and a confusing one.
//
// Rendering a PDF is the pdf module's job rather than this one's, so the
// message names the two calls that fix it rather than leaving "render this
// first" as an errand. It names Batch and not Document because a rendered PDF
// is a directory of pages: one call per page would be the caller reimplementing
// the fan-out Batch already does.
func rejectPdf(name string) error {
	if !strings.EqualFold(path.Ext(name), ".pdf") {
		return nil
	}
	return fmt.Errorf(
		"source %q is a PDF: tesseract reads images through leptonica, which has no PDF support; render its pages with the pdf module — Document(source).Convert().WithDpi(300).Png() — and hand that directory to Batch",
		name)
}
