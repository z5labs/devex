// Package main implements the tesseract Dagger module: optical character
// recognition as a `dagger call` instead of the usual hand-rolled Dockerfile
// plus rescue-the-output shell script. Hand it an image and get back plain
// text, or hOCR / ALTO / TSV / PAGE / searchable PDF for anything that needs
// word positions and confidences.
//
// There is no official Tesseract container image — upstream ships source only
// — so this module assembles its own the way the qemu module does: a
// module-pinned Alpine plus `apk add tesseract-ocr`.
//
// Assembling the image means two fetches, not one, and a network that cannot
// reach the public internet has to be told about both. New's registry argument
// moves the *image*; WithApkRepository, WithApkKey and WithApkAuth move the
// *packages*, and are what an air-gapped run needs — a mirrored Alpine image
// still runs `apk add` against dl-cdn.alpinelinux.org otherwise. Both `apk add`
// this module performs, the toolchain's and the PDF rasterizer's, are
// configured from the one place.
//
// The language set lives on the root object rather than on Document because on
// Alpine each language is a separate apk package (`tesseract-ocr-data-<lang>`,
// none of which the base package pulls in). Selecting a language changes what
// the image *is*, not just what a flag says; Document.WithLanguage only picks a
// subset of what was installed. WithTessdata is the same decision for models
// Alpine has no package for — a directory of `.traineddata` merged into the
// image's datadir, and from there indistinguishable from a packaged language.
//
// PDF input is rasterized rather than read, because leptonica cannot read PDF
// at all. That work happens in its own container — Alpine at the same pinned
// tag, plus poppler-utils and a font — instead of on the toolchain image,
// which is a decision about who pays for it. Unconditionally installing both
// packages takes the toolchain image from 67.1MiB to 81.4MiB, a 21% tax on
// every caller who only ever hands this module a PNG. Rasterizing separately
// leaves that image untouched and costs a PDF caller a 35.0MiB container of
// which the 8.0MiB Alpine base is already shared, so the extra bytes are paid
// once, by the callers who asked for them, and the rasterization caches
// separately from recognition on top of that.
//
// File map (all `package main`, surfaced as one Dagger module):
//
//   - enums.go     — PageSegMode / EngineMode / Format enums plus the token and
//     output-extension tables that map them onto the CLI.
//   - options.go   — the recognition option set Document and Batch share: one
//     builder, one piece of argv and one deferred check per option, so the two
//     units of work cannot drift apart.
//   - document.go  — *Document, one image in and one artifact set out.
//   - batch.go     — *Batch, a directory in and a mirrored directory out, all
//     of it in a single container exec.
//   - ci.go        — *Ci, a batch plus a confidence gate, for the repo that
//     wants its whole document pipeline as one declarative call.
//   - pdf.go       — FromPdf, the rasterizer that turns a PDF into pages a
//     *Document can recognise.
//   - training.go  — *Training, the other direction: images plus ground truth
//     in, a fine-tuned model out.
package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"dagger/tesseract/internal/dagger"
)

