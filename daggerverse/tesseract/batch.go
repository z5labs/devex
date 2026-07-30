package main

import (
	"context"
	"fmt"
	"path"
	"runtime"
	"sort"
	"strings"
	"sync"

	"dagger/tesseract/internal/dagger"
)

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
//
// It is also built out of Document rather than merely resembling one. Each
// matched image is recognised as its own Document — its own exec, its own
// mounts, its own cache entry — and what runs several of them at a time is Go,
// not a runner inside a container. That is what keeps the scheduling, the
// bound and the failure reporting in a language with types and a debugger,
// instead of in a shell script mounted into an image.
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

// WithConcurrency bounds how many images the batch recognises at once.
//
// It defaults to the number of CPUs the module can see, which is what a batch
// wants: every image is its own exec, and recognising them one at a time would
// pay that overhead N times for none of the parallelism it buys. Processes are
// how tesseract parallelises well — eleven pages on four CPUs take 3.37s one at
// a time and 1.05s eleven at a time, ~90% efficiency where its own OpenMP
// manages 33%, because those regions sit inside the LSTM inner loops and do not
// amortize.
//
// Recognising more than one at a time therefore also caps OpenMP at one thread
// per process, unless the caller named an explicit ompThreadLimit on New, which
// is left alone. Concurrency multiplied by per-process threads rather than
// bounded by cores is the one shape that is slower than doing nothing: four
// concurrent unbounded passes on four CPUs take 3m36.8s against 1.24s bounded,
// 174x.
//
// Pass a number to take less of the machine than the default, or 1 to recognise
// one image at a time. Non-positive is rejected at output time.
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
// source directory root, sorted. Sorted is not the order they are recognised
// in — they run several at a time — but it is the order their artifacts are
// assembled in, so the returned directory is the same whatever the scheduling
// did.
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
// Each image is recognised on its own, as its own exec, WithConcurrency of them
// at a time. tesseract's own list-file mode — a text file of image paths as the
// FILE argument — is not what a batch wants, because it treats the list as one
// multi-page *document*: it renders a single concatenated artifact set (one .txt
// with form-feed page breaks, one multi-page PDF) and offers no way to get the
// per-image files this returns.
//
// One exec per image is what makes the results cache per image too: an edited
// page invalidates its own recognition and nothing else, which is the whole
// difference for a corpus that grows a page at a time. It costs one exec's
// overhead per page instead of per batch — see the README for what that is
// worth measured — and it is the shape that lets the fan-out live in Go.
func (b *Batch) Export(
	ctx context.Context,
	// Output formats to render for each image in the batch.
	formats []Format,
) (*dagger.Directory, error) {
	selected, err := selectedFormats(formats)
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
	// Validated once here rather than per image: the checks are the same for
	// every recognition in the batch, and a bad option should be reported as
	// itself instead of as a failure of whichever page happened to run first.
	if err := b.Options.validate(ctx, b.Tesseract); err != nil {
		return nil, err
	}

	execs, err := b.recognise(ctx, sources, formatConfigs(selected))
	if err != nil {
		return nil, err
	}

	// Assembling the mirrored directory is pure graph-building — every
	// recognition has already run — so it is one WithFile per artifact, in a
	// fixed order, off the exec that produced it.
	out := dag.Directory()
	for i, source := range sources {
		base := outputBaseFor(source)
		for _, format := range selected {
			ext := formatTable[format].ext
			out = out.WithFile(base+ext, execs[i].File(outputBase+ext))
		}
	}
	return out, nil
}

// recognise runs one exec per image, at most workers() of them at a time, and
// returns the finished execs positionally.
//
// The bound is a slot channel rather than an unbounded fan-out because a
// thousand pages is a thousand containers otherwise, and because recognition
// that is already using every core gains nothing from being asked for more of
// them at once.
//
// The first failure cancels the rest: a batch that is going to fail has no
// reason to recognise the remaining pages first, and the error it reports names
// the image. tesseract's own message usually cannot — handed a file leptonica
// will not decode, it falls back to reading the file as a list of image paths
// and reports the *contents* of its first line as the file it could not open.
func (b *Batch) recognise(ctx context.Context, sources []string, configs []string) ([]*dagger.Container, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	execs := make([]*dagger.Container, len(sources))
	slots := make(chan struct{}, b.workers())

	for i, source := range sources {
		wg.Go(func() {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}

			exec, err := b.document(source).run(ctx, outputBase, configs...)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				// Formatted rather than %w-wrapped: the error crosses the
				// module boundary, which unwraps a chain back to the inner
				// error and would drop the image's name along with it.
				if first == nil {
					first = fmt.Errorf("Batch: %q: %v", source, err)
					cancel()
				}
				return
			}
			execs[i] = exec
		})
	}
	wg.Wait()

	if first != nil {
		return nil, first
	}
	return execs, nil
}

// document binds one matched image to the toolchain as the Document it is, so
// the two units of work share their execution as well as their option set: a
// batch is its images recognised as Documents, several at a time.
func (b *Batch) document(source string) *Document {
	return &Document{
		Tesseract: b.toolchain(),
		Source:    b.Source.File(source),
		Options:   b.Options,
	}
}

// toolchain is the module the batch's recognitions run on: the caller's, or a
// copy bounded to one OpenMP thread when several of them run at once.
//
// Concurrency multiplied by per-process threads rather than bounded by cores is
// the one shape slower than doing nothing (see WithConcurrency), and one thread
// per process is the fast shape outright. The bound rides on a copy of the
// module rather than on the caller's, so nothing else built from the same
// Tesseract sees it; it is applied after `apk add`, so the two images still
// share the package fetch. An ompThreadLimit the caller named on New is theirs
// and is left alone.
func (b *Batch) toolchain() *Tesseract {
	if b.workers() <= minBatchConcurrency || b.Tesseract.OmpThreadLimit != hostCpuOmpThreadLimit {
		return b.Tesseract
	}
	out := b.Tesseract.clone()
	out.OmpThreadLimit = concurrentOmpThreadLimit
	return out
}

// with returns a copy of the batch carrying a new option set, which is what
// keeps every builder immutable.
func (b *Batch) with(opts options) *Batch {
	out := *b
	out.Options = opts
	return &out
}

// workers is how many recognitions run at once: whatever WithConcurrency asked
// for, or one per CPU the module can see.
//
// The default is the core count rather than one because every image is its own
// exec now: recognising them one at a time would pay that exec's overhead N
// times over and spend none of the parallelism it bought. What makes the core
// count safe is toolchain's OpenMP bound — see WithConcurrency.
func (b *Batch) workers() int {
	if b.HasConcurrency {
		return b.Concurrency
	}
	if cpus := runtime.NumCPU(); cpus > minBatchConcurrency {
		return cpus
	}
	return minBatchConcurrency
}

// validateConcurrency rejects a bound that would run no images at all. It is
// deferred to output time rather than reported from the builder because a
// builder has no error return, which is the same reason WithDpi's check lives
// away from WithDpi.
func (b *Batch) validateConcurrency() error {
	if b.HasConcurrency && b.Concurrency < minBatchConcurrency {
		return fmt.Errorf(
			"WithConcurrency: concurrency must be positive, got %d: pass 1 to recognise one image at a time, or leave it unset for one per available CPU",
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
