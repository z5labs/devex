package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"dagger/tesseract/internal/dagger"
)

// options is the recognition option set that applies to a unit of work,
// whatever that unit is: one image for Document, a whole directory for Batch.
//
// It exists as its own type so those two cannot drift. Every option is
// expressed exactly once here — as a builder, as a piece of argv, and as a
// deferred check — and Document and Batch each hold one of these and forward to
// it. Adding an option means touching this file plus one three-line forwarder
// per unit of work, rather than reimplementing the flag and its validation.
//
// It is a plain struct rather than a module object: Dagger surfaces it as a
// `+private` field, so it round-trips as part of its owner's state and never
// appears in the API. Callers still see the builders on Document and Batch,
// which is where they read naturally.
type options struct {
	Language     string
	PageSeg      PageSegMode
	Engine       EngineMode
	Dpi          int
	HasDpi       bool
	ParamNames   []string
	ParamValues  []string
	UserWords    *dagger.File
	UserPatterns *dagger.File
}

// The builders are value-in, value-out: the owner copies itself and swaps its
// options field, which is what makes a configured Document or Batch safe to
// branch into several outputs without the branches interfering.

func (o options) withLanguage(lang string) options {
	out := o.clone()
	out.Language = lang
	return out
}

func (o options) withPageSegmentation(mode PageSegMode) options {
	out := o.clone()
	out.PageSeg = mode
	return out
}

func (o options) withEngine(mode EngineMode) options {
	out := o.clone()
	out.Engine = mode
	return out
}

func (o options) withDpi(dpi int) options {
	out := o.clone()
	out.Dpi = dpi
	out.HasDpi = true
	return out
}

func (o options) withParameter(name string, value string) options {
	out := o.clone()
	out.ParamNames = append(out.ParamNames, name)
	out.ParamValues = append(out.ParamValues, value)
	return out
}

func (o options) withUserWords(words *dagger.File) options {
	out := o.clone()
	out.UserWords = words
	return out
}

func (o options) withUserPatterns(patterns *dagger.File) options {
	out := o.clone()
	out.UserPatterns = patterns
	return out
}

// clone copies the slices as well as the struct, so appending a parameter to
// one branch cannot be seen by another that shares the same backing array.
func (o options) clone() options {
	out := o
	out.ParamNames = append([]string(nil), o.ParamNames...)
	out.ParamValues = append([]string(nil), o.ParamValues...)
	return out
}

// mount attaches whatever the options reference from the host: the user word
// and pattern lists, at the paths flags names them by.
func (o options) mount(ctr *dagger.Container) *dagger.Container {
	if o.UserWords != nil {
		ctr = ctr.WithMountedFile(userWordsPath, o.UserWords)
	}
	if o.UserPatterns != nil {
		ctr = ctr.WithMountedFile(userPatternsPath, o.UserPatterns)
	}
	return ctr
}

// flags renders everything between `tesseract IMAGE OUTPUTBASE` and the
// trailing configfiles. The split matters: tesseract stops parsing flags at the
// first configfile, so a `-l` emitted after one is read as a file name to open.
func (o options) flags(t *Tesseract) ([]string, error) {
	args := append([]string(nil), t.tessdataArgs()...)
	args = append(args, "-l", o.language(t))
	if o.PageSeg != "" {
		tok, ok := o.PageSeg.token()
		if !ok {
			return nil, fmt.Errorf("WithPageSegmentation: unknown mode %q", string(o.PageSeg))
		}
		args = append(args, "--psm", tok)
	}
	if o.Engine != "" {
		tok, ok := o.Engine.token()
		if !ok {
			return nil, fmt.Errorf("WithEngine: unknown mode %q", string(o.Engine))
		}
		args = append(args, "--oem", tok)
	}
	if o.HasDpi {
		args = append(args, "--dpi", strconv.Itoa(o.Dpi))
	}
	if o.UserWords != nil {
		args = append(args, "--user-words", userWordsPath)
	}
	if o.UserPatterns != nil {
		args = append(args, "--user-patterns", userPatternsPath)
	}
	for i, name := range o.ParamNames {
		args = append(args, "-c", name+"="+o.ParamValues[i])
	}
	return args, nil
}

// language resolves the `-l` value: the caller's selection, or the first
// installed language. Defaulting to the installed set rather than omitting the
// flag matters when English was not installed — tesseract's own default is
// "eng", which would fail to load.
func (o options) language(t *Tesseract) string {
	if strings.TrimSpace(o.Language) != "" {
		return o.Language
	}
	return t.Languages[0]
}

// validate reports every deferred builder check.
//
// The checks live here rather than in the builders because a builder has no
// error return, and because some of them (is this language installed? is this a
// real control variable?) need the assembled image itself to answer.
func (o options) validate(ctx context.Context, t *Tesseract) error {
	if err := o.validateDpi(); err != nil {
		return err
	}
	if err := o.validateLanguage(ctx, t); err != nil {
		return err
	}
	return o.validateParameters(ctx, t)
}

// validateDpi rejects a non-positive `--dpi`, which tesseract would take as a
// real resolution and scale its analysis by.
func (o options) validateDpi() error {
	if o.HasDpi && o.Dpi <= 0 {
		return fmt.Errorf("WithDpi: dpi must be positive, got %d", o.Dpi)
	}
	return nil
}

// validateLanguage checks every `+`-joined code against what the image
// actually carries. tesseract does fail on an unknown language, but its error
// talks about traineddata paths and TESSDATA_PREFIX rather than about the
// language set this module built the image with.
func (o options) validateLanguage(ctx context.Context, t *Tesseract) error {
	if strings.TrimSpace(o.Language) == "" {
		return nil
	}
	installed, err := t.Langs(ctx)
	if err != nil {
		return err
	}
	for _, lang := range strings.Split(o.Language, "+") {
		lang = strings.TrimSpace(lang)
		if lang == "" {
			return fmt.Errorf(
				"WithLanguage: empty language code in %q: installed languages are %s",
				o.Language, strings.Join(installed, ", "))
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
func (o options) validateParameters(ctx context.Context, t *Tesseract) error {
	for _, name := range o.ParamNames {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("WithParameter: parameter name is required")
		}
		if strings.Contains(name, "=") {
			return fmt.Errorf("WithParameter: parameter name %q must not contain %q", name, "=")
		}
	}
	if len(o.ParamNames) == 0 {
		return nil
	}
	known, err := t.Parameters(ctx)
	if err != nil {
		return err
	}
	names := parseParameterNames(known)
	for _, name := range o.ParamNames {
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