const (
	// alpineImagePath is the repository under the configured registry. Only
	// the registry prefix is caller-overridable, for an image mirrored into a
	// private registry; the path is fixed so every recognition runs on a known
	// toolchain. Where the packages come from is a separate question, answered
	// by WithApkRepository.
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

	// packagedTessdataDir is where the apk packages drop their models. It
	// holds more than models: `configs/` carries the renderer configfiles
	// recognition names as its trailing arguments, and pdf.ttf is what the
	// PDF renderer draws its invisible text layer with.
	packagedTessdataDir = "/usr/share/tessdata"

	// tessdataDir is the module-owned datadir WithTessdata merges the
	// packaged models and the caller's into, and the path recognition then
	// points --tessdata-dir at.
	tessdataDir = "/tessdata"

	// outputBase is the `tesseract IMAGE OUTPUTBASE` argument. Each renderer
	// appends its own extension to it (see Format.ext).
	outputBase = outputDir + "/result"

	// stdoutBase is the OUTPUTBASE that makes tesseract write to stdout
	// instead of a file.
	stdoutBase = "-"

	userWordsPath    = workDir + "/user-words.txt"
	userPatternsPath = workDir + "/user-patterns.txt"

	// batchSourceDir is where Batch mounts the caller's directory, and
	// batchManifestPath the record list its exec loops over. Both sit beside
	// the single-document mounts rather than replacing them, so the user word
	// and pattern lists land in the same place for either unit of work.
	batchSourceDir    = workDir + "/batch"
	batchManifestPath = workDir + "/batch.tsv"

	// defaultBatchGlob matches every path in the source directory. It is not
	// the whole default: what actually narrows a batch to images is the
	// extension filter isImagePath applies on top of it, because Dagger's glob
	// has no brace expansion to spell the alternation with.
	defaultBatchGlob = "**/*"

	// trainingSourceDir is where Training mounts the caller's pairs together
	// with the box files generated for them, and trainingManifestPath the
	// record list the sample-building loop reads. trainingListPath is the
	// sample list lstmtraining and lstmeval are both pointed at; it sits
	// beside the manifest rather than in the scratch directory because it is
	// written here rather than by the run.
	trainingSourceDir    = workDir + "/training"
	trainingManifestPath = workDir + "/training.tsv"
	trainingListPath     = workDir + "/training-samples.txt"

	// trainingScratchDir holds everything a training run writes that is not
	// the model: the network extracted from the base model, the per-sample
	// `.lstmf` files, and lstmtraining's checkpoints. It is inside the
	// container and never lifted off it — the checkpoints alone are ~70MB and
	// mean nothing outside the run that wrote them.
	trainingScratchDir = "/training"
	trainingLstmfDir   = trainingScratchDir + "/lstmf"
	trainingBaseLstm   = trainingScratchDir + "/base.lstm"

	// trainingModelBase is lstmtraining's `--model_output`, off which it names
	// every checkpoint it writes; trainingCheckpoint is the one it keeps
	// current, and the one `--stop_training` is continued from.
	trainingModelBase  = trainingScratchDir + "/model"
	trainingCheckpoint = trainingModelBase + "_checkpoint"

	// trainingPageSeg is the `--psm` the sample-building pass runs under.
	// Mode 6 — one uniform block of text — is what tesseract's own training
	// tooling uses, and on a single-line image it is the mode that finds that
	// line without also trying to work out a column layout around it.
	trainingPageSeg = "6"

	// defaultTrainingIterations is how long fine-tuning runs when the caller
	// names no length. See Training.WithIterations for why it is this small.
	defaultTrainingIterations = 100

	// groundTruthExt is the suffix pairing a ground-truth file with its image:
	// `line-1.png` is transcribed by `line-1.gt.txt`. It is tesseract's own
	// training-data convention rather than this module's invention.
	groundTruthExt = ".gt.txt"

	// boxExt names the file carrying an image's ground truth in the format
	// `lstm.train` reads it, lstmfExt the training sample built from the two,
	// and traineddataExt the model that comes out the far end.
	boxExt         = ".box"
	lstmfExt       = ".lstmf"
	traineddataExt = ".traineddata"

	// processedImagesExt is what the get.images renderer appends to the output
	// base for the image tesseract actually recognised, after its own
	// thresholding and deskewing.
	processedImagesExt = ".processed.tif"

	// apkRepositoriesFile is the list `apk add` resolves packages from.
	// WithApkRepository overwrites it rather than appending to it, so a caller
	// who names a mirror gets an image that cannot quietly fall back to the
	// CDN the mirror exists to replace.
	apkRepositoriesFile = "/etc/apk/repositories"

	// apkKeysDir is where apk looks up the public key an index's signature
	// names. A repository whose key is not here is untrusted, and apk refuses
	// its index rather than installing from it — which is why a private
	// mirror needs WithApkKey and not just WithApkRepository.
	apkKeysDir = "/etc/apk/keys"

	// apkNetrcPath is where WithApkAuth's credentials are mounted and
	// apkNetrcEnv the variable pointing apk at them. Alpine 3.24 ships
	// apk-tools 3.0, whose built-in libfetch reads HTTP credentials from the
	// netrc file `$NETRC` names and from nowhere else that keeps them out of
	// sight: userinfo in the repository URL would land in the repositories
	// file and in every error message quoting it, and HTTP_AUTH would land in
	// the image's environment. The file is mounted rather than written so it
	// is not a layer either.
	apkNetrcPath = "/run/apk/netrc"
	apkNetrcEnv  = "NETRC"

	// popplerPkg carries pdftoppm, the rasterizer FromPdf drives. fontPkg is
	// the substitute font family poppler draws with when a PDF names one of
	// the base-14 fonts without embedding it; see rasterize for why it is not
	// optional.
	popplerPkg = "poppler-utils"
	fontPkg    = "ttf-liberation"

	// pdfSourcePath is where the PDF is mounted in the rasterizer. It sits
	// outside workDir because the rasterizer is a different container from the
	// one recognition runs in, and shares nothing with it but the page
	// directory.
	pdfSourcePath = "/pdf/source.pdf"

	// pdfPagesDir holds the rasterized pages. The rasterizer writes them here
	// and recognition mounts them back at the same path, which is what lets
	// the page list name them by absolute path.
	pdfPagesDir = workDir + "/pages"

	// pdfPageBase is the OUTPUTBASE handed to pdftoppm, which appends a page
	// number and the format's extension to it; pdfPageListPath is the file
	// list recognition then takes as its FILE argument.
	pdfPageBase     = pdfPagesDir + "/page"
	pdfPageListPath = pdfPagesDir + "/pages.txt"

	// defaultPdfDpi is the resolution pages are rasterized at. 300 is the
	// long-standing recommendation for OCR input in tesseract's own quality
	// documentation, and the resolution scanners are set to for the same
	// reason.
	defaultPdfDpi = 300

	// ompThreadLimitEnv caps the OpenMP team size tesseract's recognition
	// loops fan out to.
	ompThreadLimitEnv = "OMP_THREAD_LIMIT"

	// hostCpuOmpThreadLimit is the unset value: no OMP_THREAD_LIMIT on the
	// image, so libgomp sizes every thread team by the CPUs it can see. That
	// is the right default for the common case — one image, one machine, a
	// caller who wants the whole box — and the wrong one as soon as several
	// recognitions share the cores, which is what the bound exists for.
	hostCpuOmpThreadLimit = 0
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
	// +private
	OmpThreadLimit int
	// +private
	Tessdata *dagger.Directory
	// +private
	ApkRepositories []string
	// +private
	ApkKeys []*dagger.File
	// +private
	ApkAuth *dagger.Secret
}

