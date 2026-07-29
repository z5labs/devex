package affectedpkg

import (
	"reflect"
	"testing"
)

// tinyTree is the smallest graph that exercises InputHashes: one shared module,
// two checks over it, and a root module holding the global inputs.
func tinyTree() (closure, srcs map[string]map[string]bool, blobs map[string]string) {
	closure = map[string]map[string]bool{
		"kicad-tests:all":    {"daggerverse/kicad/tests": true, "daggerverse/kicad": true},
		"kicad-tests:native": {"daggerverse/kicad/tests": true, "daggerverse/kicad": true},
		"crypto-tests:all":   {"daggerverse/crypto": true},
		"ci:generated":       {".": true},
	}
	srcs = map[string]map[string]bool{
		".":                       {rootConfig: true, coreBinding: true, "ci/main.go": true},
		"daggerverse/kicad":       {"daggerverse/kicad/main.go": true},
		"daggerverse/kicad/tests": {"daggerverse/kicad/tests/main.go": true},
		"daggerverse/crypto":      {"daggerverse/crypto/crypto.go": true},
	}
	blobs = map[string]string{}
	for _, set := range srcs {
		for p := range set {
			blobs[p] = "blob:" + p
		}
	}
	return closure, srcs, blobs
}

// TestInputHashesAreDeterministic guards the property the whole store depends
// on: the digest must not depend on Go's randomized map iteration order.
func TestInputHashesAreDeterministic(t *testing.T) {
	closure, srcs, blobs := tinyTree()
	first := InputHashes(closure, srcs, blobs, nil)
	for range 25 {
		if got := InputHashes(closure, srcs, blobs, nil); !reflect.DeepEqual(got, first) {
			t.Fatalf("hashes differ between runs:\n%v\n%v", first, got)
		}
	}
}

// TestInputHashesSeparateChecksOnOneModule: two checks sharing a closure must
// still hash apart, or a pass recorded for one would retire the other.
func TestInputHashesSeparateChecksOnOneModule(t *testing.T) {
	closure, srcs, blobs := tinyTree()
	h := InputHashes(closure, srcs, blobs, nil)
	if h["kicad-tests:all"] == "" || h["kicad-tests:native"] == "" {
		t.Fatalf("missing hashes: %v", h)
	}
	if h["kicad-tests:all"] == h["kicad-tests:native"] {
		t.Error("two checks over the same closure must not share an input hash")
	}
	if _, ok := h["ci:generated"]; ok {
		t.Error("ci:* checks must not be hashed")
	}
}

func TestInputHashesUnhashableWithoutRootContext(t *testing.T) {
	closure, srcs, blobs := tinyTree()
	delete(srcs, RootPkg)
	if got := InputHashes(closure, srcs, blobs, nil); got != nil {
		t.Errorf("got %v, want nil when the global inputs cannot be read", got)
	}
}

func TestInputHashesUnresolvedCheckIsUnhashable(t *testing.T) {
	closure, srcs, blobs := tinyTree()
	// A check whose module could not be resolved never reaches closure at all.
	delete(closure, "crypto-tests:all")
	h := InputHashes(closure, srcs, blobs, nil)
	if _, ok := h["crypto-tests:all"]; ok {
		t.Error("an unresolved check must not be hashed")
	}
	run, skipped := MemoFilter([]string{"crypto-tests:all"}, h, map[string]bool{"": true})
	if !reflect.DeepEqual(run, []string{"crypto-tests:all"}) || skipped != nil {
		t.Errorf("unhashed check was skipped: run=%v skipped=%v", run, skipped)
	}
}

func TestMemoFilter(t *testing.T) {
	hashes := map[string]string{"a:all": "h1", "b:all": "h2"}
	run, skipped := MemoFilter(
		[]string{"ci:generated", "a:all", "b:all", "c:all"},
		hashes,
		map[string]bool{"h1": true},
	)
	// ci:generated and c:all are unhashed, b:all's hash is not recorded.
	if want := []string{"ci:generated", "b:all", "c:all"}; !reflect.DeepEqual(run, want) {
		t.Errorf("run = %v, want %v", run, want)
	}
	if want := []string{"a:all"}; !reflect.DeepEqual(skipped, want) {
		t.Errorf("skipped = %v, want %v", skipped, want)
	}
}

// TestInputHashesIgnoreNonGlobalRootPaths pins the nonGlobalRootPaths
// subtraction: adding a toolchain edits the repo-root dagger.json and makes
// dagger regenerate the core binding, and neither may retire recorded passes.
func TestInputHashesIgnoreNonGlobalRootPaths(t *testing.T) {
	closure, srcs, blobs := tinyTree()
	before := InputHashes(closure, srcs, blobs, nil)

	after := map[string]string{}
	for p, oid := range blobs {
		after[p] = oid
	}
	after[rootConfig] = "blob:a-new-toolchain-entry"
	after[coreBinding] = "blob:a-new-id-type-and-loader"
	if got := InputHashes(closure, srcs, after, nil); !reflect.DeepEqual(got, before) {
		t.Errorf("a module-adding edit changed check hashes:\nbefore %v\nafter  %v", before, got)
	}

	// The rest of the root context is still global.
	after["ci/main.go"] = "blob:edited"
	got := InputHashes(closure, srcs, after, nil)
	for name, h := range before {
		if got[name] == h {
			t.Errorf("%s kept its hash across a ci/ change", name)
		}
	}
}

func TestMemoTrustedSubtractsOnlyNonGlobalRootPaths(t *testing.T) {
	_, srcs, _ := tinyTree()
	dirs := []string{".", "daggerverse/kicad", "daggerverse/kicad/tests", "daggerverse/crypto"}
	cases := []struct {
		name    string
		changes []Change
		want    bool
	}{
		{"root config alone", []Change{{Path: rootConfig}}, true},
		{"core binding alone", []Change{{Path: coreBinding}}, true},
		{"both, as a module addition", []Change{{Path: rootConfig}, {Path: coreBinding}}, true},
		{"with a module change", []Change{{Path: rootConfig}, {Path: "daggerverse/kicad/main.go"}}, true},
		{"with a ci/ change", []Change{{Path: coreBinding}, {Path: "ci/main.go"}}, false},
		{"no changes", nil, true},
	}
	for _, tc := range cases {
		if got := MemoTrusted(tc.changes, dirs, srcs, nil); got != tc.want {
			t.Errorf("%s: MemoTrusted = %v, want %v", tc.name, got, tc.want)
		}
	}
}
