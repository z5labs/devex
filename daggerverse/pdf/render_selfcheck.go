package main

import (
	"errors"
	"fmt"
	"strings"
)

// renderSelfCheck verifies the two halves of a slice's script that no document
// can exercise: that the per-page commands survive being concatenated into one
// exec, and that a page's failure is still readable back out of what that exec
// printed.
//
// It sits beside the fanout package's self-check and for the same reason.
// poppler will not fail a page — measured against the pinned poppler 25.12,
// every damaged page shape is a warning on stderr and an exit status of 0 — so
// no fixture makes one page of a slice fail, and the reporting has to be checked
// where it is built instead. The script assembly is checked here rather than
// through a render because a passing render proves the commands ran, not that
// each one is still the page's own invocation.
//
// Unlike fanout's, these cases cannot also be a `go test`: they read unexported
// values of package main, and package main imports the generated client, whose
// package initializer panics outside a Dagger session. The check is the only way
// to run them, which is why it is one.
func renderSelfCheck() error {
	var errs []error
	for _, c := range renderSelfCheckCases() {
		if err := c.run(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.name, err))
		}
	}
	return errors.Join(errs...)
}

type renderSelfCheckCase struct {
	name string
	run  func() error
}

func renderSelfCheckCases() []renderSelfCheckCase {
	return []renderSelfCheckCase{
		{"a slice's script runs the prelude once and every page's command in order", checkSliceScriptShape},
		{"every page's command is guarded by its own page number", checkSliceScriptNamesEveryPage},
		{"a reported page is read back and taken out of stderr", checkFailedPageIsReadBack},
		{"stderr with no report yields no page", checkNoReportYieldsNoPage},
	}
}

// sliceScriptFixture is the shape a family hands render: a prelude and three
// pages' commands. The commands are not poppler's, deliberately — what is being
// checked is that whatever a family generated survives verbatim.
func sliceScriptFixture() (prelude []string, jobs []pageJob) {
	return []string{"mkdir -p /out", "cd /out"}, []pageJob{
		{page: 7, command: "render-seven"},
		{page: 8, command: "render-eight"},
		{page: 9, command: "render-nine"},
	}
}

// checkSliceScriptShape pins that the prelude is written once however many pages
// share the exec, and that the commands follow it in page order. A prelude
// repeated per page would be harmless; a prelude that moved below a command
// would render into a directory that does not exist yet.
func checkSliceScriptShape() error {
	prelude, jobs := sliceScriptFixture()
	script := sliceScript(prelude, jobs)

	for _, line := range prelude {
		if n := strings.Count(script, "\n"+line+"\n"); n != 1 {
			return fmt.Errorf("expected the prelude line %q exactly once, got %d:\n%s", line, n, script)
		}
	}
	at := make([]int, 0, len(jobs))
	for _, job := range jobs {
		i := strings.Index(script, job.command)
		if i < 0 {
			return fmt.Errorf("expected page %d's command %q in the script:\n%s", job.page, job.command, script)
		}
		at = append(at, i)
	}
	for i := 1; i < len(at); i++ {
		if at[i] <= at[i-1] {
			return fmt.Errorf(
				"expected page %d's command after page %d's, got the reverse:\n%s",
				jobs[i].page, jobs[i-1].page, script)
		}
	}
	if last := strings.Index(script, jobs[0].command); last < strings.Index(script, prelude[len(prelude)-1]) {
		return fmt.Errorf("expected the whole prelude above the first command:\n%s", script)
	}
	return nil
}

// checkSliceScriptNamesEveryPage is the property a failure's message rests on:
// every command is followed by the guard that reports *its* page, so a slice of
// twelve pages that fails on the fourth says four.
func checkSliceScriptNamesEveryPage() error {
	prelude, jobs := sliceScriptFixture()
	script := sliceScript(prelude, jobs)

	for _, job := range jobs {
		want := fmt.Sprintf("%s || %s %d $?", job.command, pageFailureFn, job.page)
		if !strings.Contains(script, want) {
			return fmt.Errorf("expected the script to carry %q:\n%s", want, script)
		}
	}
	if !strings.Contains(script, pageFailureFn+"() {") {
		return fmt.Errorf("expected the script to define %s:\n%s", pageFailureFn, script)
	}
	// The guard re-raises the tool's own status rather than one of this
	// module's, which is what keeps `exit 99` poppler's 99.
	if !strings.Contains(script, `exit "$2"`) {
		return fmt.Errorf("expected the guard to re-raise the command's exit status:\n%s", script)
	}
	return nil
}

// checkFailedPageIsReadBack asserts the round trip the message depends on: the
// page comes back as a number, and the line carrying it does not come back as
// part of what the caller reads.
func checkFailedPageIsReadBack() error {
	stderr := strings.Join([]string{
		"Syntax Error: Couldn't find trailer dictionary",
		pageFailureMarker + "413",
	}, "\n")

	page, rest := takeFailedPage(stderr)
	if page != 413 {
		return fmt.Errorf("expected page 413, got %d", page)
	}
	if strings.Contains(rest, pageFailureMarker) {
		return fmt.Errorf("expected the report to be taken out of stderr, got:\n%s", rest)
	}
	if !strings.Contains(rest, "Syntax Error") {
		return fmt.Errorf("expected poppler's own message to survive, got:\n%s", rest)
	}
	return nil
}

// checkNoReportYieldsNoPage asserts the other half: a failure that never reached
// a page reports no page, so the message names the run of pages rather than
// inventing one of them.
func checkNoReportYieldsNoPage() error {
	for _, stderr := range []string{
		"",
		"mkdir: can't create directory '/out': Read-only file system",
		pageFailureMarker + "not-a-number",
	} {
		if page, _ := takeFailedPage(stderr); page != 0 {
			return fmt.Errorf("expected no page from %q, got %d", stderr, page)
		}
	}
	return nil
}
