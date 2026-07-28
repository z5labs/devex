package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	// Registered for their DecodeConfig alone: a training run needs an image's
	// pixel dimensions and nothing else, and DecodeConfig reads the header
	// rather than the pixels.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"dagger/tesseract/internal/dagger"
)

// trainingArgv0 is the `$0` the training script runs under, so the datadir,
// the base model name, the iteration count and the output path reach it as
// `$1`..`$4` rather than being interpolated into the script text. The base
// model name in particular comes from a file name in the caller's tessdata
// directory, which is not a thing to paste into a shell script.
const trainingArgv0 = "tesseract-training"

// integerModelMarker is what lstmtraining says when asked to continue from a
// quantized network. It is the first thing anyone fine-tuning on this image
// will hit — every model Alpine packages is quantized — so the message it
// produces is translated rather than passed through.
const integerModelMarker = "is an integer (fast) model"

// trainingScript is the whole fine-tuning run: extract the base network,
// turn each image plus its ground truth into a training sample, train, and
// freeze the result into a `.traineddata`.
//
// It is one exec rather than four because the intermediates are worthless on
// their own and large: the extracted network is ~12MB and lstmtraining's
// checkpoints are ~70MB, none of which any caller wants to see. Keeping them
// inside a single container also keeps the whole run one cache entry, which is
// what lets Traineddata and Evaluate share it instead of training twice.
//
// `set -e` aborts on the first failure, so an image that produces no training
// sample fails the run carrying tesseract's own message rather than silently
// training on fewer samples than the caller supplied.
const trainingScript = `set -e
datadir="$1"
lang="$2"
iterations="$3"
output="$4"
model="$datadir/$lang` + traineddataExt + `"
mkdir -p ` + trainingLstmfDir + ` ` + outputDir + `
combine_tessdata -e "$model" ` + trainingBaseLstm + `
while IFS="` + "\t" + `" read -r src out dir; do
	mkdir -p "$dir"
	tesseract "$src" "$out" --tessdata-dir "$datadir" -l "$lang" --psm ` + trainingPageSeg + ` lstm.train
done < ` + trainingManifestPath + `
lstmtraining --model_output ` + trainingModelBase + ` --continue_from ` + trainingBaseLstm + ` --traineddata "$model" --train_listfile ` + trainingListPath + ` --max_iterations "$iterations"
lstmtraining --stop_training --continue_from ` + trainingCheckpoint + ` --traineddata "$model" --model_output "$output"
`

// Training is a directory of image plus ground-truth pairs bound to the
// toolchain, and the fine-tuning run that turns them into a model.
//
// The unit of work is one text line: each image holds a single line and its
// `.gt.txt` holds the text that line renders, which is the shape tesseract's
// own training data takes and the reason the ground truth is rejected when it
// carries more than one line. A page of text is not a training sample; it is
// as many samples as it has lines, and cutting it into them is a decision
// about the data rather than about this module.
//
// Fine-tuning needs a *float* base model, which nothing Alpine packages is:
// every model in tesseract-ocr/tessdata is quantized to integers for
// recognition speed and lstmtraining refuses to continue from one. The float
// models live in tesseract-ocr/tessdata_best, and reach this module the same
// way any other unpackaged model does — through WithTessdata. That is why
// WithBaseModel is required rather than defaulting to the recognition
// language: the default would be a model that cannot be trained.
type Training struct {
	// +private
	Tesseract *Tesseract
	// +private
	Source *dagger.Directory
	// +private
	BaseModel string
	// +private
	Iterations int
}

// trainingPair is one training sample: the image, and the path both its ground
// truth and its generated artifacts are named after.
type trainingPair struct {
	// image is the path of the image, relative to the source directory root.
	image string
	// base is that path with its extension removed, which names the ground
	// truth (`<base>.gt.txt`), the box file (`<base>.box`) and the training
	// sample (`<base>.lstmf`).
	base string
}

