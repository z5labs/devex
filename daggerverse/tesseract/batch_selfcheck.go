package main

import (
	"errors"
	"fmt"
	"strings"
)

// batchSelfCheck verifies the two halves of a slice's script that no directory
// of images can exercise: that the per-image invocations survive being
// concatenated into one exec, and that an image's failure is still readable back
// out of what that exec printed.
//
// It sits beside the fanout package's self-check and for a related reason. A
// fixture can show that *an* unreadable image fails the batch by name —
// BatchConcurrencyReportsFailingImage does — but not that every image in a slice
// carries its own position, which is what a twelve-image slice failing on its
// fourth depends on. Nor can a fixture show what quoting does to a path a caller
// could write and this suite will not commit: a file name holding a quote, a
// space or a `$`.
//
// Unlike fanout's, these cases cannot also be a `go test`: they read unexported
// values of package main, and package main imports the generated client, whose
// package initializer panics outside a Dagger session. The check is the only way
// to run them, which is why it is one.
func batchSelfCheck() error {
	var errs []error
	for _, c := range batchSelfCheckCases() {
		if err := c.run(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.name, err))
		}
	}
	return errors.Join(errs...)
}

type batchSelfCheckCase struct {
	name string
	run  func() error
}

func batchSelfCheckCases() []batchSelfCheckCase {
	return []batchSelfCheckCase{
		{"a slice's script creates every output directory once, above the recognitions", checkSliceScriptShape},
		{"every image's invocation is guarded by its own position", checkSliceScriptNamesEveryImage},
		{"every word of an invocation is quoted", checkSliceScriptQuotesEveryWord},
		{"a reported image is read back and taken out of stderr", checkFailedImageIsReadBack},
		{"stderr with no report yields no image", checkNoReportYieldsNoImage},
	}
}

// sliceScriptFixture is the shape recognise hands the script builder: a slice of
// images at two depths, and the flags every recognition in the batch shares.
func sliceScriptFixture() (slice []string, args []string) {
	return []string{"scans/page-1.png", "scans/page-2.png", "scans/deep/page-3.tif"},
		[]string{"-l", "eng", "txt"}
}

// checkSliceScriptShape pins that the output directories are created once
// however many images share the exec, and that they are created before any
// recognition writes into them. tesseract will not create an output base's
// directory, so a mkdir that moved below a recognition would fail that
// recognition rather than being harmless.
func checkSliceScriptShape() error {
	slice, args := sliceScriptFixture()
	script := sliceScript(slice, args)

	mkdir := shellCommand(append([]string{"mkdir", "-p"}, outputDirsFor(slice)...))
	if n := strings.Count(script, "\n"+mkdir+"\n"); n != 1 {
		return fmt.Errorf("expected the mkdir %q exactly once, got %d:\n%s", mkdir, n, script)
	}
	for _, want := range []string{"'/out/scans'", "'/out/scans/deep'"} {
		if !strings.Contains(mkdir, want) {
			return fmt.Errorf("expected the mkdir to create %s, got %q", want, mkdir)
		}
	}

	at := make([]int, 0, len(slice))
	for _, source := range slice {
		i := strings.Index(script, shellQuote(batchSourceDir+"/"+source))
		if i < 0 {
			return fmt.Errorf("expected %q to be recognised in the script:\n%s", source, script)
		}
		at = append(at, i)
	}
	if at[0] < strings.Index(script, mkdir) {
		return fmt.Errorf("expected the mkdir above the first recognition:\n%s", script)
	}
	for i := 1; i < len(at); i++ {
		if at[i] <= at[i-1] {
			return fmt.Errorf(
				"expected %q to be recognised after %q, got the reverse:\n%s",
				slice[i], slice[i-1], script)
		}
	}
	return nil
}

// checkSliceScriptNamesEveryImage is the property a failure's message rests on:
// every invocation is followed by the guard that reports *its* position in the
// slice, so a slice of twelve images that fails on the fourth says the fourth.
func checkSliceScriptNamesEveryImage() error {
	slice, args := sliceScriptFixture()
	script := sliceScript(slice, args)

	for i, source := range slice {
		want := fmt.Sprintf("%s || %s %d $?", shellCommand(append([]string{
			"tesseract",
			batchSourceDir + "/" + source,
			outputDir + "/" + outputBaseFor(source),
		}, args...)), imageFailureFn, i)
		if !strings.Contains(script, want) {
			return fmt.Errorf("expected the script to carry %q:\n%s", want, script)
		}
	}
	if !strings.Contains(script, imageFailureFn+"() {") {
		return fmt.Errorf("expected the script to define %s:\n%s", imageFailureFn, script)
	}
	// The guard re-raises tesseract's own status rather than one of this
	// module's, which is what keeps `exit 1` tesseract's 1.
	if !strings.Contains(script, `exit "$2"`) {
		return fmt.Errorf("expected the guard to re-raise the invocation's exit status:\n%s", script)
	}
	return nil
}

