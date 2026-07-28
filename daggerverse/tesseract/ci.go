package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"dagger/tesseract/internal/dagger"
)

const (
	// tsvLevelColumn, tsvConfColumn and tsvTextColumn are the TSV header names
	// the gate reads its measurement out of. They are looked up by name rather
	// than by position because the column order is tesseract's to change, and a
	// gate reading the wrong column would not fail — it would pass the wrong
	// scans.
	tsvLevelColumn = "level"
	tsvConfColumn  = "conf"
	tsvTextColumn  = "text"

	// tsvWordLevel is the `level` a TSV row carries when it describes a word
	// rather than the page, block, paragraph or line containing it. Only word
	// rows carry a real confidence; every level above them reports -1.
	tsvWordLevel = "5"

	// minConfidenceFloor and minConfidenceCeiling bound the threshold.
	// tesseract's confidences are percentages, so a bar above 100 could never
	// be cleared; a bar of 0 could never be missed, which would be a gate that
	// silently does nothing rather than one that was never asked for.
	minConfidenceFloor   = 1
	minConfidenceCeiling = 100
)

// Ci is a chained builder for a document-processing pipeline: a directory of
// scans in, the archival formats out, and a quality gate in between.
//
// It composes the Batch primitive without adding capability of its own — every
// stage is a call the caller could make by hand — so a document repo's CI is one
// declarative `dagger call` rather than a recognition step, an export step and a
// hand-rolled TSV parser.
//
// The confidence gate is the part that is not merely a bundled call. It reads
// the `conf` column tesseract already reports in its TSV output and fails the
// run when mean word confidence falls below the threshold, which is how a
// scanner regression or a wrong-language configuration is caught before the
// artifacts ship rather than after they are archived.
type Ci struct {
	// +private
	Batch *Batch
	// +private
	Formats []Format
	// +private
	MinConfidence int
	// +private
	HasMinConfidence bool
}

// Ci returns a new pipeline builder over a directory of scans. Which files take
// part is the batch default: the image extensions leptonica can read, at any
// depth.
func (t *Tesseract) Ci(source *dagger.Directory) *Ci {
	return &Ci{Batch: t.Batch(source)}
}

// WithLanguage selects the recognition language (`-l`) for every image in the
// source directory. See Document.WithLanguage.
func (c *Ci) WithLanguage(lang string) *Ci {
	out := *c
	out.Batch = c.Batch.WithLanguage(lang)
	return &out
}

// WithFormats replaces the set of formats the pipeline renders for each image.
// Unset, it renders plain text.
func (c *Ci) WithFormats(
	// Output formats to render for every image in the source directory.
	formats []Format,
) *Ci {
	out := *c
	out.Formats = formats
	return &out
}

// WithMinConfidence fails the pipeline when a page comes back recognised less
// confidently than this, as a percentage from 1 to 100.
//
// The measurement is the mean of the per-word confidences tesseract already
// reports in its TSV output, taken per page: the gate names the page that
// measured worst rather than the batch, because a batch is assembled by a
// scanner and it is one sheet that goes through crooked.
//
// What it catches is the class of failure recognition does not report as one. A
// page fed sideways, a scanner drifting out of focus, a language configured
// wrong — all of them recognise *something*, exit 0 and render every artifact
// asked for. Unset, there is no bar and no page can be too poor to ship.
func (c *Ci) WithMinConfidence(
	// Lowest mean word confidence a page may be recognised at, as a percentage.
	percent int,
) *Ci {
	out := *c
	out.MinConfidence = percent
	out.HasMinConfidence = true
	return &out
}

