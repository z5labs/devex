package main

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"dagger/tesseract/internal/dagger"
)

// batchArgv0 is the `$0` the batch loop runs under. Every recognition flag and
// configfile reaches the loop as `"$@"` rather than being interpolated into the
// script, which is what keeps a parameter value with a space or a quote in it
// from needing any escaping at all.
const batchArgv0 = "tesseract-batch"

// batchScript is the whole of the batch mechanism: read one record per image,
// create its output directory, recognise it.
//
// `set -e` aborts on the first failure, so a page tesseract cannot read fails
// the run carrying its own message instead of going quietly missing from the
// results. IFS is a literal tab, so a file name with spaces survives the read;
// checkManifestSafe rejects the tabs and newlines that would not.
const batchScript = `set -e
while IFS="` + "\t" + `" read -r src out dir; do
	mkdir -p "$dir"
	tesseract "$src" "$out" "$@"
done < ` + batchManifestPath

// imageExtensions is what the default glob accepts, lower-cased for a
// case-insensitive comparison. It is the set this image can actually decode:
// `tesseract --version` reports leptonica linked against libgif, libjpeg,
// libpng, libtiff and libwebp, and leptonica reads BMP and the PNM family
// itself.
//
// PDF is absent because leptonica cannot read it at all, which is the same
// reason Document rejects a PDF source outright.
var imageExtensions = []string{
	".bmp", ".gif", ".jpe", ".jpeg", ".jpg", ".pbm",
	".pgm", ".pnm", ".png", ".ppm", ".tif", ".tiff", ".webp",
}

// Batch is a directory of images plus the recognition options that apply to
// all of them. It carries the same options type Document does, so the two
// cannot drift: every With* here forwards to the shared builder.
type Batch struct {
	// +private
	Tesseract *Tesseract
	// +private
	Source *dagger.Directory
	// +private
	Glob string
	// +private
	Options options
}

// WithGlob replaces the set of files to recognise with everything matching one
// glob pattern, resolved against the directory root. `**` crosses directory
// boundaries, so `**/*.tif` reaches nested scans and `receipts/*.png` stays in
// one folder.
//
// Setting a pattern also takes over the extension filtering the default does.
// That is deliberate: a caller who names the pattern knows what is in the
// directory, and leptonica sniffs content rather than trusting extensions, so
// `**/*.scan` is a reasonable thing to ask for. PDFs stay rejected either way,
// because leptonica genuinely cannot read them.
func (b *Batch) WithGlob(pattern string) *Batch {
	out := *b
	out.Glob = pattern
	return &out
}

// WithLanguage selects the recognition language (`-l`) for every image in the
// batch. See Document.WithLanguage.
func (b *Batch) WithLanguage(lang string) *Batch {
	return b.with(b.Options.withLanguage(lang))
}

// WithPageSegmentation sets how much layout analysis precedes recognition
// (`--psm`) for every image in the batch. See Document.WithPageSegmentation.
func (b *Batch) WithPageSegmentation(mode PageSegMode) *Batch {
	return b.with(b.Options.withPageSegmentation(mode))
}

// WithEngine selects the OCR engine (`--oem`) for every image in the batch.
// See Document.WithEngine.
func (b *Batch) WithEngine(mode EngineMode) *Batch {
	return b.with(b.Options.withEngine(mode))
}

// WithDpi declares the source resolution (`--dpi`) for every image in the
// batch. See Document.WithDpi.
func (b *Batch) WithDpi(dpi int) *Batch {
	return b.with(b.Options.withDpi(dpi))
}

// WithParameter sets one of tesseract's control variables (`-c name=value`)
// for every image in the batch. See Document.WithParameter.
func (b *Batch) WithParameter(name string, value string) *Batch {
	return b.with(b.Options.withParameter(name, value))
}

// WithUserWords supplies a word list (`--user-words`) for every image in the
// batch. See Document.WithUserWords.
func (b *Batch) WithUserWords(words *dagger.File) *Batch {
	return b.with(b.Options.withUserWords(words))
}

// WithUserPatterns supplies a pattern list (`--user-patterns`) for every image
// in the batch. See Document.WithUserPatterns.
func (b *Batch) WithUserPatterns(patterns *dagger.File) *Batch {
	return b.with(b.Options.withUserPatterns(patterns))
}

// Files lists the images the batch will recognise, as paths relative to the
// source directory root, in the order they are processed.
//
// It answers the question a glob always raises — did that pattern pick up what
// I meant? — without paying for the recognition, and it fails on an empty match
// exactly as Export does.
func (b *Batch) Files(ctx context.Context) ([]string, error) {
	return b.matches(ctx)
}

// Export recognises every matched image and returns a directory mirroring the
// input layout, with one artifact per requested format per image: `scans/a.png`
// becomes `scans/a.txt` alongside `scans/a.pdf`.
//
// The whole batch is one container exec. tesseract's own list-file mode — a
// text file of image paths as the FILE argument — is not what does that here,
// because it treats the list as one multi-page *document*: it renders a single
// concatenated artifact set (one .txt with form-feed page breaks, one
// multi-page PDF) and offers no way to get the per-image files this returns.
// So the exec loops over the manifest instead, which keeps the expensive part
// — a container exec, its mounts and its cache lookup, per page — collapsed to
// one, while each image still gets its own output base.
func (b *Batch) Export(
	ctx context.Context,
	// Output formats to render for each image in the batch.
	formats []Format,
) (*dagger.Directory, error) {
	configs, err := selectFormats(formats)
	if err != nil {
		return nil, err
	}
	sources, err := b.matches(ctx)
	if err != nil {
		return nil, err
	}
	if err := b.Options.validate(ctx, b.Tesseract); err != nil {
		return nil, err
	}
	manifest, err := batchManifest(sources)
	if err != nil {
		return nil, err
	}
	flags, err := b.Options.flags(b.Tesseract)
	if err != nil {
		return nil, err
	}

	args := append([]string{"sh", "-c", batchScript, batchArgv0}, flags...)
	exec, err := execTesseract(ctx, b.container(manifest), append(args, configs...))
	if err != nil {
		return nil, err
	}
	return exec.Directory(outputDir), nil
}