// checkSliceScriptQuotesEveryWord is what stands between a caller's file name or
// parameter value and the shell. Both reach the script as words of a command
// this module joins, and neither is anything this module generated.
func checkSliceScriptQuotesEveryWord() error {
	slice := []string{`scans/o'brien; rm -rf $HOME.png`}
	args := []string{"-c", "tessedit_char_whitelist=$(id)"}
	script := sliceScript(slice, args)

	for _, word := range []string{
		batchSourceDir + "/" + slice[0],
		outputDir + "/" + outputBaseFor(slice[0]),
		args[1],
	} {
		if !strings.Contains(script, shellQuote(word)) {
			return fmt.Errorf("expected %q to be quoted in the script:\n%s", word, script)
		}
	}
	if !strings.Contains(script, `'\''brien`) {
		return fmt.Errorf("expected the embedded quote to be escaped:\n%s", script)
	}

	// The assertion that actually covers a word this check did not think of:
	// scan the assembled command and require that everything *outside* a quoted
	// literal is a word separator. A `;`, a `$` or an unbalanced quote reaching
	// the shell would show up here whatever it was spelled as. The guard suffix
	// is deliberately not scanned — its `||` and `$?` are this module's own and
	// are meant to reach the shell.
	command := shellCommand(append([]string{
		"tesseract",
		batchSourceDir + "/" + slice[0],
		outputDir + "/" + outputBaseFor(slice[0]),
	}, args...))
	if loose, err := unquoted(command); err != nil {
		return fmt.Errorf("%w:\n%s", err, command)
	} else if strings.Trim(loose, " ") != "" {
		return fmt.Errorf("expected only separators outside the quoted literals, got %q:\n%s",
			loose, command)
	}
	return nil
}

// unquoted returns everything in a shell command that sits outside a
// single-quoted literal, and reports a literal that is never closed.
//
// Inside single quotes the shell interprets nothing, so what is left over is the
// whole of what it can act on. The one escape shellQuote emits — `'\''`, which
// closes the literal, emits a backslash-escaped quote and reopens it — is
// recognised and contributes nothing.
func unquoted(command string) (string, error) {
	var (
		loose  strings.Builder
		inside bool
	)
	for i := 0; i < len(command); i++ {
		switch c := command[i]; {
		case c == '\'':
			inside = !inside
		case inside:
			// Literal text; the shell never sees it as anything else.
		case c == '\\' && i+1 < len(command) && command[i+1] == '\'':
			i++
		default:
			loose.WriteByte(c)
		}
	}
	if inside {
		return loose.String(), errors.New("expected every quoted literal to be closed")
	}
	return loose.String(), nil
}

// checkFailedImageIsReadBack asserts the round trip the message depends on: the
// image comes back as a position, and the line carrying it does not come back as
// part of what the caller reads.
func checkFailedImageIsReadBack() error {
	stderr := strings.Join([]string{
		"Error in pixReadStream: Pdf reading is not supported",
		imageFailureMarker + "0",
		"",
	}, "\n")

	at, rest := takeFailedImage(stderr)
	if at != 0 {
		return fmt.Errorf("expected position 0, got %d", at)
	}
	if strings.Contains(rest, imageFailureMarker) {
		return fmt.Errorf("expected the report to be taken out of stderr, got:\n%s", rest)
	}
	if !strings.Contains(rest, "pixReadStream") {
		return fmt.Errorf("expected tesseract's own message to survive, got:\n%s", rest)
	}
	return nil
}

// checkNoReportYieldsNoImage asserts the other half: a failure that never
// reached an image reports no position, so the message names the run of images
// rather than inventing one of them.
func checkNoReportYieldsNoImage() error {
	for _, stderr := range []string{
		"",
		"mkdir: can't create directory '/out': Read-only file system",
		imageFailureMarker + "not-a-number",
		imageFailureMarker + "-1",
	} {
		if at, _ := takeFailedImage(stderr); at != -1 {
			return fmt.Errorf("expected no image from %q, got %d", stderr, at)
		}
	}
	return nil
}
