package affectedpkg

import "testing"

// TestSelfCheck runs the same invariants the ci:selection-self-test Dagger check
// enforces, so a regression is caught by `go test` too.
func TestSelfCheck(t *testing.T) {
	if err := SelfCheck(); err != nil {
		t.Fatal(err)
	}
}

func TestOwningModule(t *testing.T) {
	dirs := []string{
		"daggerverse/go",
		"daggerverse/go/tests",
		"daggerverse/certificate-management",
		"daggerverse/certificate-management/tests",
		"daggerverse/certificate-management/examples/go",
	}
	cases := []struct {
		path string
		want string
		ok   bool
	}{
		{"daggerverse/go/main.go", "daggerverse/go", true},
		{"daggerverse/go/tests/main.go", "daggerverse/go/tests", true}, // longest prefix wins
		{"daggerverse/certificate-management/examples/go/x.go", "daggerverse/certificate-management/examples/go", true},
		{"daggerverse/certificate-management/tests/testdata/leaf.crt", "daggerverse/certificate-management/tests", true},
		{"daggerverse/gopls/main.go", "", false}, // sibling that shares a name prefix must not match
		{".github/workflows/ci.yml", "", false},
		{"ci/main.go", "", false},
		{"dagger.json", "", false},
	}
	for _, tc := range cases {
		got, ok := OwningModule(tc.path, dirs)
		if got != tc.want || ok != tc.ok {
			t.Errorf("OwningModule(%q) = (%q, %v), want (%q, %v)", tc.path, got, ok, tc.want, tc.ok)
		}
	}
}

func TestBuildClosuresTransitive(t *testing.T) {
	// z5labs/tests -> z5labs -> go: a change to go must reach z5labs/tests even
	// though the tests module does not list go directly in this fixture.
	adj := map[string][]string{
		"daggerverse/go":           nil,
		"daggerverse/z5labs":       {"daggerverse/go"},
		"daggerverse/z5labs/tests": {"daggerverse/z5labs", "daggerverse/random"},
		"daggerverse/random":       nil,
	}
	closure := BuildClosures(map[string]string{"z5labs-tests:all": "daggerverse/z5labs/tests"}, adj)
	got := closure["z5labs-tests:all"]
	for _, want := range []string{"daggerverse/z5labs/tests", "daggerverse/z5labs", "daggerverse/go", "daggerverse/random"} {
		if !got[want] {
			t.Errorf("closure missing %q; got %v", want, got)
		}
	}
}

func TestAggregatorBindings(t *testing.T) {
	checkModule := map[string]string{
		"kicad-tests:all":        "daggerverse/kicad/tests",
		"kafka-tests:native":     "daggerverse/kafka/tests",
		"kafka-tests:cluster":    "daggerverse/kafka/tests", // same toolchain, two checks
		"z-5-labs-tests:all":     "daggerverse/z5labs/tests",
		"ci:generated":           ".",
		"ci:selection-self-test": ".",
	}
	got := AggregatorBindings(checkModule)
	want := map[string]string{
		"ci/internal/dagger/kicad-tests.gen.go":    "daggerverse/kicad/tests",
		"ci/internal/dagger/kafka-tests.gen.go":    "daggerverse/kafka/tests",
		"ci/internal/dagger/z-5-labs-tests.gen.go": "daggerverse/z5labs/tests",
	}
	if len(got) != len(want) {
		t.Fatalf("AggregatorBindings() = %v, want %v", got, want)
	}
	for path, dir := range want {
		if got[path] != dir {
			t.Errorf("AggregatorBindings()[%q] = %q, want %q", path, got[path], dir)
		}
	}
}

func TestAggregatorBindingsExcludesAmbiguous(t *testing.T) {
	// A toolchain named "dagger" would collide with the ci module's own core
	// binding, and two module dirs claiming one binding is nonsense — both must
	// fall through to the ci/ full-suite fail-safe rather than reattribute.
	got := AggregatorBindings(map[string]string{
		"dagger:all":  "daggerverse/dagger/tests",
		"dup-tests:a": "daggerverse/dup/tests",
		"dup-tests:b": "daggerverse/other/tests",
	})
	for _, path := range []string{
		"ci/internal/dagger/dagger.gen.go",
		"ci/internal/dagger/dup-tests.gen.go",
	} {
		if dir, ok := got[path]; ok {
			t.Errorf("AggregatorBindings() reattributed %q to %q; want it excluded", path, dir)
		}
	}
}