// with returns a copy of the batch carrying a new option set, which is what
// keeps every builder immutable.
func (b *Batch) with(opts options) *Batch {
	out := *b
	out.Options = opts
	return &out
}

// container mounts the source directory and the manifest the exec loops over,
// alongside whatever the options bring with them.
func (b *Batch) container(manifest *dagger.File) *dagger.Container {
	return b.Options.mount(b.Tesseract.Container().
		WithMountedDirectory(batchSourceDir, b.Source).
		WithMountedFile(batchManifestPath, manifest))
}

// matches resolves the glob into the sorted list of images to recognise, and
// reports the reasons a batch cannot run: nothing matched, a PDF matched, or
// two images would render onto the same output base.
func (b *Batch) matches(ctx context.Context) ([]string, error) {
	pattern := b.Glob
	if pattern == "" {
		pattern = defaultBatchGlob
	}
	found, err := b.Source.Glob(ctx, pattern)
	if err != nil {
		return nil, fmt.Errorf("Batch: glob %q: %w", pattern, err)
	}

	var (
		sources  []string
		filtered int
	)
	for _, p := range found {
		// Glob reports directories too, with a trailing separator. They are
		// not inputs, and handing one to tesseract is an error rather than a
		// no-op.
		if strings.HasSuffix(p, "/") {
			continue
		}
		if b.Glob == "" && !isImagePath(p) {
			filtered++
			continue
		}
		if err := rejectPdf(p); err != nil {
			return nil, fmt.Errorf("Batch: %w", err)
		}
		if err := checkManifestSafe(p); err != nil {
			return nil, err
		}
		sources = append(sources, p)
	}
	sort.Strings(sources)

	if len(sources) == 0 {
		return nil, emptyMatchError(b.Glob, pattern, filtered)
	}
	if err := checkOutputCollisions(sources); err != nil {
		return nil, err
	}
	return sources, nil
}

// emptyMatchError explains an empty match in terms of whichever filter was
// responsible, so the fix named is the one that applies. An empty directory
// returned instead would look like a batch that succeeded and found no text.
func emptyMatchError(glob string, pattern string, filtered int) error {
	if glob != "" {
		return fmt.Errorf(
			"Batch: glob %q matched no files in the source directory: check the pattern against the directory layout (%q crosses directory boundaries, %q does not)",
			pattern, "**", "*")
	}
	if filtered > 0 {
		return fmt.Errorf(
			"Batch: the source directory holds no images: %d file(s) were skipped because the default glob takes only %s; pass WithGlob to recognise other extensions",
			filtered, strings.Join(imageExtensions, " "))
	}
	return fmt.Errorf("Batch: the source directory is empty")
}

// checkOutputCollisions rejects two inputs that would render onto the same
// output base, which is what `a.png` and `a.tif` in one folder do: both become
// `a.txt`, and the second silently overwrites the first.
func checkOutputCollisions(sources []string) error {
	seen := make(map[string]string, len(sources))
	for _, p := range sources {
		base := outputBaseFor(p)
		if first, dup := seen[base]; dup {
			return fmt.Errorf(
				"Batch: %q and %q both render onto %q: rename one, or narrow the glob so only one of them matches",
				first, p, base)
		}
		seen[base] = p
	}
	return nil
}

// checkManifestSafe rejects a path the tab-separated manifest cannot carry
// unambiguously. Spaces are fine — the exec splits on tabs alone — but a tab or
// a newline in a file name would be read as a field or record boundary and
// recognise the wrong file.
func checkManifestSafe(p string) error {
	if strings.ContainsAny(p, "\t\n") {
		return fmt.Errorf("Batch: file name %q contains a tab or newline, which the batch manifest cannot represent: rename it, or recognise it on its own with Document", p)
	}
	return nil
}

// batchManifest renders the file the exec loops over: one record per image,
// holding the source path, the output base and the directory to create for it.
// All three are absolute container paths, resolved here rather than in the
// shell so the loop needs no path arithmetic of its own.
func batchManifest(sources []string) (*dagger.File, error) {
	var sb strings.Builder
	for _, p := range sources {
		out := outputDir + "/" + outputBaseFor(p)
		fmt.Fprintf(&sb, "%s\t%s\t%s\n", batchSourceDir+"/"+p, out, path.Dir(out))
	}
	name := path.Base(batchManifestPath)
	return dag.Directory().WithNewFile(name, sb.String()).File(name), nil
}

// outputBaseFor maps an input path onto the output base that mirrors it, by
// dropping the extension each renderer then replaces with its own.
func outputBaseFor(p string) string {
	return strings.TrimSuffix(p, path.Ext(p))
}

// isImagePath reports whether a path carries one of the extensions the default
// glob accepts. The comparison is case-insensitive because scanner software
// writes `.JPG` as readily as `.jpg`.
func isImagePath(p string) bool {
	return contains(imageExtensions, strings.ToLower(path.Ext(p)))
}
