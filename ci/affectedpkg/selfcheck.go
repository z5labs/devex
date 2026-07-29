package affectedpkg

import (
	"fmt"
	"sort"
	"strings"
)

// fixture is a representative slice of the real repository dependency graph,
// used by SelfCheck so a regression in the change->checks mapping fails CI even
// though the live graph (which the ci module resolves from the Dagger Workspace)
// is not available in a pure unit test.
//
// adj is direct-dependency adjacency (module dir -> the dirs it depends on);
// every referenced dir also appears as a key so it is a known module directory.
type fixture struct {
	adj         map[string][]string
	checkModule map[string]string
}

func repoFixture() fixture {
	adj := map[string][]string{
		// leaf modules
		"daggerverse/random":                             nil,
		"daggerverse/crypto":                             nil,
		"daggerverse/certificate-management":             nil,
		"daggerverse/postgres":                           nil,
		"daggerverse/otel":                               nil,
		"daggerverse/grafana-stack":                      nil,
		"daggerverse/kicad":                              nil,
		"daggerverse/certificate-management/examples/go": {"daggerverse/certificate-management", "daggerverse/random", "daggerverse/crypto"},
		// modules with dependencies
		"daggerverse/kafka":     {"daggerverse/certificate-management", "daggerverse/crypto", "daggerverse/random"},
		"daggerverse/envoy":     {"daggerverse/certificate-management", "daggerverse/crypto", "daggerverse/random"},
		"daggerverse/skill-gen": {"daggerverse/postgres"},
		// tests toolchains (the check-bearing modules)
		"daggerverse/certificate-management/tests": {"daggerverse/certificate-management", "daggerverse/random", "daggerverse/crypto", "daggerverse/certificate-management/examples/go"},
		"daggerverse/crypto/tests":                 {"daggerverse/crypto"},
		"daggerverse/kafka/tests":                  {"daggerverse/kafka", "daggerverse/random", "daggerverse/certificate-management", "daggerverse/crypto"},
		"daggerverse/skill-gen/tests":              {"daggerverse/skill-gen", "daggerverse/postgres", "daggerverse/random", "daggerverse/certificate-management", "daggerverse/crypto"},
		"daggerverse/postgres/tests":               {"daggerverse/postgres", "daggerverse/random", "daggerverse/certificate-management", "daggerverse/crypto"},
		"daggerverse/envoy/tests":                  {"daggerverse/envoy", "daggerverse/certificate-management", "daggerverse/crypto", "daggerverse/random"},
		"daggerverse/otel/tests":                   {"daggerverse/otel", "daggerverse/certificate-management", "daggerverse/crypto", "daggerverse/grafana-stack", "daggerverse/random"},
		"daggerverse/kicad/tests":                  {"daggerverse/kicad"},
		// Kept dependency-free so it stays out of every other case's expected
		// set; it is here for its digit-bearing name (see below).
		"daggerverse/z5labs":       nil,
		"daggerverse/z5labs/tests": {"daggerverse/z5labs"},
	}
	checkModule := map[string]string{
		"certificate-management-tests:all": "daggerverse/certificate-management/tests",
		"crypto-tests:all":                 "daggerverse/crypto/tests",
		"kafka-tests:native":               "daggerverse/kafka/tests",
		"skill-gen-tests:all":              "daggerverse/skill-gen/tests",
		"postgres-tests:cluster":           "daggerverse/postgres/tests",
		"envoy-tests:admin":                "daggerverse/envoy/tests",
		"otel-tests:core":                  "daggerverse/otel/tests",
		"kicad-tests:all":                  "daggerverse/kicad/tests",
		// Dagger kebab-cases toolchain names across letter<->digit boundaries, so
		// the toolchain z5labs-tests surfaces as z-5-labs-tests:all — matching the
		// ci/internal/dagger/z-5-labs-tests.gen.go binding file name byte for byte.
		"z-5-labs-tests:all":     "daggerverse/z5labs/tests",
		"ci:generated":           ".",
		"ci:selection-self-test": ".",
	}
	return fixture{adj: adj, checkModule: checkModule}
}

