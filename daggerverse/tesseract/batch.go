package main

import (
	"context"
	"fmt"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"dagger/tesseract/fanout"
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
// Each matched image is recognised by its own tesseract invocation, and the
// invocations are split across at most WithConcurrency execs — one contiguous
// slice of the batch each. What decides the bound, the partition, the
// scheduling, the fail-fast and the failure message is Go; the only thing
// interpreted inside a container is the list of per-image commands that slice
// was handed. That keeps all of it in a language with types and a debugger,
// instead of in a runner mounted into an image.
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

// WithConcurrency bounds how many images the batch recognises at once, and with
// it how many execs the export creates.
//
// It defaults to the number of CPUs the module can see, which is what a batch
// wants: processes are how tesseract parallelises well — eleven pages on four
// CPUs take 3.37s one at a time and 1.05s eleven at a time, ~90% efficiency
// where its own OpenMP manages 33%, because those regions sit inside the LSTM
// inner loops and do not amortize.
//
// Recognising more than one at a time therefore also caps OpenMP at one thread
// per process, unless the caller named an explicit ompThreadLimit on New, which
// is left alone. Concurrency multiplied by per-process threads rather than
// bounded by cores is the one shape that is slower than doing nothing: four
// concurrent unbounded passes on four CPUs take 3m36.8s against 1.24s bounded,
// 174x.
//
// Pass a number to take less of the machine than the default, or 1 to recognise
// the whole batch in one exec, an image at a time. Non-positive is rejected at
// output time.
//
// The two meanings are one setting because they were never independent. The
// images are split into this many contiguous slices and each slice is one exec
// running its images' invocations in turn, so the bound is at once how many
// recognise at a time and how many containers an export creates — a
// three-thousand-image batch is this many, not three thousand. It is what
// decides how the results cache, too: a slice is the unit that hits or misses,
// so an edited image re-recognises its slice rather than only itself.
//
// What it is still not is a change to the answer. The same artifacts come back
// under every bound, named the same way and recognised from the same bytes;
// concurrency is allowed to change how long an export takes and nothing else.
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
// Each image is recognised by its own invocation, WithConcurrency of them at a
// time, and each invocation writes its artifacts straight onto the mirrored
// path. tesseract's own list-file mode — a text file of image paths as the FILE
// argument — is not what a batch wants, because it treats the list as one
// multi-page *document*: it renders a single concatenated artifact set (one .txt
// with form-feed page breaks, one multi-page PDF) and offers no way to get the
// per-image files this returns.
//
// One invocation per image is what makes the results cache per slice of images
// rather than per batch: an edited page invalidates the slice it falls in and
// nothing else, which is most of the difference for a corpus that grows a page
// at a time. See the README for what that is worth measured, and for why the
// slice — rather than the image — is the granularity now.
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
	return assemble(execs), nil
}

// assemble folds the finished execs into the directory an export returns: one
// WithDirectory per exec, in slice order, which is sorted-path order.
//
// This is the whole of what #371 changed and the reason it was worth changing.
// Every fold adds an overlayfs lowerdir to the destination snapshot's chain, and
// containerd refuses to mount a chain whose compacted mount data exceeds one
// page — measured at around 440 folds on a fresh engine, and fewer as snapshot
// IDs grow a digit. A directory produced by an exec is *one* snapshot however
// many files that exec wrote, so folding per exec rather than per artifact makes
// the depth len(execs), which the bound caps and neither the image count nor the
// format count enters.
//
// The format loop leaving this fold is half of that: it used to multiply the
// depth, so asking for two formats halved the number of images a batch could
// carry. Each slice's exec now writes every requested format for every one of
// its images, and the whole of what it wrote is taken at once.
//
// It is also why nothing downstream should fold this directory an artifact at a
// time to build one of its own: the ceiling is on the chain, not on this module.
func assemble(execs []*dagger.Container) *dagger.Directory {
	out := dag.Directory()
	for _, exec := range execs {
		out = out.WithDirectory("/", exec.Directory(outputDir))
	}
	return out
}

