// Package main implements the tesseract Dagger module: optical character
// recognition as a `dagger call` instead of the usual hand-rolled Dockerfile
// plus rescue-the-output shell script. Hand it an image and get back plain
// text, or hOCR / ALTO / TSV / PAGE / searchable PDF for anything that needs
// word positions and confidences.
//
// There is no official Tesseract container image — upstream ships source only
// — so this module assembles its own the way the qemu module does: a
// module-pinned Alpine plus `apk add tesseract-ocr`, with only the registry
// prefix caller-overridable for air-gapped mirrors.
//
// The language set lives on the root object rather than on Document because on
// Alpine each language is a separate apk package (`tesseract-ocr-data-<lang>`,
// none of which the base package pulls in). Selecting a language changes what
// the image *is*, not just what a flag says; Document.WithLanguage only picks a
// subset of what was installed.
//
// File map (all `package main`, surfaced as one Dagger module):
//
//   - enums.go     — PageSegMode / EngineMode / Format enums plus the token and
//     output-extension tables that map them onto the CLI.
//   - document.go  — *Document, its immutable With* builders, the deferred
//     validation, the argument builder, and every output.
package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"dagger/tesseract/internal/dagger"
)

const (
	// alpineImagePath is the repository under the configured registry. Only
	// the registry prefix is caller-overridable (air-gapped mirrors); the
	// path is fixed so every recognition runs on a known toolchain.
	alpineImagePath = "library/alpine"
	defaultRegistry = "docker.io"

	// defaultAlpineTag pins the base image. Alpine v3.24's community
	// repository carries tesseract-ocr 5.5.2, whose language data is the
	// combined tesseract-ocr/tessdata models — legacy and LSTM both.
	defaultAlpineTag = "3.24"

	// tesseractPkg is the apk package holding the binary; langPkgPrefix plus
	// a language code names the package holding that language's traineddata.
	tesseractPkg  = "tesseract-ocr"
	langPkgPrefix = "tesseract-ocr-data-"

	// defaultLanguage is what New installs when the caller names none, and
	// mirrors tesseract's own default for `-l`.
	defaultLanguage = "eng"

	// osdLanguage is the orientation-and-script-detection model. It is not a
	// recognition language: Osd needs it, and nothing else does.
	osdLanguage = "osd"

	// workDir holds the mounted source and the optional user word/pattern
	// lists; outputDir is the writable staging area recognition writes into.
	workDir   = "/work"
	outputDir = "/out"

	// outputBase is the `tesseract IMAGE OUTPUTBASE` argument. Each renderer
	// appends its own extension to it (see Format.ext).
	outputBase = outputDir + "/result"

	// stdoutBase is the OUTPUTBASE that makes tesseract write to stdout
	// instead of a file.
	stdoutBase = "-"

	userWordsPath    = workDir + "/user-words.txt"
	userPatternsPath = workDir + "/user-patterns.txt"
)

// Tesseract is the root namespace for every exported function in this module.
// It carries the image coordinates and the language set the image is built
// with; Document hangs off it so the generated SDK surfaces recognition under
// `dag.Tesseract().Document(...)`.
type Tesseract struct {
	// +private
	Registry string
	// +private
	AlpineTag string
	// +private
	Languages []string
}

// New returns a Tesseract module backed by <registry>/library/alpine:<tag>
// with tesseract-ocr and one language package per requested language.
func New(
	// Container registry hosting the alpine image.
	// +default="docker.io"
	registry string,
	// Tag of the alpine image the toolchain is assembled on.
	// +default="3.24"
	alpineTag string,
	// Language codes to install, one apk package each. Empty installs "eng".
	// "osd" is not a recognition language but is required by Document.Osd.
	// +optional
	languages []string,
) *Tesseract {
	if registry == "" {
		registry = defaultRegistry
	}
	if alpineTag == "" {
		alpineTag = defaultAlpineTag
	}
	return &Tesseract{
		Registry:  registry,
		AlpineTag: alpineTag,
		Languages: normalizeLanguages(languages),
	}
}