// WithBaseModel names the model fine-tuning starts from. It is required, and
// it has to be a float model: `lstmtraining` refuses to continue from a
// quantized one, and every model Alpine packages is quantized.
//
// The float models are published as tesseract-ocr/tessdata_best, and are
// supplied to this module exactly as any other unpackaged model is — as a
// directory handed to WithTessdata. The name here is the one that directory
// gives the model, so a `best.traineddata` is the base model "best".
//
// The name is also what the trained model is called: fine-tuning "eng"
// produces an `eng.traineddata`, which is what makes the result drop straight
// back into WithTessdata as a replacement for the model it came from. Give it
// another name by putting it in a directory under one — WithTessdata reads the
// language off the file name.
func (tr *Training) WithBaseModel(
	// Name of the model to fine-tune, as Langs reports it.
	lang string,
) *Training {
	out := *tr
	out.BaseModel = lang
	return &out
}

// WithIterations sets how many training iterations to run — one iteration is
// one training sample presented to the network, so a 40-line set runs 40
// iterations per pass over the data.
//
// The count is always bounded: lstmtraining left to itself trains until its
// error rate stops improving, which on a real data set is hours, so this
// module always passes `--max_iterations` and defaults it to 100. That default
// is deliberately far below what fine-tuning a model for production takes
// (upstream's own worked example uses 400 for a single font, and thousands is
// ordinary) and is chosen instead to keep the first call a caller makes —
// and this module's own test suite — finish in seconds rather than turning
// into an unattended job. Raise it for anything real.
func (tr *Training) WithIterations(
	// Number of training iterations to run. Must be positive.
	n int,
) *Training {
	out := *tr
	out.Iterations = n
	return &out
}

// Files lists the images the run will train on, as paths relative to the
// source directory root, in the order they are presented.
//
// It is where the pairing is checked, so it answers the question a training
// directory always raises — did every image find its ground truth? — without
// paying for the training run. The check is by name alone; what the ground
// truth files actually say is read when the run happens.
func (tr *Training) Files(ctx context.Context) ([]string, error) {
	pairs, err := tr.pairs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p.image)
	}
	return out, nil
}

// Traineddata runs the fine-tuning and returns the resulting model, named
// after the base model it was trained from.
//
// The file is a complete `.traineddata`: `--stop_training` folds the trained
// network back together with the base model's unicharset and dictionaries, so
// it is a drop-in for the model it started from rather than a fragment needing
// assembly. Hand it to WithTessdata and it becomes a language like any other.
func (tr *Training) Traineddata(ctx context.Context) (*dagger.File, error) {
	exec, output, err := tr.train(ctx)
	if err != nil {
		return nil, err
	}
	return exec.File(output), nil
}

// Evaluate runs `lstmeval` against the fine-tuned model and returns the error
// rates it reports: BCER, the character error rate, and BWER, the word error
// rate, both as percentages.
//
// It evaluates against the training set, which makes this a measure of how
// well the model fit the data it was shown rather than of how it will do on
// data it has not seen. Those are different numbers and the second one is the
// one that matters for a model going into production: hold part of the ground
// truth back, and build a second Training over it to measure that.
//
// The run is shared with Traineddata rather than repeated — both read the same
// finished container — so asking for the model and its error rate costs one
// training run, not two.
func (tr *Training) Evaluate(ctx context.Context) (string, error) {
	exec, output, err := tr.train(ctx)
	if err != nil {
		return "", err
	}
	eval, err := tr.exec(ctx, exec, []string{
		"lstmeval", "--model", output, "--eval_listfile", trainingListPath,
	})
	if err != nil {
		return "", err
	}
	// lstmeval leads with a deserialization notice and ends with the rates;
	// only the last line is an answer, and it arrives on stderr.
	out := combinedOutput(ctx, eval)
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "BCER") {
			return line, nil
		}
	}
	return "", fmt.Errorf("Evaluate: lstmeval reported no error rate:\n%s", out)
}