// recognise partitions the images into at most workers() slices, runs one exec
// per slice, and returns the finished execs positionally — in slice order, which
// is sorted-path order.
//
// One exec per *slice* rather than per image is what keeps a batch's exec count,
// and so its fold depth, off the number of images: see assemble for what the
// fold could not survive, and fanout.Partition for the property that makes it
// constant. It costs no parallelism, the images already having been admitted
// workers() at a time — a three-thousand-image batch on a 16 CPU host was
// creating three thousand containers in order to run sixteen.
//
// The scheduling and the partitioning are both fanout.RunSlices', one call
// taking the bound once — a module that partitioned by one figure and scheduled
// by another would have two bounds to keep in agreement and no reason for them
// to differ. That package imports no dagger and is tested with `go test -race`.
//
// The first failure cancels the rest: a batch that is going to fail has no
// reason to recognise the remaining pages first, and the error it reports names
// the image rather than the slice it shared an exec with. tesseract's own
// message usually cannot — handed a file leptonica will not decode, it falls
// back to reading the file as a list of image paths and reports the *contents*
// of its first line as the file it could not open.
//
// The flags are rendered once here rather than per slice. They are the same for
// every recognition in the batch, and a bad one should be reported as itself
// instead of as a failure of whichever slice happened to run first.
func (b *Batch) recognise(ctx context.Context, sources []string, configs []string) ([]*dagger.Container, error) {
	toolchain := b.toolchain()
	flags, err := b.Options.flags(toolchain)
	if err != nil {
		return nil, err
	}
	args := append(flags, configs...)

	return fanout.RunSlices(ctx, b.workers(), sources,
		func(ctx context.Context, slice []string) (*dagger.Container, error) {
			return b.recogniseSlice(ctx, toolchain, slice, args)
		})
}

// recogniseSlice runs one slice of the batch: its images mounted at their own
// paths, its recognitions run in turn inside one exec, and its artifacts written
// straight onto the mirrored paths.
//
// The images are *mounted* rather than copied in, one mount each, and that is
// the half of this change that is easy to get wrong in either direction.
//
// Copying them in a file at a time is what the fold on the output side had to
// stop doing: every copy adds an overlayfs lowerdir to the container's own
// rootfs chain, so the input side would rebuild exactly the depth assemble just
// shed. A mount adds none — it is an independent mount point on the exec, not a
// layer on a snapshot — so nothing here grows the chain, whatever the slice
// holds.
//
// Copying the slice in with **one** filtered operation would be shallower still,
// and #371 was written expecting that to be the answer. Measured on dagger
// v0.21.7, it is not: a `WithDirectory(…, Include: slice)` re-recognises the
// *whole batch* when one image changes — 50 images with one edited took 4.84s
// against 2.66s for the per-image mount it replaced, which is a cold run. The
// mechanism is buildkit's, and it is the same one #298 was relying on without
// naming: an exec mount carrying a path *selector* gets a content-based cache
// key computed from the bytes at that path, and nothing else does. A copy into
// the rootfs is keyed on the source directory's digest as a whole, include
// patterns or not, so every slice's exec is invalidated by an edit to any image
// in the directory.
//
// So the mount is not a stylistic preference over the copy; it is the only shape
// measured to keep an edited page from re-recognising the other forty-nine.
// Mounting the whole source directory in one go loses it for the same reason —
// a mount with no selector is not content-hashed either — which is what #298
// found and what this must not undo.
func (b *Batch) recogniseSlice(
	ctx context.Context,
	toolchain *Tesseract,
	slice []string,
	args []string,
) (*dagger.Container, error) {
	ctr := toolchain.Container().WithDirectory(outputDir, dag.Directory())
	for _, source := range slice {
		ctr = ctr.WithMountedFile(batchSourceDir+"/"+source, b.Source.File(source))
	}

	exec := b.Options.mount(ctr).WithExec(
		[]string{"sh", "-c", sliceScript(slice, args)},
		dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny})
	code, err := exec.ExitCode(ctx)
	if err != nil {
		return nil, err
	}
	if code == 0 {
		return exec, nil
	}
	return nil, sliceFailure(ctx, exec, slice, code)
}

// sliceScript is the script one exec runs: the guard, the output directories the
// slice's images mirror, and then one guarded tesseract invocation per image in
// slice order.
//
// Every word of every invocation is shell-quoted, which the argv the recognition
// used to be built as did not have to be. Two of those words are the caller's
// outright — the image's path, and any `-c name=value` WithParameter set — so a
// join without quoting would be the caller writing shell.
//
// `set -e` is what makes the mkdir fail the exec; the recognitions do not rely
// on it, each being an explicitly handled `||`. A failure still stops the images
// behind it inside its own slice, and fanout.Run's cancellation is what stops
// the other slices.
func sliceScript(slice []string, args []string) string {
	lines := make([]string, 0, len(slice)+3)
	lines = append(lines, "set -e",
		imageFailureFn+`() { echo "`+imageFailureMarker+`$1" >&2; exit "$2"; }`,
		shellCommand(append([]string{"mkdir", "-p"}, outputDirsFor(slice)...)))

	for i, source := range slice {
		command := shellCommand(append([]string{
			"tesseract",
			batchSourceDir + "/" + source,
			outputDir + "/" + outputBaseFor(source),
		}, args...))
		lines = append(lines, fmt.Sprintf("%s || %s %d $?", command, imageFailureFn, i))
	}
	return strings.Join(lines, "\n")
}

