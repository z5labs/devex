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
		"ci:generated":                     ".",
		"ci:selection-self-test":           ".",
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
	dirs := make([]string, 0, len(f.adj))
	for d := range f.adj {
		if strings.HasPrefix(d, "daggerverse/") {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// selfCheckCase is one change -> expected selection assertion.
type selfCheckCase struct {
	name    string
	changed []string
	// wantFull asserts the full-suite fail-safe fired.
	wantFull bool
	// wantChecks, when wantFull is false, is the exact expected kept set.
	wantChecks []string
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
		{name: "CI workflow change runs the full suite", changed: []string{".github/workflows/ci.yml"}, wantFull: true},
		{name: "ci aggregator change runs the full suite", changed: []string{"ci/main.go"}, wantFull: true},
		{name: "root dagger.json change runs the full suite", changed: []string{"dagger.json"}, wantFull: true},
		{name: "no changed files runs the full suite", changed: nil, wantFull: true},
		{
			name:     "a module change mixed with an infra change runs the full suite",
			changed:  []string{"daggerverse/kicad/main.go", "README.md"},
			wantFull: true,
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

	for _, tc := range selfCheckCases() {
		kept, full := Select(universe, closure, tc.changed, dirs)
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