// train assembles and runs the fine-tuning container, and returns the finished
// exec alongside the path the model landed at.
//
// Everything that can be rejected is rejected before the container is built,
// because the cheapest training run is the one that does not start: the
// expensive part here is measured in minutes, and a misnamed base model or an
// unpaired image is knowable in milliseconds.
//
// The checks run cheapest first, and the base model is checked last for that
// reason alone: it is the only one that needs the image assembled to answer,
// so a directory that is not a training set says so without building anything.
func (tr *Training) train(ctx context.Context) (*dagger.Container, string, error) {
	iterations, err := tr.iterations()
	if err != nil {
		return nil, "", err
	}
	pairs, err := tr.pairs(ctx)
	if err != nil {
		return nil, "", err
	}
	boxes, err := tr.boxes(ctx, pairs)
	if err != nil {
		return nil, "", err
	}
	base, err := tr.baseModel(ctx)
	if err != nil {
		return nil, "", err
	}

	output := outputDir + "/" + base + traineddataExt
	ctr := tr.Tesseract.Container().
		// The box files have to sit beside the images: tesseract resolves a
		// training box relative to the image it was handed, not relative to
		// the output base. Merging them into the source directory is what puts
		// them there without writing into the caller's own directory.
		WithMountedDirectory(trainingSourceDir, tr.Source.WithDirectory("/", boxes)).
		WithMountedFile(trainingManifestPath, trainingManifest(pairs)).
		WithMountedFile(trainingListPath, trainingList(pairs))

	exec, err := tr.exec(ctx, ctr, []string{
		"sh", "-c", trainingScript, trainingArgv0,
		tr.Tesseract.datadir(), base, strconv.Itoa(iterations), output,
	})
	if err != nil {
		return nil, "", err
	}
	return exec, output, nil
}

// exec runs one step of the training pipeline, translating the one failure
// this module can explain better than the tool can.
func (tr *Training) exec(ctx context.Context, ctr *dagger.Container, args []string) (*dagger.Container, error) {
	exec, err := execTool(ctx, ctr, args, "training")
	if err == nil || !strings.Contains(err.Error(), integerModelMarker) {
		return exec, err
	}
	return nil, fmt.Errorf(
		"WithBaseModel: %q is a quantized model, which lstmtraining cannot fine-tune: the models in tesseract-ocr/tessdata — which is every model Alpine packages — are integerized for recognition speed and carry no trainable weights. Fetch the same language from tesseract-ocr/tessdata_best, add it with WithTessdata, and name that model here instead:\n%w",
		tr.BaseModel, err)
}

// baseModel resolves and validates the model fine-tuning continues from.
func (tr *Training) baseModel(ctx context.Context) (string, error) {
	name := strings.TrimSpace(tr.BaseModel)
	if name == "" {
		return "", fmt.Errorf(
			"Training: a base model is required: call WithBaseModel with a float model from tesseract-ocr/tessdata_best supplied through WithTessdata; there is no default because every model Alpine packages is quantized and cannot be fine-tuned")
	}
	ok, err := tr.Tesseract.hasModel(ctx, name)
	if err != nil {
		return "", err
	}
	if ok {
		return name, nil
	}
	installed, err := tr.Tesseract.Langs(ctx)
	if err != nil {
		return "", err
	}
	return "", fmt.Errorf(
		"WithBaseModel: model %q is not installed: installed models are %s; supply it with WithTessdata, which is where a float model from tesseract-ocr/tessdata_best has to come from anyway",
		name, strings.Join(installed, ", "))
}

// iterations resolves the training length, rejecting a count that would leave
// lstmtraining with no checkpoint to freeze.
func (tr *Training) iterations() (int, error) {
	if tr.Iterations == 0 {
		return defaultTrainingIterations, nil
	}
	if tr.Iterations < 0 {
		return 0, fmt.Errorf("WithIterations: iterations must be positive, got %d", tr.Iterations)
	}
	return tr.Iterations, nil
}

