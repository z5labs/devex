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
	kept, full := Select(universe, closure, []string{"daggerverse/kicad/main.go"}, dirs)
	if full {
		t.Fatal("did not expect full-suite fallback")
	}
	if !sameSet(kept, []string{"ci:generated", "kicad-tests:all", "mystery-tests:all"}) {
		t.Errorf("unresolved check was dropped: got %v", sortedCopy(kept))
	}
}