// New returns a Tesseract module backed by <registry>/library/alpine:<tag>
// with tesseract-ocr and one language package per requested language.
func New(
	// Container registry hosting the alpine image. This moves the image only:
	// see WithApkRepository for where the packages installed onto it are
	// fetched from.
	// +default="docker.io"
	registry string,
	// Tag of the alpine image the toolchain is assembled on.
	// +default="3.24"
	alpineTag string,
	// Language codes to install, one apk package each. Empty installs "eng".
	// "osd" is not a recognition language but is required by Document.Osd.
	// +optional
	languages []string,
	// Upper bound on the OpenMP threads tesseract may use, set on the
	// assembled image as OMP_THREAD_LIMIT. Unset, tesseract uses one thread
	// per available CPU, which is what a caller who owns the machine wants.
	// Set it when several recognitions share the cores — concurrent passes
	// each claiming every CPU oversubscribe the box badly enough to cost an
	// order of magnitude, which is the shape of the long-standing upstream
	// slowdown reports (tesseract-ocr/tesseract#2611, #1171, #263). One
	// thread per pass is the usual setting there.
	// +optional
	ompThreadLimit int,
) (*Tesseract, error) {
	if registry == "" {
		registry = defaultRegistry
	}
	if alpineTag == "" {
		alpineTag = defaultAlpineTag
	}
	if ompThreadLimit < 0 {
		return nil, fmt.Errorf(
			"New: ompThreadLimit must not be negative, got %d: pass a positive thread cap, or leave it unset for one thread per available CPU",
			ompThreadLimit)
	}
	return &Tesseract{
		Registry:       registry,
		AlpineTag:      alpineTag,
		Languages:      normalizeLanguages(languages),
		OmpThreadLimit: ompThreadLimit,
	}, nil
}