// Container returns the assembled toolchain image. This is the escape hatch
// for everything this module does not wrap — the training binaries the apk
// package ships, `combine_tessdata`, and tesseract's long tail of renderers
// stay reachable via `container with-exec`.
//
// +cache="session"
func (t *Tesseract) Container() *dagger.Container {
	args := []string{"apk", "add", "--no-cache", tesseractPkg}
	for _, lang := range t.Languages {
		args = append(args, langPkgPrefix+lang)
	}
	return dag.Container().
		From(t.image()).
		WithExec(args)
}

// Version returns the tesseract release the assembled image ships, as the
// bare version number reported by `tesseract --version`.
//
// +cache="session"
func (t *Tesseract) Version(ctx context.Context) (string, error) {
	out, err := t.Container().
		WithExec([]string{"tesseract", "--version"}).
		Stdout(ctx)
	if err != nil {
		return "", err
	}
	// The first line is `tesseract <version>`; the rest reports leptonica,
	// the image libraries and the SIMD features the build found.
	first := strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
	return strings.TrimSpace(strings.TrimPrefix(first, "tesseract")), nil
}

// Langs returns the language codes installed in the image, as reported by
// `tesseract --list-langs`. This is the set Document.WithLanguage validates
// against, and it includes "osd" when that package was requested.
//
// +cache="session"
func (t *Tesseract) Langs(ctx context.Context) ([]string, error) {
	out, err := t.Container().
		WithExec([]string{"tesseract", "--list-langs"}).
		Stdout(ctx)
	if err != nil {
		return nil, err
	}
	return parseLangs(out), nil
}

// Parameters returns tesseract's control-variable table — every name, its
// default value and a one-line description — as `tesseract --print-parameters`
// prints it. These are the names Document.WithParameter accepts.
//
// +cache="session"
func (t *Tesseract) Parameters(ctx context.Context) (string, error) {
	out, err := t.Container().
		WithExec([]string{"tesseract", "--print-parameters"}).
		Stdout(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(out, "\n"), nil
}

// Document binds one image to the toolchain.
//
// The boundary input is a *dagger.File rather than a *dagger.Directory, unlike
// the kicad module's Project: tesseract resolves nothing relative to its input,
// so one image — including a multi-page TIFF — is the whole unit of work.
func (t *Tesseract) Document(source *dagger.File) *Document {
	return &Document{Tesseract: t, Source: source}
}

func (t *Tesseract) image() string {
	return fmt.Sprintf("%s/%s:%s", t.Registry, alpineImagePath, t.AlpineTag)
}

// hasLanguage reports whether a language code was installed into the image.
// It reads the requested set rather than `--list-langs` so it stays a pure
// function; the exec-backed check is Langs.
func (t *Tesseract) hasLanguage(lang string) bool {
	for _, l := range t.Languages {
		if l == lang {
			return true
		}
	}
	return false
}

// normalizeLanguages trims, drops empties, de-duplicates and sorts the
// requested set, defaulting to English. Sorting matters beyond tidiness: the
// language list becomes the `apk add` argv, so an unstable order would build a
// differently-cached image for the same request.
func normalizeLanguages(languages []string) []string {
	seen := make(map[string]struct{}, len(languages))
	out := make([]string, 0, len(languages))
	for _, lang := range languages {
		lang = strings.TrimSpace(lang)
		if lang == "" {
			continue
		}
		if _, dup := seen[lang]; dup {
			continue
		}
		seen[lang] = struct{}{}
		out = append(out, lang)
	}
	if len(out) == 0 {
		return []string{defaultLanguage}
	}
	sort.Strings(out)
	return out
}

// parseLangs pulls the language codes out of `--list-langs` output, which
// leads with a `List of available languages in "..." (N):` header line.
func parseLangs(out string) []string {
	var langs []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "List of available languages") {
			continue
		}
		langs = append(langs, line)
	}
	sort.Strings(langs)
	return langs
}

// combinedOutput joins a finished exec's stdout and stderr. tesseract splits
// usage errors onto stderr and progress onto stdout, so an error message built
// from either stream alone drops half of what went wrong.
func combinedOutput(ctx context.Context, exec *dagger.Container) string {
	stdout, _ := exec.Stdout(ctx)
	stderr, _ := exec.Stderr(ctx)
	return strings.TrimSpace(strings.TrimSpace(stdout) + "\n" + strings.TrimSpace(stderr))
}