// Check runs the pipeline's gate and produces nothing, for the PR that wants to
// know whether the scans are good enough without paying to render the archive.
//
// The gate is recognition itself plus the confidence bar: every matched image is
// recognised, so a page tesseract cannot read fails here, and when a threshold
// was set every page is measured against it. Recognition is what costs, and it
// is unavoidable — a page's confidence is not knowable without recognising it.
func (c *Ci) Check(ctx context.Context) error {
	if err := c.validate(); err != nil {
		return err
	}
	sources, err := c.Batch.matches(ctx)
	if err != nil {
		return err
	}
	// TSV whether or not a threshold was set, so the two shapes of a check are
	// one code path. It is also the cheapest renderer to discard: the gate
	// reads it, and nothing here lifts it off the exec.
	out, err := c.Batch.Export(ctx, []Format{FormatTsv})
	if err != nil {
		return err
	}
	if !c.HasMinConfidence {
		return nil
	}
	return c.gate(ctx, out, sources)
}

// Run executes the pipeline and returns every enabled format for every matched
// image, in a directory mirroring the source layout. A failing gate returns the
// error and no directory, so artifacts never reach a caller whose scans did not
// clear the bar.
//
// Gating rides along on the recognition pass that renders the artifacts rather
// than preceding it, because recognising the whole directory twice is the
// single most expensive thing this module could be asked to do, and the only
// difference between a gated run and an ungated one is a TSV. So a gated run
// renders TSV alongside whatever was enabled, measures it, and withholds the
// whole directory if the measurement fails — the artifacts exist inside the
// exec, and a caller who is refused them is no better off for them having been
// skipped.
func (c *Ci) Run(ctx context.Context) (*dagger.Directory, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	formats := c.formats()
	if !c.HasMinConfidence {
		return c.Batch.Export(ctx, formats)
	}
	sources, err := c.Batch.matches(ctx)
	if err != nil {
		return nil, err
	}
	out, err := c.Batch.Export(ctx, append(append([]Format(nil), formats...), FormatTsv))
	if err != nil {
		return nil, err
	}
	if err := c.gate(ctx, out, sources); err != nil {
		return nil, err
	}
	if containsFormat(formats, FormatTsv) {
		return out, nil
	}
	// The gate's own TSV is scaffolding, not an artifact: returning it would
	// make enabling a threshold silently change what the pipeline produces.
	return out.WithoutFiles(tsvPaths(sources)), nil
}

// validate reports the deferred builder checks, which live here because a
// builder has no error return.
func (c *Ci) validate() error {
	if !c.HasMinConfidence {
		return nil
	}
	if c.MinConfidence < minConfidenceFloor || c.MinConfidence > minConfidenceCeiling {
		return fmt.Errorf(
			"WithMinConfidence: percent must be between %d and %d, got %d: tesseract reports confidence as a percentage, so a bar of 0 passes every page and one above 100 fails every page",
			minConfidenceFloor, minConfidenceCeiling, c.MinConfidence)
	}
	return nil
}

// gate measures every page against the threshold and reports the one that
// measured worst, along with how many others joined it.
//
// The worst page rather than the first is what a caller wants to look at: a
// batch that drifted out of focus fails several pages at once, and the one to
// go and find in the stack is the one furthest below the bar.
func (c *Ci) gate(ctx context.Context, out *dagger.Directory, sources []string) error {
	var (
		worst    confidence
		failures int
	)
	for _, source := range sources {
		tsv, err := out.File(tsvPathFor(source)).Contents(ctx)
		if err != nil {
			return fmt.Errorf("Ci: read the TSV output for %q: %w", source, err)
		}
		got, err := wordConfidence(tsv)
		if err != nil {
			return fmt.Errorf("Ci: read the confidence of %q: %w", source, err)
		}
		got.source = source
		if got.score() >= float64(c.MinConfidence) {
			continue
		}
		if failures == 0 || got.score() < worst.score() {
			worst = got
		}
		failures++
	}
	if failures == 0 {
		return nil
	}
	return c.gateError(worst, failures)
}

