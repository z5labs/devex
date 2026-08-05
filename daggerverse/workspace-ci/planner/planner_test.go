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

// TestRenderSelfCheck runs the render fixtures the module exposes as a check. The
// golden shapes live in RenderSelfCheck rather than here so a format regression
// fails a CI leg and not only a `go test` nobody runs in CI.
func TestRenderSelfCheck(t *testing.T) {
	if err := RenderSelfCheck(); err != nil {
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

// TestIsCoarse pins the property the coarse key shape rests on: a run-everything leg
// is exactly a leg whose name is its module, and no per-check leg can be one.
func TestIsCoarse(t *testing.T) {
	if !ModuleEntry("mods/a").IsCoarse() {
		t.Error("a run-everything leg did not report as coarse")
	}
	if CheckEntry("mods/a", "a", "ok").IsCoarse() {
		t.Error("a per-check leg reported as coarse")
	}
	// The one leg name that could be mistaken for a module directory is a check
	// whose module is the workspace root, and even that keeps the colon.
	if CheckEntry(".", "ci", "generated").IsCoarse() {
		t.Error("a root-module per-check leg reported as coarse")
	}
}

// TestTimeoutsApplyCoarseKey pins the whole point of the <module-dir>:* shape (#306):
// a module whose whole suite needs a long budget can say so without dragging the
// per-check legs it contributes on the narrow path up with it.
func TestTimeoutsApplyCoarseKey(t *testing.T) {
	// A module that produces both leg shapes at once is not what a single plan
	// emits, but it is what makes the two resolutions comparable in one table.
	entries := []Entry{
		ModuleEntry("mods/a"),
		CheckEntry("mods/a", "a", "ok"),
		CheckEntry("mods/a", "a", "slow"),
		ModuleEntry("mods/b"),
	}

	for _, tc := range []struct {
		name string
		t    Timeouts
		want map[string]int
	}{
		{
			name: "the coarse key alone leaves the per-check legs on the default",
			t:    Timeouts{CoarseKey("mods/a"): 15},
			want: map[string]int{"mods/a": 15, "mods/a:ok": 6, "mods/a:slow": 6, "mods/b": 6},
		},
		{
			name: "the coarse key beats the module key on the coarse leg only",
			t:    Timeouts{"mods/a": 9, CoarseKey("mods/a"): 15},
			want: map[string]int{"mods/a": 15, "mods/a:ok": 9, "mods/a:slow": 9, "mods/b": 6},
		},
		{
			name: "a per-check key still wins on its own leg",
			t:    Timeouts{"mods/a": 9, CoarseKey("mods/a"): 15, "mods/a:slow": 20},
			want: map[string]int{"mods/a": 15, "mods/a:ok": 9, "mods/a:slow": 20, "mods/b": 6},
		},
		{
			name: "a coarse key for a module with no coarse leg reaches nothing",
			t:    Timeouts{CoarseKey("mods/a"): 15, CoarseKey("mods/c"): 30},
			want: map[string]int{"mods/a": 15, "mods/a:ok": 6, "mods/a:slow": 6, "mods/b": 6},
		},
		{
			// AC: a table with no coarse key resolves exactly as it did before the
			// shape existed — the module key still covers the coarse leg.
			name: "a table with no coarse key is unchanged",
			t:    Timeouts{"mods/a": 9},
			want: map[string]int{"mods/a": 9, "mods/a:ok": 9, "mods/a:slow": 9, "mods/b": 6},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, e := range tc.t.Apply(entries, 6) {
				if e.Timeout != tc.want[e.Name] {
					t.Errorf("%q got a step budget of %d, want %d", e.Name, e.Timeout, tc.want[e.Name])
				}
				if e.JobTimeout != e.Timeout+JobTimeoutHeadroom {
					t.Errorf("%q got a job budget of %d, want %d", e.Name, e.JobTimeout, e.Timeout+JobTimeoutHeadroom)
				}
			}
		})
	}
}

// TestParseTimeoutsCoarseKey pins that the coarse shape survives the JSON round trip
// the table crosses the module boundary as, including its validation.
func TestParseTimeoutsCoarseKey(t *testing.T) {
	got, err := ParseTimeouts(`{"daggerverse/kafka/tests:*": 15}`)
	if err != nil {
		t.Fatal(err)
	}
	if got[CoarseKey("daggerverse/kafka/tests")] != 15 {
		t.Errorf("parsed %v", got)
	}
	if _, err := ParseTimeouts(`{"daggerverse/kafka/tests:*": 0}`); err == nil {
		t.Error("a non-positive coarse budget was accepted")
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