func (f fixture) universe() []string {
	names := make([]string, 0, len(f.checkModule))
	for n := range f.checkModule {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (f fixture) moduleDirs() []string {
	dirs := make([]string, 0, len(f.adj)+1)
	for d := range f.adj {
		if strings.HasPrefix(d, "daggerverse/") {
			dirs = append(dirs, d)
		}
	}
	// The root module claims whatever no nested module does, mirroring the
	// dagger.json walk in the ci module.
	return append(dirs, RootPkg)
}

// nonInputs are the fixture paths that no module's source context contains, and
// so are inputs to nothing.
//
// Module-scoped entries are declared out in that module's dagger.json
// ("include": ["!README.md"]), which stops Dagger uploading them at all.
// Repo-level entries were never in any context to begin with: the root module's
// source is ci/, so its context is exactly ci/** plus dagger.json.
func nonInputs() map[string]bool {
	return map[string]bool{
		"daggerverse/crypto/README.md":      true,
		"daggerverse/kicad/README.md":       true,
		"daggerverse/kicad/tests/README.md": true,
		"ci/README.md":                      true,
		"README.md":                         true,
		"LICENSE":                           true,
		"docs/dagger-internals/nesting.md":  true,
	}
}

// srcsFor models the per-module source contexts Dagger would report for the
// paths a case touches: everything under the owning module, minus nonInputs and
// minus anything deleted (which cannot appear in the head context). Every module
// dir gets an entry, because a missing one means "source context unresolved" and
// makes Attribute decline to narrow.
func (f fixture) srcsFor(changes []Change) map[string]map[string]bool {
	dirs := f.moduleDirs()
	skip := nonInputs()
	out := make(map[string]map[string]bool, len(dirs))
	for _, d := range dirs {
		out[d] = map[string]bool{}
	}
	for _, c := range changes {
		if c.Deleted || skip[c.Path] {
			continue
		}
		if d, ok := OwningModule(c.Path, dirs); ok {
			out[d][c.Path] = true
		}
	}
	return out
}

// selfCheckCase is one change -> expected selection assertion.
type selfCheckCase struct {
	name    string
	changed []string
	// removed are paths that the change set deletes, i.e. that do not exist at
	// head. They are attributed to their module even though no source context
	// can contain them.
	removed []string
	// wantFull asserts the full-suite fail-safe fired.
	wantFull bool
	// wantChecks, when wantFull is false, is the exact expected kept set.
	wantChecks []string
}

// changes flattens a case into the (path, deleted) pairs Attribute consumes.
func (c selfCheckCase) changes() []Change {
	out := make([]Change, 0, len(c.changed)+len(c.removed))
	for _, p := range c.changed {
		out = append(out, Change{Path: p})
	}
	for _, p := range c.removed {
		out = append(out, Change{Path: p, Deleted: true})
	}
	return out
}

// selfCheckCases returns the invariants that must hold. They double as the
// acceptance-criteria demonstration: a change to a shared module still triggers
// its dependents, and broad/infra changes fall back to the full suite.
func selfCheckCases() []selfCheckCase {
	const ci1, ci2 = "ci:generated", "ci:selection-self-test"
	return []selfCheckCase{
		{
			name:    "shared module certificate-management triggers all dependents",
			changed: []string{"daggerverse/certificate-management/main.go"},
			wantChecks: []string{
				ci1, ci2,
				"certificate-management-tests:all",
				"kafka-tests:native",
				"skill-gen-tests:all",
				"postgres-tests:cluster",
				"envoy-tests:admin",
				"otel-tests:core",
			},
		},
		{
			name:    "random has the widest blast radius",
			changed: []string{"daggerverse/random/random.go"},
			wantChecks: []string{
				ci1, ci2,
				"certificate-management-tests:all",
				"kafka-tests:native",
				"skill-gen-tests:all",
				"postgres-tests:cluster",
				"envoy-tests:admin",
				"otel-tests:core",
			},
		},
		{
			name:       "leaf module kicad triggers only its own suite",
			changed:    []string{"daggerverse/kicad/main.go"},
			wantChecks: []string{ci1, ci2, "kicad-tests:all"},
		},
		{
			name:       "a module's own tests dir triggers only that suite",
			changed:    []string{"daggerverse/kicad/tests/main.go"},
			wantChecks: []string{ci1, ci2, "kicad-tests:all"},
		},
		{
			name:    "crypto triggers crypto plus every dependent",
			changed: []string{"daggerverse/crypto/crypto.go"},
			wantChecks: []string{
				ci1, ci2,
				"certificate-management-tests:all",
				"crypto-tests:all",
				"kafka-tests:native",
				"skill-gen-tests:all",
				"postgres-tests:cluster",
				"envoy-tests:admin",
				"otel-tests:core",
			},
		},
		{
			name:    "the certificate-management example only feeds its own suite",
			changed: []string{"daggerverse/certificate-management/examples/go/main.go"},
			wantChecks: []string{
				ci1, ci2,
				"certificate-management-tests:all",
			},
		},
		{
			// The regenerated binding is attributable to exactly one toolchain, so
			// it no longer drags in the whole universe (#179).
			name:       "a per-toolchain aggregator binding narrows to its own suite",
			changed:    []string{"ci/internal/dagger/kicad-tests.gen.go"},
			wantChecks: []string{ci1, ci2, "kicad-tests:all"},
		},
		{
			// The binding file name and the check-name prefix share dagger's
			// letter<->digit kebab-casing, so no separate mangling is needed.
			name:       "a digit-bearing toolchain binding narrows to its own suite",
			changed:    []string{"ci/internal/dagger/z-5-labs-tests.gen.go"},
			wantChecks: []string{ci1, ci2, "z-5-labs-tests:all"},
		},
		{
			// The shape of a real toolchain-adding PR: tests tree plus its binding.
			name:       "adding a tests toolchain runs only that toolchain's checks",
			changed:    []string{"daggerverse/kicad/tests/main.go", "ci/internal/dagger/kicad-tests.gen.go"},
			wantChecks: []string{ci1, ci2, "kicad-tests:all"},
		},
		{
			name:     "the ci module's own core binding runs the full suite",
			changed:  []string{"ci/internal/dagger/dagger.gen.go"},
			wantFull: true,
		},
		{
			name:     "a binding for an unknown toolchain runs the full suite",
			changed:  []string{"ci/internal/dagger/mystery-tests.gen.go"},
			wantFull: true,
		},
		{
			name:     "a toolchain binding mixed with a ci/ source change runs the full suite",
			changed:  []string{"ci/internal/dagger/kicad-tests.gen.go", "ci/affectedpkg/affected.go"},
			wantFull: true,
		},
		{name: "CI workflow change runs the full suite", changed: []string{".github/workflows/ci.yml"}, wantFull: true},
		{name: "ci aggregator change runs the full suite", changed: []string{"ci/main.go"}, wantFull: true},
		{name: "root dagger.json change runs the full suite", changed: []string{"dagger.json"}, wantFull: true},
		{name: "no changed files runs the full suite", changed: nil, wantFull: true},
		{
			name:     "a module change mixed with an infra change runs the full suite",
			changed:  []string{"daggerverse/kicad/main.go", "dagger.json"},
			wantFull: true,
		},

		// Input-awareness (#195). A path that lies under a module but outside its
		// Dagger source context is an input to nothing, so it selects nothing.
		{
			name:       "a module-root README triggers no dependents",
			changed:    []string{"daggerverse/crypto/README.md"},
			wantChecks: []string{ci1, ci2},
		},
		{
			name:       "repo-level docs run no module checks instead of the full suite",
			changed:    []string{"README.md", "LICENSE", "docs/dagger-internals/nesting.md"},
			wantChecks: []string{ci1, ci2},
		},
		{
			// ci/README.md is declared out of the root module, so unlike every
			// other path under ci/ it no longer forces the whole universe.
			name:       "the ci module's own README runs no module checks",
			changed:    []string{"ci/README.md"},
			wantChecks: []string{ci1, ci2},
		},
		{
			name:       "docs alongside a module change keep that module's checks",
			changed:    []string{"daggerverse/kicad/main.go", "README.md"},
			wantChecks: []string{ci1, ci2, "kicad-tests:all"},
		},
		{
			name:     "docs alongside an infra change still run the full suite",
			changed:  []string{"README.md", "ci/main.go"},
			wantFull: true,
		},
		{
			// The template is embedded by daggerverse/skill-gen/skill/render.go, so
			// it is program data. Nothing declares it out, so it stays an input —
			// the reason this classification is a source-context question and never
			// a question about which files look like prose.
			name:       "an embedded markdown template is an input, not docs",
			changed:    []string{"daggerverse/skill-gen/skill/templates/README.md.tmpl"},
			wantChecks: []string{ci1, ci2, "skill-gen-tests:all"},
		},
		{
			name:       "a golden testdata README is an input, not docs",
			changed:    []string{"daggerverse/skill-gen/skill/testdata/golden/README.md"},
			wantChecks: []string{ci1, ci2, "skill-gen-tests:all"},
		},
		{
			// Only a module's own root README is declared out; one nested in a
			// fixture tree is untouched by that declaration.
			name:       "a README nested inside a module stays an input",
			changed:    []string{"daggerverse/kicad/tests/fixtures/no-board/README.md"},
			wantChecks: []string{ci1, ci2, "kicad-tests:all"},
		},
		{
			// Deleted paths cannot appear in any head source context, so they are
			// attributed to their module rather than read as declared out.
			name:       "a deleted module source file still triggers its module",
			removed:    []string{"daggerverse/kicad/main.go"},
			wantChecks: []string{ci1, ci2, "kicad-tests:all"},
		},
		{
			// The documented cost of that rule: deleting a declared-out file
			// over-runs. Rare, and in the safe direction.
			name:       "a deleted module README over-runs rather than under-runs",
			removed:    []string{"daggerverse/kicad/README.md"},
			wantChecks: []string{ci1, ci2, "kicad-tests:all"},
		},
	}
}

// SelfCheck runs every invariant against the fixture graph and returns a non-nil
// error describing the first failure. It backs both the ci:selection-self-test
// Dagger check and the Go unit test.
func SelfCheck() error {
	f := repoFixture()
	closure := BuildClosures(f.checkModule, f.adj)
	universe := f.universe()
	dirs := f.moduleDirs()
	bindings := AggregatorBindings(f.checkModule)

	for _, tc := range selfCheckCases() {
		changes := tc.changes()
		changed, global := Attribute(changes, dirs, f.srcsFor(changes), bindings)
		kept, full := Select(universe, closure, changed, global)
		if full != tc.wantFull {
			return fmt.Errorf("%s: full=%v, want %v", tc.name, full, tc.wantFull)
		}
		if tc.wantFull {
			continue
		}
		if !sameSet(kept, tc.wantChecks) {
			return fmt.Errorf("%s: selected %v, want %v", tc.name, sortedCopy(kept), sortedCopy(tc.wantChecks))
		}
	}
	return nil
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int, len(a))
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