// pairs resolves the source directory into the training samples it holds, and
// reports every reason it is not a training set: an image with no ground
// truth, a ground truth with no image, two images that would share one, or
// nothing usable at all.
//
// Anything that is neither an image nor a `.gt.txt` is ignored rather than
// rejected, the same way Batch ignores the READMEs and checksum files a real
// directory collects.
func (tr *Training) pairs(ctx context.Context) ([]trainingPair, error) {
	found, err := tr.Source.Glob(ctx, defaultBatchGlob)
	if err != nil {
		return nil, fmt.Errorf("Training: list the source directory: %w", err)
	}

	var (
		images  = make(map[string]string)
		truths  = make(map[string]string)
		ignored int
	)
	for _, p := range found {
		// Glob reports directories too, with a trailing separator.
		if strings.HasSuffix(p, "/") {
			continue
		}
		switch {
		case strings.HasSuffix(p, groundTruthExt):
			truths[strings.TrimSuffix(p, groundTruthExt)] = p
		case isImagePath(p):
			if err := checkManifestSafe(p); err != nil {
				return nil, fmt.Errorf("Training: %w: rename it", err)
			}
			base := outputBaseFor(p)
			if first, dup := images[base]; dup {
				return nil, fmt.Errorf(
					"Training: %q and %q both pair with %q: one image per ground truth, so rename one of them",
					first, p, base+groundTruthExt)
			}
			images[base] = p
		default:
			ignored++
		}
	}

	bases := make([]string, 0, len(images))
	for base := range images {
		bases = append(bases, base)
	}
	sort.Strings(bases)

	pairs := make([]trainingPair, 0, len(bases))
	for _, base := range bases {
		if _, ok := truths[base]; !ok {
			return nil, fmt.Errorf(
				"Training: image %q has no ground truth: every image needs a %q beside it holding the single line of text it renders",
				images[base], base+groundTruthExt)
		}
		delete(truths, base)
		pairs = append(pairs, trainingPair{image: images[base], base: base})
	}
	if len(truths) > 0 {
		orphans := make([]string, 0, len(truths))
		for _, p := range truths {
			orphans = append(orphans, p)
		}
		sort.Strings(orphans)
		return nil, fmt.Errorf(
			"Training: ground truth with no image: %s; each one needs an image of the same name beside it, in one of %s",
			strings.Join(orphans, ", "), strings.Join(imageExtensions, " "))
	}
	if len(pairs) == 0 {
		if ignored > 0 {
			return nil, fmt.Errorf(
				"Training: the source directory holds no image and ground-truth pairs: %d file(s) were neither an image nor a %q",
				ignored, groundTruthExt)
		}
		return nil, fmt.Errorf("Training: the source directory is empty")
	}
	return pairs, nil
}

// boxes renders the box file each training sample needs, as a directory
// mirroring the source layout.
//
// A box file is how the ground truth reaches tesseract: `lstm.train` takes the
// text from a box beside the image rather than from the image's own name, in
// the WordStr format, which pairs one line of text with the region of the
// image it occupies. Since each image here *is* one line, that region is the
// whole image — so the only thing that has to be discovered is how big the
// image is.
//
// It is discovered here, in Go, rather than in the container, because nothing
// on the toolchain image reports an image's dimensions: tesseract will, but
// only by recognising the page first, and everything else means installing an
// image toolkit to read two integers out of a header. Exporting the source
// once and reading the headers costs a single round trip for the whole set.
func (tr *Training) boxes(ctx context.Context, pairs []trainingPair) (*dagger.Directory, error) {
	local, err := os.MkdirTemp(".", "tesseract-training-")
	if err != nil {
		return nil, fmt.Errorf("Training: stage the source directory: %w", err)
	}
	defer os.RemoveAll(local)

	if _, err := tr.Source.Export(ctx, local); err != nil {
		return nil, fmt.Errorf("Training: stage the source directory: %w", err)
	}

	contents := make([]string, 0, len(pairs))
	for _, p := range pairs {
		truth := p.base + groundTruthExt
		text, err := groundTruthLine(filepath.Join(local, filepath.FromSlash(truth)), truth)
		if err != nil {
			return nil, err
		}
		width, height, err := imageSize(filepath.Join(local, filepath.FromSlash(p.image)))
		if err != nil {
			return nil, fmt.Errorf("Training: read the dimensions of %q: %w", p.image, err)
		}
		contents = append(contents, wordStrBox(width, height, text))
	}
	return workdirBoxes(pairs, contents)
}