func TestSelectReattributesToolchainBinding(t *testing.T) {
	universe := []string{"ci:generated", "kicad-tests:all", "kafka-tests:native"}
	closure := map[string]map[string]bool{
		"kicad-tests:all":    {"daggerverse/kicad/tests": true, "daggerverse/kicad": true},
		"kafka-tests:native": {"daggerverse/kafka/tests": true, "daggerverse/kafka": true},
	}
	dirs := []string{"daggerverse/kicad", "daggerverse/kicad/tests", "daggerverse/kafka", "daggerverse/kafka/tests"}
	bindings := AggregatorBindings(map[string]string{
		"kicad-tests:all":    "daggerverse/kicad/tests",
		"kafka-tests:native": "daggerverse/kafka/tests",
	})

	cases := []struct {
		name     string
		changed  []string
		wantFull bool
		want     []string
	}{
		{
			name:    "toolchain binding narrows to its own suite",
			changed: []string{"ci/internal/dagger/kicad-tests.gen.go"},
			want:    []string{"ci:generated", "kicad-tests:all"},
		},
		{
			name:     "core binding still forces the full suite",
			changed:  []string{"ci/internal/dagger/dagger.gen.go"},
			wantFull: true,
		},
		{
			name:     "unknown toolchain binding still forces the full suite",
			changed:  []string{"ci/internal/dagger/removed-tests.gen.go"},
			wantFull: true,
		},
		{
			name:     "a non-.gen.go file in the binding dir still forces the full suite",
			changed:  []string{"ci/internal/dagger/kicad-tests.go"},
			wantFull: true,
		},
		{
			name:     "other ci/ sources still force the full suite",
			changed:  []string{"ci/internal/dagger/kicad-tests.gen.go", "ci/main.go"},
			wantFull: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kept, full := Select(universe, closure, tc.changed, dirs, bindings)
			if full != tc.wantFull {
				t.Fatalf("full = %v, want %v", full, tc.wantFull)
			}
			if tc.wantFull {
				return
			}
			if !sameSet(kept, tc.want) {
				t.Errorf("selected %v, want %v", sortedCopy(kept), sortedCopy(tc.want))
			}
		})
	}
}

func TestDiffRange(t *testing.T) {
	const zero = "0000000000000000000000000000000000000000"
	cases := []struct {
		base, head string
		wantOK     bool
	}{
		{"aaa", "bbb", true},
		{"", "bbb", false},   // push with empty base
		{"aaa", "", false},   // missing head
		{zero, "bbb", false}, // new branch: before is all-zeros -> full
		{"aaa", zero, false}, // defensive: zero head -> full
		{"", "", false},      // nothing -> full
	}
	for _, tc := range cases {
		b, h, ok := DiffRange(tc.base, tc.head)
		if ok != tc.wantOK {
			t.Errorf("DiffRange(%q,%q) ok=%v, want %v", tc.base, tc.head, ok, tc.wantOK)
		}
		if ok && (b != tc.base || h != tc.head) {
			t.Errorf("DiffRange(%q,%q) = (%q,%q), want passthrough", tc.base, tc.head, b, h)
		}
	}
}

func TestSelectUnresolvedIsKept(t *testing.T) {
	// A check present in the universe but absent from the closure map (the live
	// Workspace could not resolve it) must never be skipped.
	universe := []string{"ci:generated", "kicad-tests:all", "mystery-tests:all"}
	closure := map[string]map[string]bool{
		"kicad-tests:all": {"daggerverse/kicad/tests": true, "daggerverse/kicad": true},
	}
	dirs := []string{"daggerverse/kicad", "daggerverse/kicad/tests"}
	kept, full := Select(universe, closure, []string{"daggerverse/kicad/main.go"}, dirs, nil)
	if full {
		t.Fatal("did not expect full-suite fallback")
	}
	if !sameSet(kept, []string{"ci:generated", "kicad-tests:all", "mystery-tests:all"}) {
		t.Errorf("unresolved check was dropped: got %v", sortedCopy(kept))
	}
}