// outputDirsFor is the set of directories a slice's artifacts land in, sorted
// and de-duplicated, which one `mkdir -p` then creates. tesseract will not
// create an output base's directory and fails with a bare `Error, cannot create
// output file` if it is missing.
func outputDirsFor(slice []string) []string {
	seen := make(map[string]struct{}, len(slice))
	dirs := make([]string, 0, len(slice))
	for _, source := range slice {
		dir := path.Join(outputDir, path.Dir(source))
		if _, dup := seen[dir]; dup {
			continue
		}
		seen[dir] = struct{}{}
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

// sliceFailure turns a failed slice into the error the failing image would have
// carried had it been the only image in the exec.
//
// The image comes out of the script rather than out of a label chosen here,
// which is the whole difference a slice makes: Go knows which images the exec
// covers and only the script knows which invocation was running when it stopped.
// See sliceScript for how the position is reported and takeFailedImage for how
// it is read back and removed from what the caller sees.
//
// A failure carrying no position is not an image's: the mkdir is the only thing
// in a slice's script that runs outside a guarded command, so the message names
// the run of images rather than inventing one of them.
func sliceFailure(ctx context.Context, exec *dagger.Container, slice []string, code int) error {
	stdout, _ := exec.Stdout(ctx)
	stderr, _ := exec.Stderr(ctx)
	at, stderr := takeFailedImage(stderr)
	// Formatted rather than %w-wrapped: the error crosses the module boundary,
	// which unwraps a chain back to the inner error and would drop the image's
	// name along with it.
	if at >= 0 && at < len(slice) {
		return fmt.Errorf("Batch: %q: tesseract failed (exit %d):\n%s",
			slice[at], code, joinOutput(stdout, stderr))
	}
	return fmt.Errorf("Batch: %q..%q: tesseract failed (exit %d):\n%s",
		slice[0], slice[len(slice)-1], code, joinOutput(stdout, stderr))
}

// takeFailedImage reads back the position a slice's script reported failing on,
// and returns stderr with that report removed.
//
// Removing it is the point of reading it: the image belongs in the message's
// first line, where a caller looks, and the marker line is this module talking
// to itself. A stderr carrying no marker yields -1, which is the caller's signal
// that the failure was not an image's.
func takeFailedImage(stderr string) (int, string) {
	lines := strings.Split(stderr, "\n")
	kept := make([]string, 0, len(lines))
	at := -1
	for _, line := range lines {
		rest, ok := strings.CutPrefix(line, imageFailureMarker)
		if !ok {
			kept = append(kept, line)
			continue
		}
		// A script exits as soon as it reports, so there is at most one marker;
		// the last one wins if that ever stops being true.
		if n, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil && n >= 0 {
			at = n
		}
	}
	return at, strings.Join(kept, "\n")
}

// shellCommand joins words into one shell command, quoting every one of them.
//
// Quoting all of them rather than the ones that need it is deliberate: which
// words are the caller's changes as options are added, and a rule that has to be
// re-derived per word is a rule that eventually is not applied to a new one.
func shellCommand(words []string) string {
	quoted := make([]string, 0, len(words))
	for _, word := range words {
		quoted = append(quoted, shellQuote(word))
	}
	return strings.Join(quoted, " ")
}

// shellQuote renders one word as a single-quoted shell literal. Inside single
// quotes the shell interprets nothing at all, so the only thing that has to be
// handled is a single quote itself: the literal is closed, an escaped quote is
// emitted, and the literal is reopened.
func shellQuote(word string) string {
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// toolchain is the module the batch's recognitions run on: the caller's, or a
// copy bounded to one OpenMP thread when several of them run at once.
//
// Concurrency multiplied by per-process threads rather than bounded by cores is
// the one shape slower than doing nothing (see WithConcurrency), and one thread
// per process is the fast shape outright. The processes are the slices' execs
// rather than the images' — a slice runs its images one after another, so there
// are workers() tesseract processes alive at a time whether the batch holds
// sixteen images or three thousand, which is exactly the count the bound names.
// That is why the bound needs no adjusting for the partitioning: it was always
// counting concurrent recognitions, and it still is.
//
// The bound rides on a copy of the module rather than on the caller's, so
// nothing else built from the same Tesseract sees it; it is applied after `apk
// add`, so the two images still share the package fetch. An ompThreadLimit the
// caller named on New is theirs and is left alone.
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

// workers is how many recognitions run at once, and now also how many execs an
// export creates: whatever WithConcurrency asked for, or one per CPU the module
// can see.
//
// The default is the core count rather than one because a single tesseract
// recognises its image on close to one core whatever the machine has — its own
// OpenMP regions sit inside the LSTM inner loops and run at ~33% efficiency — so
// the bound is what turns a folder of scans into a parallel recognition. What
// makes the core count safe is toolchain's OpenMP bound — see WithConcurrency.
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
