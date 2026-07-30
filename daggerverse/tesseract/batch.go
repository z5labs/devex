package main

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"dagger/tesseract/internal/dagger"
)

// batchArgv0 is the `$0` the batch loop runs under. Every recognition flag and
// configfile reaches the loop as `"$@"` rather than being interpolated into the
// script, which is what keeps a parameter value with a space or a quote in it
// from needing any escaping at all.
const batchArgv0 = "tesseract-batch"

// batchScriptTemplate is the whole of the batch mechanism: one serial pass
// creating every output directory, then N concurrent passes over the same
// manifest, each recognising the records whose index it owns.
//
// The directories are created up front rather than in the recognition pass
// because several images share one: two workers creating `scans/deep` at the
// same moment is a race, and creating them all while nothing is running costs
// one mkdir per record and removes it.
//
// Round-robin over record index is what partitions the work, so a worker knows
// which records are its own without coordinating with the others — the manifest
// is read once per worker and nothing is popped from a shared queue. `xargs -P`
// would schedule better on records of uneven cost, but it answers a failed
// child with its own exit 123 and names nothing, and naming the image that
// failed is the whole of what the serial loop's `set -e` used to do for free.
//
// Each recognition's output is captured rather than streamed, so a failure
// reports as one block naming its image instead of interleaving with whatever
// the other workers are writing. tesseract's own message frequently cannot name
// it: handed a file leptonica will not decode, tesseract falls back to reading
// it as a list of image paths and reports the *contents* of the first line as
// the file it could not open.
//
// The first failure touches the stop file, which every worker checks before its
// next image, so a batch that is already going to fail stops within one image
// per worker rather than recognising the remaining thousand pages first.
//
// IFS is a literal tab, so a file name with spaces survives the read;
// checkManifestSafe rejects the tabs and newlines that would not. Every
// recognition flag reaches the workers as `"$@"`, which is why the function
// takes its slot as `$1` and shifts it off.
const batchScriptTemplate = `set -e
mkdir -p @FAILURES@

while IFS="	" read -r src out dir; do
	mkdir -p "$dir"
done < @MANIFEST@

recognise() {
	slot=$1
	shift
	index=0
	while IFS="	" read -r src out dir; do
		if [ $((index % @WORKERS@)) -eq "$slot" ]; then
			if [ -e @STOP@ ]; then
				return 1
			fi
			if ! output=$(tesseract "$src" "$out" "$@" 2>&1); then
				printf '%s:\n%s\n' "$src" "$output" >> "@FAILURES@/$slot"
				: > @STOP@
				return 1
			fi
		fi
		index=$((index + 1))
	done < @MANIFEST@
}

slot=0
pids=""
while [ "$slot" -lt @WORKERS@ ]; do
	recognise "$slot" "$@" &
	pids="$pids $!"
	slot=$((slot + 1))
done

failed=0
for pid in $pids; do
	wait "$pid" || failed=1
done

if [ "$failed" -ne 0 ]; then
	cat @FAILURES@/* >&2
	exit 1
fi`

// batchScript renders the template for a given number of workers. The
// substitutions are container paths this module owns and an integer it has
// already validated, so none of them can carry anything the shell would have to
// be protected from — unlike the recognition flags, which stay in `"$@"`.
func batchScript(workers int) string {
	return strings.NewReplacer(
		"@MANIFEST@", batchManifestPath,
		"@FAILURES@", batchFailureDir,
		"@STOP@", batchStopFile,
		"@WORKERS@", strconv.Itoa(workers),
	).Replace(batchScriptTemplate)
}

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
	Concurrency int
	// +private
	HasConcurrency bool
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

