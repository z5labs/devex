package main

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

	"dagger/tesseract/internal/dagger"
)

// Document is one image plus the recognition options that apply to it. It is
// immutable: every With* returns a copy, so a configured Document can be
// branched into several outputs without the branches interfering.
//
// Options are validated at output time rather than in the builders, because a
// builder has no error return and some checks (is this language installed? is
// this a real control variable?) need the image itself to answer.
type Document struct {
	// +private
	Tesseract *Tesseract
	// +private
	Source *dagger.File
	// +private
	Language string
	// +private
	PageSeg PageSegMode
	// +private
	Engine EngineMode
	// +private
	Dpi int
	// +private
	HasDpi bool
	// +private
	ParamNames []string
	// +private
	ParamValues []string
	// +private
	UserWords *dagger.File
	// +private
	UserPatterns *dagger.File
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
	out := d.clone()
	out.Language = lang
	return out
}

// WithPageSegmentation sets how much layout analysis precedes recognition
// (`--psm`). Unset, tesseract uses fully automatic segmentation without
// orientation detection.
func (d *Document) WithPageSegmentation(mode PageSegMode) *Document {
	out := d.clone()
	out.PageSeg = mode
	return out
}

// WithEngine selects the OCR engine (`--oem`). Unset, tesseract picks based on
// what the language data provides.
func (d *Document) WithEngine(mode EngineMode) *Document {
	out := d.clone()
	out.Engine = mode
	return out
}

// WithDpi declares the source image's resolution (`--dpi`), which tesseract
// otherwise guesses from the image metadata. Guessing wrong hurts recognition
// on images that carry no resolution at all. A non-positive value is rejected
// at output time.
func (d *Document) WithDpi(dpi int) *Document {
	out := d.clone()
	out.Dpi = dpi
	out.HasDpi = true
	return out
}

// WithParameter sets one of tesseract's control variables (`-c name=value`);
// Parameters lists every name and its default.
//
// It takes a name and a value separately rather than a map because Dagger
// functions cannot accept map parameters. An unknown name is rejected at
// output time: tesseract itself only warns and carries on, so a typo would
// otherwise silently do nothing.
func (d *Document) WithParameter(name string, value string) *Document {
	out := d.clone()
	out.ParamNames = append(out.ParamNames, name)
	out.ParamValues = append(out.ParamValues, value)
	return out
}

// WithUserWords supplies a word list (`--user-words`): one word per line,
// which recognition then favours. Useful for jargon and proper nouns the
// packaged dictionary does not know.
func (d *Document) WithUserWords(words *dagger.File) *Document {
	out := d.clone()
	out.UserWords = words
	return out
}