// WithTessdata adds a directory of `.traineddata` models to the image, which
// is the only way to reach a model Alpine has no package for: a fine-tuned
// model, a tessdata_best or tessdata_fast variant, or a language whose package
// simply does not exist.
//
// Every file whose name ends in `.traineddata` becomes a language named after
// its stem — `deu_frak.traineddata` is the language `deu_frak` — so a model is
// renamed by renaming its file. Langs reports the union of these and the
// packaged ones, and everything that takes a language name accepts either.
//
// The directory is merged with the packaged models rather than replacing them,
// because tesseract's datadir is more than a bag of models: it also holds the
// renderer configfiles and the font the PDF renderer needs. A caller-supplied
// model wins over a packaged one of the same name, which is what makes
// replacing the stock `eng` with a fine-tuned one work.
func (t *Tesseract) WithTessdata(
	// Directory of `.traineddata` models to make available to recognition.
	dir *dagger.Directory,
) *Tesseract {
	out := t.clone()
	out.Tessdata = dir
	return out
}

// WithApkRepository points package installation at an Alpine repository other
// than the one the base image ships, which is what makes this module work on a
// network that cannot reach dl-cdn.alpinelinux.org.
//
// New's registry argument is not enough on its own and never was: it moves the
// *image*, and the packages are still fetched by `apk add` from whatever
// /etc/apk/repositories carries — so mirroring Alpine into a private registry
// buys a container that then fails on its first `apk add`, or, where the CDN is
// blackholed rather than refused, hangs until it times out.
//
// Repeatable, in preference order. The first call replaces the image's list
// rather than appending to it, because the air-gapped case needs the
// unreachable defaults gone rather than merely deprioritized: a repository apk
// still consults is a repository apk still waits for.
//
// The URL is the repository base, spelled the way it would be spelled in
// /etc/apk/repositories — `https://mirror.example.com/alpine/v3.24/main`, one
// call per component. A repository's index is signed, so pair this with
// WithApkKey unless the mirror is signed by a key the base image already
// trusts.
func (t *Tesseract) WithApkRepository(
	// Base URL of an Alpine repository to resolve packages from.
	url string,
) *Tesseract {
	url = strings.TrimSpace(url)
	if url == "" {
		return t
	}
	out := t.clone()
	if !contains(out.ApkRepositories, url) {
		out.ApkRepositories = append(out.ApkRepositories, url)
	}
	return out
}

// WithApkKey trusts a repository's signing key by dropping it into
// /etc/apk/keys, which is the other half of WithApkRepository: a private
// mirror's index is signed by a key the stock Alpine image has never heard of,
// and apk refuses an index it cannot verify rather than installing from it.
//
// The file keeps its own name, because the name is load-bearing — an index
// signature names the key file it was made with, and apk looks that exact file
// up in the keys directory. A key exported from `abuild-keygen` is already
// named correctly; renaming it renames the key.
//
// Repeatable, for a mirror set signed by more than one key. It is a *File
// rather than a *Secret because a public key is not a credential: it is meant
// to be in the image, and WithApkAuth is the option for the part that is not.
func (t *Tesseract) WithApkKey(
	// Public key file trusting a repository's index signature.
	key *dagger.File,
) *Tesseract {
	out := t.clone()
	out.ApkKeys = append(out.ApkKeys, key)
	return out
}