// workdirBoxes writes the rendered box files into the module's own workdir and
// hands the directory back as a Dagger one.
//
// The directory name is a digest of what goes in it, so the same training set
// resolves to the same path however many times it is asked for, and the whole
// tree is moved into place in one rename — two runs racing on the same set
// therefore either see a complete directory or none at all, never a half
// written one.
func workdirBoxes(pairs []trainingPair, contents []string) (*dagger.Directory, error) {
	sum := sha256.New()
	for i, p := range pairs {
		fmt.Fprintf(sum, "%s\x00%s\x00", p.base, contents[i])
	}
	root := "tesseract-boxes-" + hex.EncodeToString(sum.Sum(nil)[:8])
	if _, err := os.Stat(root); err == nil {
		return dag.CurrentModule().Workdir(root), nil
	}

	staging, err := os.MkdirTemp(".", "tesseract-boxes-")
	if err != nil {
		return nil, fmt.Errorf("Training: write the box files: %w", err)
	}
	for i, p := range pairs {
		full := filepath.Join(staging, filepath.FromSlash(p.base)+boxExt)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			os.RemoveAll(staging)
			return nil, fmt.Errorf("Training: write the box files: %w", err)
		}
		if err := os.WriteFile(full, []byte(contents[i]), 0o600); err != nil {
			os.RemoveAll(staging)
			return nil, fmt.Errorf("Training: write %q: %w", p.base+boxExt, err)
		}
	}
	if err := os.Rename(staging, root); err != nil {
		defer os.RemoveAll(staging)
		// A concurrent run that won the race left the same content behind, so
		// a rename that failed onto an existing directory is not a failure.
		if _, statErr := os.Stat(root); statErr != nil {
			return nil, fmt.Errorf("Training: write the box files: %w", err)
		}
	}
	return dag.CurrentModule().Workdir(root), nil
}

// trainingManifest renders the file the lstmf pass loops over: one record per
// sample, holding the image, the output base its `.lstmf` is written to, and
// the directory to create for it. All three are absolute container paths, as
// in Batch, so the loop needs no path arithmetic of its own.
func trainingManifest(pairs []trainingPair) *dagger.File {
	var sb strings.Builder
	for _, p := range pairs {
		out := trainingLstmfDir + "/" + p.base
		fmt.Fprintf(&sb, "%s\t%s\t%s\n", trainingSourceDir+"/"+p.image, out, path.Dir(out))
	}
	name := path.Base(trainingManifestPath)
	return dag.Directory().WithNewFile(name, sb.String()).File(name)
}

// trainingList renders the sample list lstmtraining and lstmeval both read.
// It is written here rather than globbed in the container so the samples are
// presented in a fixed order: training is sensitive to the order it sees its
// data in, and a glob's order is the filesystem's business.
func trainingList(pairs []trainingPair) *dagger.File {
	var sb strings.Builder
	for _, p := range pairs {
		fmt.Fprintf(&sb, "%s\n", trainingLstmfDir+"/"+p.base+lstmfExt)
	}
	name := path.Base(trainingListPath)
	return dag.Directory().WithNewFile(name, sb.String()).File(name)
}

// wordStrBox renders the one-line WordStr box that pairs a line of ground
// truth with the image rendering it. The region is the whole image, which is
// what makes "one image, one line" the contract rather than a convention.
func wordStrBox(width int, height int, text string) string {
	return fmt.Sprintf("WordStr 0 0 %d %d 0 #%s\n", width, height, text)
}

// groundTruthBox renders the box file for a single image and the line it
// renders, for the one-image path Document.LstmTrain takes. Training's own
// path builds a whole directory of these from one export rather than one file
// at a time.
func groundTruthBox(ctx context.Context, source *dagger.File, text string) (*dagger.File, error) {
	line := strings.TrimSpace(text)
	if line == "" {
		return nil, fmt.Errorf("LstmTrain: groundTruth is required: it is the line of text the image renders")
	}
	if strings.Contains(line, "\n") {
		return nil, fmt.Errorf(
			"LstmTrain: groundTruth holds more than one line: a training sample is one image of one text line, so crop the image to a line and pass that line's text")
	}

	local, err := os.MkdirTemp(".", "tesseract-sample-")
	if err != nil {
		return nil, fmt.Errorf("LstmTrain: stage the source image: %w", err)
	}
	defer os.RemoveAll(local)

	staged := filepath.Join(local, "source")
	if _, err := source.Export(ctx, staged); err != nil {
		return nil, fmt.Errorf("LstmTrain: stage the source image: %w", err)
	}
	width, height, err := imageSize(staged)
	if err != nil {
		return nil, fmt.Errorf("LstmTrain: read the source image dimensions: %w", err)
	}

	name := path.Base(outputBase) + boxExt
	return dag.Directory().WithNewFile(name, wordStrBox(width, height, line)).File(name), nil
}

