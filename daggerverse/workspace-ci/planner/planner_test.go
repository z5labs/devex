package planner

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSelfCheck and TestHashSelfCheck run the fixtures the module exposes as a
// check, so a regression shows up in `go test` too rather than only in a CI leg.
func TestSelfCheck(t *testing.T) {
	if err := SelfCheck(); err != nil {
		t.Fatal(err)
	}
}

func TestHashSelfCheck(t *testing.T) {
	if err := HashSelfCheck(); err != nil {
		t.Fatal(err)
	}
}

// TestKebab pins dagger's casing rule, which is what maps a toolchain name onto
// the binding file dagger generates for it. The digit boundary is the one that
// surprises: toolchain z5labs-tests becomes z-5-labs-tests.gen.go.
func TestKebab(t *testing.T) {
	for in, want := range map[string]string{
		"z5labs-tests": "z-5-labs-tests",
		"pdf-tests":    "pdf-tests",
		"workspace-ci": "workspace-ci",
		"RootOk":       "root-ok",
		"HTTPServer":   "http-server",
	} {
		if got := Kebab(in); got != want {
			t.Errorf("Kebab(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAggregatorBindings pins that a toolchain's binding resolves to the toolchain
// and that the core binding, which no toolchain owns, does not.
func TestAggregatorBindings(t *testing.T) {
	got := AggregatorBindings("ci", map[string]string{
		"z5labs-tests": "daggerverse/z5labs/tests",
		"dagger":       "daggerverse/dagger/tests",
	})
	if dir := got["ci/internal/dagger/z-5-labs-tests.gen.go"]; dir != "daggerverse/z5labs/tests" {
		t.Errorf("the z5labs-tests binding resolved to %q", dir)
	}
	if _, ok := got[CoreBinding("ci")]; ok {
		t.Error("the core binding was attributed to a toolchain")
	}
}

// TestParseTimeouts pins that a table which cannot be read is an error rather than
// a silent fallback to the default.
func TestParseTimeouts(t *testing.T) {
	got, err := ParseTimeouts(`{"daggerverse/pdf/tests:all": 12}`)
	if err != nil {
		t.Fatal(err)
	}
	if got["daggerverse/pdf/tests:all"] != 12 {
		t.Errorf("parsed %v", got)
	}
	if empty, err := ParseTimeouts(""); err != nil || len(empty) != 0 {
		t.Errorf("an empty table parsed to %v, %v", empty, err)
	}
	for _, bad := range []string{`{`, `[]`, `{"x": 0}`, `{"x": -1}`} {
		if _, err := ParseTimeouts(bad); err == nil {
			t.Errorf("%q was accepted as a timeout table", bad)
		}
	}
}

// TestTimeoutsApply pins the precedence: a leg's own name beats its module, and
// both beat the default.
func TestTimeoutsApply(t *testing.T) {
	entries := []Entry{CheckEntry("mods/a", "a", "ok"), CheckEntry("mods/a", "a", "slow"), ModuleEntry("mods/b")}
	got := Timeouts{"mods/a": 9, "mods/a:slow": 20}.Apply(entries, 6)
	want := map[string]int{"mods/a:ok": 9, "mods/a:slow": 20, "mods/b": 6}
	for _, e := range got {
		if e.Timeout != want[e.Name] {
			t.Errorf("%q got a step budget of %d, want %d", e.Name, e.Timeout, want[e.Name])
		}
		if e.JobTimeout != e.Timeout+JobTimeoutHeadroom {
			t.Errorf("%q got a job budget of %d, want %d", e.Name, e.JobTimeout, e.Timeout+JobTimeoutHeadroom)
		}
	}
}

// TestRender pins the two formats and, above all, that an empty plan renders as an
// empty array: `null` survives a workflow's non-empty test and then breaks
// fromJSON.
func TestRender(t *testing.T) {
	empty, err := Render(nil, FormatGithubActions)
	if err != nil {
		t.Fatal(err)
	}
	if empty != "[]" {
		t.Errorf("an empty plan rendered as %q", empty)
	}

	entries := []Entry{CheckEntry("mods/a", "a", "ok")}
	oneLine, err := Render(entries, FormatGithubActions)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(oneLine, "\n") {
		t.Errorf("the github-actions form spans more than one line: %q", oneLine)
	}
	var back []Entry
	if err := json.Unmarshal([]byte(oneLine), &back); err != nil {
		t.Fatalf("the github-actions form is not a JSON array: %v", err)
	}

	canonical, err := Render(entries, FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(canonical, "\n") {
		t.Errorf("the canonical form is not indented: %q", canonical)
	}
	if _, err := Render(entries, Format("yaml")); err == nil {
		t.Error("an unknown format was accepted")
	}
}