// WithApkAuth supplies credentials for a repository that requires them.
//
// The secret's contents are a netrc file — `machine mirror.example.com login
// USER password PASS`, one stanza per host — which is what apk-tools 3's
// built-in libfetch reads when a repository answers 401. Alpine 3.24 ships
// apk-tools 3.0; see NETRC in `apk(8)`. Hosts are matched by name only, so a
// stanza covers a mirror on any port.
//
// It is a *dagger.Secret rather than a string, and is mounted rather than
// written, so the credentials stay out of the cache key, out of argv, out of
// the image's environment and out of any layer a caller exports. That is also
// why the credentials are not simply userinfo in the WithApkRepository URL,
// which would put them in /etc/apk/repositories and in every apk error message
// that quotes it.
func (t *Tesseract) WithApkAuth(
	// netrc-formatted credentials for the configured repositories.
	credentials *dagger.Secret,
) *Tesseract {
	out := t.clone()
	out.ApkAuth = credentials
	return out
}

// Container returns the assembled toolchain image. This is the escape hatch
// for everything this module does not wrap — the training binaries the apk
// package ships, `combine_tessdata`, and tesseract's long tail of renderers
// stay reachable via `container with-exec`.
//
// A requested OpenMP bound lives here rather than on the recognition
// invocation so everything reached through this escape hatch inherits it too.
//
// +cache="session"
func (t *Tesseract) Container() *dagger.Container {
	args := []string{"apk", "add", "--no-cache", tesseractPkg}
	for _, lang := range t.Languages {
		args = append(args, langPkgPrefix+lang)
	}
	ctr := t.base().WithExec(args)
	// Merged after the install because the packaged half of the union is
	// whatever apk just wrote, and mounted rather than copied so a 20MB-plus
	// model set does not become a layer per image.
	if t.Tessdata != nil {
		ctr = ctr.WithMountedDirectory(
			tessdataDir,
			ctr.Directory(packagedTessdataDir).WithDirectory("/", t.Tessdata))
	}
	// Applied after the install so the thread bound does not become part of
	// the apk layer's cache key: two images differing only in their limit
	// still share the package fetch.
	if t.OmpThreadLimit > hostCpuOmpThreadLimit {
		ctr = ctr.WithEnvVariable(ompThreadLimitEnv, strconv.Itoa(t.OmpThreadLimit))
	}
	return ctr
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

// Langs returns the language codes the image can recognise in, as reported by
// `tesseract --list-langs`: the packaged languages and any model WithTessdata
// added, as one set. This is what Document.WithLanguage validates against, and
// it includes "osd" when that model was installed or supplied.
//
// +cache="session"
func (t *Tesseract) Langs(ctx context.Context) ([]string, error) {
	out, err := t.Container().
		WithExec(append([]string{"tesseract", "--list-langs"}, t.tessdataArgs()...)).
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

// Batch binds a directory of images to the toolchain, for the shape a
// scanned-document pipeline actually arrives in: a folder of pages rather than
// one file at a time.
//
// Export returns a directory mirroring the input layout, so a batch composes
// with whatever reads the results the same way the input directory was
// composed. Which files take part is a glob, defaulting to the image
// extensions leptonica can read.
func (t *Tesseract) Batch(source *dagger.Directory) *Batch {
	return &Batch{Tesseract: t, Source: source}
}

// Training binds a directory of image and ground-truth pairs to the toolchain
// and fine-tunes a model against them, which is recognition run backwards: the
// text is what you have and the model is what you want.
//
// It is here rather than behind Container because the apk package already
// ships every binary the job needs — lstmtraining, combine_tessdata, lstmeval
// — so what stands between a directory of transcribed lines and a
// `.traineddata` is orchestration rather than installation: a box file per
// image, a training sample per box, one training run, one freeze.
//
// The model it produces pairs directly with WithTessdata, so a fine-tune and
// the recognition that uses it are two calls on the same module.
func (t *Tesseract) Training(source *dagger.Directory) *Training {
	return &Training{Tesseract: t, Source: source}
}

func (t *Tesseract) image() string {
	return fmt.Sprintf("%s/%s:%s", t.Registry, alpineImagePath, t.AlpineTag)
}

// clone copies the module's configuration for a builder method, deep enough
// that the copy's slices are its own: a builder that appended in place would
// let a second call off the same value overwrite the first one's addition.
func (t *Tesseract) clone() *Tesseract {
	out := *t
	out.Languages = append([]string(nil), t.Languages...)
	out.ApkRepositories = append([]string(nil), t.ApkRepositories...)
	out.ApkKeys = append([]*dagger.File(nil), t.ApkKeys...)
	return &out
}

// base is the module's Alpine image with package installation configured, and
// is what every `apk add` in this module starts from — the toolchain's here,
// the rasterizer's in pdf.go — so the two cannot drift into fetching packages
// from different places.
func (t *Tesseract) base() *dagger.Container {
	return t.withApkConfig(dag.Container().From(t.image()))
}

// withApkConfig applies the caller's repositories, keys and credentials to a
// container, immediately before the `apk add` that reads them.
//
// Each half is applied only when it was asked for. That is not tidiness: with
// none of the options set the container is byte-identical to the one this
// module assembled before they existed, so an existing caller sees no
// cache-key churn from a feature they are not using.
func (t *Tesseract) withApkConfig(ctr *dagger.Container) *dagger.Container {
	if len(t.ApkRepositories) > 0 {
		ctr = ctr.WithNewFile(apkRepositoriesFile, strings.Join(t.ApkRepositories, "\n")+"\n")
	}
	if len(t.ApkKeys) > 0 {
		ctr = ctr.WithFiles(apkKeysDir, t.ApkKeys)
	}
	if t.ApkAuth != nil {
		ctr = ctr.
			WithMountedSecret(apkNetrcPath, t.ApkAuth).
			WithEnvVariable(apkNetrcEnv, apkNetrcPath)
	}
	return ctr
}

// datadir is the directory the image's models actually live in: the merged one
// when WithTessdata supplied any, and the packaged one otherwise. Recognition
// reaches it through tessdataArgs, which can leave the flag off entirely;
// training cannot, because it has to name a model file by path.
func (t *Tesseract) datadir() string {
	if t.Tessdata == nil {
		return packagedTessdataDir
	}
	return tessdataDir
}

// tessdataArgs returns the flag that moves tesseract's datadir to the merged
// directory, and nothing at all when no caller-supplied models were added.
// Leaving the flag off entirely in that case keeps the packaged path the
// default one, so an image with no tessdata behaves exactly as it did before
// the flag existed.
func (t *Tesseract) tessdataArgs() []string {
	if t.Tessdata == nil {
		return nil
	}
	return []string{"--tessdata-dir", tessdataDir}
}

// hasModel reports whether a model is available under the given name. It
// answers from the requested package set without an exec whenever it can, and
// only asks the image itself when a caller-supplied directory could have added
// the model — so the common case stays a pure function.
func (t *Tesseract) hasModel(ctx context.Context, name string) (bool, error) {
	if t.hasLanguage(name) {
		return true, nil
	}
	if t.Tessdata == nil {
		return false, nil
	}
	installed, err := t.Langs(ctx)
	if err != nil {
		return false, err
	}
	return contains(installed, name), nil
}

// hasLanguage reports whether a language code was requested from apk. It reads
// the requested set rather than `--list-langs` so it stays a pure function;
// hasModel is the one that accounts for caller-supplied models.
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