// WithConcurrency bounds how many images the batch recognises at once. The
// default is one, which is what a batch has always done.
//
// Recognition is where a batch spends its time, and it is the one thing the
// serial loop does not collapse: N independent images recognised one after
// another. Processes are how tesseract parallelises well — eleven pages on four
// CPUs take 3.37s one at a time and 1.05s eleven at a time, ~90% efficiency
// where its own OpenMP manages 33%, because those regions sit inside the LSTM
// inner loops and do not amortize.
//
// A bound above one therefore also caps OpenMP at one thread per process for
// this batch's exec, unless the caller named an explicit ompThreadLimit on New,
// which is left alone. Concurrency multiplied by per-process threads rather
// than bounded by cores is the one shape that is slower than doing nothing:
// those same four workers on an unbounded image take 3m36.8s against 1.24s
// bounded, 174x. The bound is set on the exec rather than on the image, so
// nothing else built from the same Tesseract sees it and two batches differing
// only in concurrency still share the package fetch.
//
// Somewhere around the core count is the number to pass; more processes than
// cores buys little once each is single-threaded. Non-positive is rejected at
// output time.
func (b *Batch) WithConcurrency(
	// Maximum number of images to recognise at the same time.
	concurrency int,
) *Batch {
	out := *b
	out.Concurrency = concurrency
	out.HasConcurrency = true
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
// source directory root, sorted — which is the order they are recognised in
// until WithConcurrency deals them out to several workers at once.
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
// one, while each image still gets its own output base. How many of those
// recognitions run at once inside that one exec is WithConcurrency.
func (b *Batch) Export(
	ctx context.Context,
	// Output formats to render for each image in the batch.
	formats []Format,
) (*dagger.Directory, error) {
	configs, err := selectFormats(formats)
	if err != nil {
		return nil, err
	}
	if err := b.validateConcurrency(); err != nil {
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

	args := append([]string{"sh", "-c", batchScript(b.workers()), batchArgv0}, flags...)
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
//
// A concurrent batch also bounds OpenMP here, on the exec rather than on the
// image: concurrency multiplied by per-process threads is the shape that
// collapses (see WithConcurrency), and one thread per process is the only one
// worth running. An ompThreadLimit the caller named on New is theirs and is
// left alone — it is already on the image, and overriding it would ignore the
// one instruction on the subject this module was given.
func (b *Batch) container(manifest *dagger.File) *dagger.Container {
	ctr := b.Tesseract.Container().
		WithMountedDirectory(batchSourceDir, b.Source).
		WithMountedFile(batchManifestPath, manifest)
	if b.workers() > 1 && b.Tesseract.OmpThreadLimit == hostCpuOmpThreadLimit {
		ctr = ctr.WithEnvVariable(ompThreadLimitEnv, strconv.Itoa(concurrentOmpThreadLimit))
	}
	return b.Options.mount(ctr)
}

// workers is how many tesseract processes one Export runs at once: whatever
// WithConcurrency asked for, or one.
func (b *Batch) workers() int {
	if !b.HasConcurrency {
		return defaultBatchConcurrency
	}
	return b.Concurrency
}

// validateConcurrency rejects a bound that would run no images at all. It is
// deferred to output time rather than reported from the builder because a
// builder has no error return, which is the same reason WithDpi's check lives
// away from WithDpi.
func (b *Batch) validateConcurrency() error {
	if b.HasConcurrency && b.Concurrency < defaultBatchConcurrency {
		return fmt.Errorf(
			"WithConcurrency: concurrency must be positive, got %d: pass 1 to recognise one image at a time, which is what leaving it unset does",
			b.Concurrency)
	}
	return nil
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
			return nil, fmt.Errorf("Batch: %w: rename it, or recognise it on its own with Document", err)
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
// unambiguously. Spaces are fine — the loops split on tabs alone — but a tab or
// a newline in a file name would be read as a field or record boundary and act
// on the wrong file. Batch and Training share the manifest shape and therefore
// share the check; each adds its own way out of it.
func checkManifestSafe(p string) error {
	if strings.ContainsAny(p, "\t\n") {
		return fmt.Errorf("file name %q contains a tab or newline, which the manifest cannot represent", p)
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