// gateError renders the failure, which has to carry three things: what was
// measured, where, and what to do about it.
func (c *Ci) gateError(worst confidence, failures int) error {
	also := ""
	if failures > 1 {
		also = fmt.Sprintf(", the lowest of %d page(s) below it", failures)
	}
	if worst.words == 0 {
		return fmt.Errorf(
			"Ci: %q recognised no words at all, so there is no confidence to measure it against the required %d%%%s: check the page is not blank, and that the recognition language is the one it is written in",
			worst.source, c.MinConfidence, also)
	}
	return fmt.Errorf(
		"Ci: %q was recognised at a mean word confidence of %.1f%%, below the required %d%%%s: check the scan and the recognition language, or lower the bar with WithMinConfidence",
		worst.source, worst.mean, c.MinConfidence, also)
}

// formats resolves the render set. Validating it is Export's job, since that is
// where the set is turned into renderers; this only supplies the default.
//
// That default is plain text: it is what tesseract itself produces when no
// renderer is named, and the one format every downstream step can read. It is
// spelled here rather than as a named constant because a package-level constant
// of an enum type is read as another member of that enum, and a second member
// carrying "TXT" fails typedef generation outright.
func (c *Ci) formats() []Format {
	if len(c.Formats) == 0 {
		return []Format{FormatTxt}
	}
	return c.Formats
}

// confidence is one page's measurement: the mean of its word confidences, and
// how many words that mean was taken over.
type confidence struct {
	source string
	mean   float64
	words  int
}

// score is the value the threshold is compared against. A page that recognised
// nothing scores below every legal threshold rather than at zero, because the
// mean of no words is not zero — it is undefined, and it is a different fault
// with a different fix.
func (c confidence) score() float64 {
	if c.words == 0 {
		return -1
	}
	return c.mean
}

// wordConfidence measures one page out of the TSV renderer's own output.
//
// Nothing here re-implements the measurement: tesseract computes a confidence
// per recognised word and publishes it in the `conf` column, and this reads
// that column and averages it. The rows above the word level (page, block,
// paragraph, line) all report -1, so the level column is what separates a
// measurement from a placeholder.
//
// Empty text at word level is skipped for the same reason. tesseract emits word
// rows for regions it segmented but read nothing out of, and counting those as
// low-confidence words would score a page by how aggressively it was segmented.
func wordConfidence(tsv string) (confidence, error) {
	lines := strings.Split(strings.TrimRight(tsv, "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return confidence{}, fmt.Errorf("the TSV output is empty")
	}
	header := strings.Split(lines[0], "\t")
	level, err := tsvColumn(header, tsvLevelColumn)
	if err != nil {
		return confidence{}, err
	}
	conf, err := tsvColumn(header, tsvConfColumn)
	if err != nil {
		return confidence{}, err
	}
	text, err := tsvColumn(header, tsvTextColumn)
	if err != nil {
		return confidence{}, err
	}

	var got confidence
	for _, line := range lines[1:] {
		cols := strings.Split(line, "\t")
		if len(cols) != len(header) {
			continue
		}
		if cols[level] != tsvWordLevel || strings.TrimSpace(cols[text]) == "" {
			continue
		}
		value, err := strconv.ParseFloat(cols[conf], 64)
		if err != nil || value < 0 {
			continue
		}
		got.mean += value
		got.words++
	}
	if got.words > 0 {
		got.mean /= float64(got.words)
	}
	return got, nil
}

// tsvColumn locates a column by its header name, so the reader survives
// tesseract reordering or adding columns.
func tsvColumn(header []string, name string) (int, error) {
	for i, col := range header {
		if strings.TrimSpace(col) == name {
			return i, nil
		}
	}
	return 0, fmt.Errorf(
		"the TSV output has no %q column, so there is nothing to measure: its header is %q",
		name, strings.Join(header, "\t"))
}

// tsvPathFor names the TSV artifact a batch renders for one source image, and
// tsvPaths does the same for a whole batch.
func tsvPathFor(source string) string {
	return outputBaseFor(source) + formatTable[FormatTsv].ext
}

func tsvPaths(sources []string) []string {
	paths := make([]string, 0, len(sources))
	for _, source := range sources {
		paths = append(paths, tsvPathFor(source))
	}
	return paths
}

func containsFormat(formats []Format, want Format) bool {
	for _, f := range formats {
		if f == want {
			return true
		}
	}
	return false
}