// WithUserPatterns supplies a pattern list (`--user-patterns`): one pattern
// per line describing the shape of expected strings, such as part numbers.
func (d *Document) WithUserPatterns(patterns *dagger.File) *Document {
	out := d.clone()
	out.UserPatterns = patterns
	return out
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
	if d.HasDpi {
		args = append(args, "--dpi", strconv.Itoa(d.Dpi))
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

func (d *Document) clone() *Document {
	out := *d
	out.ParamNames = append([]string(nil), d.ParamNames...)
	out.ParamValues = append([]string(nil), d.ParamValues...)
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
	args, err := d.args(source, output, configs...)
	if err != nil {
		return nil, err
	}
	return execTesseract(ctx, d.container(source), args)
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

// container mounts the source and any user word/pattern lists, and stages the
// writable output directory recognition renders into.
func (d *Document) container(source string) *dagger.Container {
	ctr := d.Tesseract.Container().
		WithMountedFile(source, d.Source).
		WithExec([]string{"mkdir", "-p", outputDir})
	if d.UserWords != nil {
		ctr = ctr.WithMountedFile(userWordsPath, d.UserWords)
	}
	if d.UserPatterns != nil {
		ctr = ctr.WithMountedFile(userPatternsPath, d.UserPatterns)
	}
	return ctr
}

// args renders the tesseract argv. Everything except the trailing configfiles
// is a flag, and tesseract requires the flags first.
func (d *Document) args(source string, output string, configs ...string) ([]string, error) {
	args := []string{"tesseract", source, output}
	args = append(args, d.Tesseract.tessdataArgs()...)
	args = append(args, "-l", d.language())
	if d.PageSeg != "" {
		tok, ok := d.PageSeg.token()
		if !ok {
			return nil, fmt.Errorf("WithPageSegmentation: unknown mode %q", string(d.PageSeg))
		}
		args = append(args, "--psm", tok)
	}
	if d.Engine != "" {
		tok, ok := d.Engine.token()
		if !ok {
			return nil, fmt.Errorf("WithEngine: unknown mode %q", string(d.Engine))
		}
		args = append(args, "--oem", tok)
	}
	if d.HasDpi {
		args = append(args, "--dpi", strconv.Itoa(d.Dpi))
	}
	if d.UserWords != nil {
		args = append(args, "--user-words", userWordsPath)
	}
	if d.UserPatterns != nil {
		args = append(args, "--user-patterns", userPatternsPath)
	}
	for i, name := range d.ParamNames {
		args = append(args, "-c", name+"="+d.ParamValues[i])
	}
	return append(args, configs...), nil
}

// language resolves the `-l` value: the caller's selection, or the first
// installed language. Defaulting to the installed set rather than omitting the
// flag matters when English was not installed — tesseract's own default is
// "eng", which would fail to load.
func (d *Document) language() string {
	if strings.TrimSpace(d.Language) != "" {
		return d.Language
	}
	return d.Tesseract.Languages[0]
}

// validate reports every deferred builder check and returns the path the
// source is mounted at.
func (d *Document) validate(ctx context.Context) (string, error) {
	source, err := d.sourcePath(ctx)
	if err != nil {
		return "", err
	}
	if err := d.validateDpi(); err != nil {
		return "", err
	}
	if err := d.validateLanguage(ctx); err != nil {
		return "", err
	}
	if err := d.validateParameters(ctx); err != nil {
		return "", err
	}
	return source, nil
}

// sourcePath resolves where the source is mounted, keeping the caller's
// extension so container logs name something recognisable, and rejects PDF
// input along the way.
//
// Leptonica — the image library tesseract reads through — has no PDF support
// at all, and says so unhelpfully: it reports the file's first line as if it
// were a file name it could not open. Rejecting the extension up front is the
// difference between an actionable error and a confusing one.
func (d *Document) sourcePath(ctx context.Context) (string, error) {
	name, err := d.Source.Name(ctx)
	if err != nil {
		return "", fmt.Errorf("read source file name: %w", err)
	}
	if strings.EqualFold(path.Ext(name), ".pdf") {
		return "", fmt.Errorf(
			"source %q is a PDF: tesseract reads images through leptonica, which has no PDF support; rasterize the pages to PNG or TIFF first",
			name)
	}
	return workDir + "/source" + path.Ext(name), nil
}

// validateDpi rejects a non-positive `--dpi`, which tesseract would take as a
// real resolution and scale its analysis by.
func (d *Document) validateDpi() error {
	if d.HasDpi && d.Dpi <= 0 {
		return fmt.Errorf("WithDpi: dpi must be positive, got %d", d.Dpi)
	}
	return nil
}

// validateLanguage checks every `+`-joined code against what the image
// actually carries. tesseract does fail on an unknown language, but its error
// talks about traineddata paths and TESSDATA_PREFIX rather than about the
// language set this module built the image with.
func (d *Document) validateLanguage(ctx context.Context) error {
	if strings.TrimSpace(d.Language) == "" {
		return nil
	}
	installed, err := d.Tesseract.Langs(ctx)
	if err != nil {
		return err
	}
	for _, lang := range strings.Split(d.Language, "+") {
		lang = strings.TrimSpace(lang)
		if lang == "" {
			return fmt.Errorf(
				"WithLanguage: empty language code in %q: installed languages are %s",
				d.Language, strings.Join(installed, ", "))
		}
		if !contains(installed, lang) {
			return fmt.Errorf(
				"WithLanguage: language %q is not installed: installed languages are %s; pass it to New(languages:) to add its package, or supply its model with WithTessdata",
				lang, strings.Join(installed, ", "))
		}
	}
	return nil
}

// validateParameters rejects a malformed or unknown control-variable name.
//
// The `=` and empty-name checks matter because `-c` takes `name=value`: an
// embedded `=` would silently set a different variable to a different value.
// The unknown-name check matters because tesseract only prints
// `Warning: The parameter '...' was not found.` and exits 0, so a typo would
// otherwise be indistinguishable from a setting that had no effect.
func (d *Document) validateParameters(ctx context.Context) error {
	for _, name := range d.ParamNames {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("WithParameter: parameter name is required")
		}
		if strings.Contains(name, "=") {
			return fmt.Errorf("WithParameter: parameter name %q must not contain %q", name, "=")
		}
	}
	if len(d.ParamNames) == 0 {
		return nil
	}
	known, err := d.Tesseract.Parameters(ctx)
	if err != nil {
		return err
	}
	names := parseParameterNames(known)
	for _, name := range d.ParamNames {
		if !contains(names, name) {
			return fmt.Errorf(
				"WithParameter: unknown parameter %q: tesseract reports %d control variables and this is not one of them; call Parameters() for the full list",
				name, len(names))
		}
	}
	return nil
}

// parseParameterNames pulls the names out of `--print-parameters`, whose rows
// are `name<TAB>default<TAB>description` under a one-line header.
func parseParameterNames(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		name, _, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