// groundTruthLine reads a `.gt.txt` and returns the single line of text it
// holds.
//
// More than one line is refused rather than joined. A box covering the whole
// image says "this image is this text", and against a two-line image that
// claim is false in a way training cannot recover from: the network is shown
// two lines of pixels and told they are one line of characters, and learns
// from it.
func groundTruthLine(local string, name string) (string, error) {
	raw, err := os.ReadFile(local)
	if err != nil {
		return "", fmt.Errorf("Training: read %q: %w", name, err)
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	switch len(lines) {
	case 0:
		return "", fmt.Errorf(
			"Training: ground truth %q is empty: it has to hold the line of text its image renders", name)
	case 1:
		return lines[0], nil
	default:
		return "", fmt.Errorf(
			"Training: ground truth %q holds %d lines: a training sample is one image of one text line, so split the image into one file per line",
			name, len(lines))
	}
}

// imageSize reports an image's pixel dimensions from its header alone.
//
// The registered decoders cover every format the toolchain image can read but
// the PNM family, which no Go decoder ships and which pnmSize handles instead
// — so what this module accepts as a training image is the same set it accepts
// as a batch image, rather than a smaller one nobody documented.
func imageSize(local string) (int, int, error) {
	f, err := os.Open(local)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	buf := bufio.NewReader(f)
	if width, height, ok, err := pnmSize(buf); ok || err != nil {
		return width, height, err
	}
	cfg, _, err := image.DecodeConfig(buf)
	if err != nil {
		return 0, 0, fmt.Errorf("decode the image header: %w", err)
	}
	return cfg.Width, cfg.Height, nil
}

// pnmSize reads the dimensions out of a PBM/PGM/PPM header, and reports
// whether the file was one at all. The header is the magic number followed by
// two decimal integers, separated by any whitespace and interrupted by `#`
// comments running to end of line.
func pnmSize(buf *bufio.Reader) (int, int, bool, error) {
	magic, err := buf.Peek(2)
	if err != nil || magic[0] != 'P' || magic[1] < '1' || magic[1] > '6' {
		return 0, 0, false, nil
	}
	if _, err := buf.Discard(2); err != nil {
		return 0, 0, true, err
	}
	width, err := pnmInt(buf)
	if err != nil {
		return 0, 0, true, err
	}
	height, err := pnmInt(buf)
	if err != nil {
		return 0, 0, true, err
	}
	return width, height, true, nil
}

// pnmInt reads the next decimal integer of a PNM header, skipping the
// whitespace and comments that may precede it.
func pnmInt(buf *bufio.Reader) (int, error) {
	var digits []byte
	for {
		c, err := buf.ReadByte()
		if err != nil {
			// A header that ends the file right after its last digit is
			// malformed but readable, and refusing it here would reject an
			// image tesseract is perfectly happy with.
			if errors.Is(err, io.EOF) && len(digits) > 0 {
				return strconv.Atoi(string(digits))
			}
			return 0, fmt.Errorf("read the PNM header: %w", err)
		}
		switch {
		case c == '#':
			for c != '\n' {
				if c, err = buf.ReadByte(); err != nil {
					return 0, fmt.Errorf("read the PNM header: %w", err)
				}
			}
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			if len(digits) > 0 {
				return strconv.Atoi(string(digits))
			}
		case c >= '0' && c <= '9':
			digits = append(digits, c)
		default:
			return 0, fmt.Errorf("read the PNM header: unexpected byte %q", c)
		}
	}
}
