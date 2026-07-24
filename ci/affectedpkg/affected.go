// Package affectedpkg computes the minimum set of CI checks that could be
// affected by a set of changed files, given the module dependency graph.
//
// It is deliberately free of Dagger, filesystem, and git dependencies so the
// selection logic can be unit-tested in isolation. The glue that supplies the
// dependency graph from the live Dagger Workspace lives in the ci module (see
// ci/affected.go); this package only reasons over plain data.
//
// The guiding rule (issue #170) is: never skip a check a change could plausibly
// affect. Correctness beats speed — when it is unclear whether a check is
// affected, it runs.
package affectedpkg

import "strings"

// zeroSHA is git's all-zeros object id, which GitHub sends as
// github.event.before for the first push to a new branch.
const zeroSHA = "0000000000000000000000000000000000000000"

// DiffRange validates a (base, head) commit pair for change detection. It
// returns ok=false — meaning "no usable diff, run the full suite" — when either
// side is empty or the base is the all-zeros SHA (new branch / no base). Callers
// diff base...head (three-dot, merge-base) to obtain a change set.
func DiffRange(base, head string) (b, h string, ok bool) {
	if base == "" || head == "" || base == zeroSHA || head == zeroSHA {
		return "", "", false
	}
	return base, head, true
}

// BuildClosures returns, per check name, the set of module directories a change
// to which could affect that check: the transitive dependency closure of the
// check's own module directory (including the module itself).
//
//   - checkModule maps a check name (e.g. "kafka-tests:native") to its owning
//     module directory (e.g. "daggerverse/kafka/tests").
//   - adj is the direct-dependency adjacency: module directory -> the module
//     directories it directly depends on.
func BuildClosures(checkModule map[string]string, adj map[string][]string) map[string]map[string]bool {
	memo := map[string]map[string]bool{}
	out := make(map[string]map[string]bool, len(checkModule))
	for name, root := range checkModule {
		out[name] = closureOf(root, adj, memo)
	}
	return out
}

// closureOf computes the transitive closure of module directories reachable from
// root (including root) over the direct-dependency adjacency adj. The module
// graph is a DAG, so the tentative memo entry is never re-entered via a cycle.
func closureOf(root string, adj map[string][]string, memo map[string]map[string]bool) map[string]bool {
	if c, ok := memo[root]; ok {
		return c
	}
	res := map[string]bool{root: true}
	memo[root] = res // fill in place; safe for a DAG
	for _, dep := range adj[root] {
		for d := range closureOf(dep, adj, memo) {
			res[d] = true
		}
	}
	return res
}

// Select returns the subset of check names in universe that could be affected by
// changedFiles, together with full=true when the entire universe must run.
//
// closure is the per-check dependency closure from BuildClosures. A check absent
// from closure is "unresolved" and is always kept — never skipped.
//
// moduleDirs is the set of all known daggerverse module directories, used to map
// each changed file to its owning module by longest-prefix match.
//
// bindings is the per-toolchain aggregator-binding reattribution map from
// AggregatorBindings: those few generated files under ci/ are provably owned by a
// single toolchain, so they resolve to it instead of tripping the ci/ fail-safe.
//
// The result is full (run everything) when any fail-safe condition holds:
//   - changedFiles is empty (no diff available or it could not be computed);
//   - any changed file does not resolve to a daggerverse module directory (nor to
//     a known toolchain binding) — i.e. a change to CI infra, the ci/ aggregator
//     itself, root dagger.json, or other broadly-shared code.
//
// Otherwise only affected checks — plus every ci:* check, which always runs
// (repo-wide generated-code freshness and this package's own self-test) — are
// returned, in universe order.
func Select(universe []string, closure map[string]map[string]bool, changedFiles []string, moduleDirs []string, bindings map[string]string) (kept []string, full bool) {
	if len(changedFiles) == 0 {
		return universe, true
	}
	changed := make(map[string]bool)
	for _, f := range changedFiles {
		dir, ok := bindings[f]
		if !ok {
			dir, ok = OwningModule(f, moduleDirs)
		}
		if !ok {
			return universe, true
		}
		changed[dir] = true
	}
	for _, name := range universe {
		switch {
		case isCiCheck(name):
			kept = append(kept, name)
		case !isResolved(closure, name):
			kept = append(kept, name) // fail-safe: never skip an unresolved check
		case intersects(changed, closure[name]):
			kept = append(kept, name)
		}
	}
	return kept, false
}

// bindingDir/bindingExt bracket the root ci module's generated dependency
// bindings: dagger emits one ci/internal/dagger/<toolchain>.gen.go per installed
// toolchain, plus the module's own core dagger.gen.go.
const (
	bindingDir = "ci/internal/dagger/"
	bindingExt = ".gen.go"
	// coreBinding is the ci module's own core binding. It is not attributable to
	// any single toolchain, so it must keep failing open even in the pathological
	// case of a toolchain literally named "dagger".
	coreBinding = bindingDir + "dagger" + bindingExt
)

// AggregatorBindings maps each per-toolchain aggregator binding path to the
// toolchain module directory it is generated from, so that regenerating a single
// binding — which repo convention requires when adding or changing a tests
// toolchain (see ci/README.md) — is attributed to that toolchain instead of
// tripping Select's ci/ fail-safe and forcing the whole universe to run (#179).
//
// checkModule is the same check-name -> owning-module-directory map fed to
// BuildClosures. The binding's file name and the check-name prefix are both the
// toolchain name after dagger's kebab-casing (which splits letter<->digit
// boundaries: toolchain z5labs-tests -> z-5-labs-tests:all and
// ci/internal/dagger/z-5-labs-tests.gen.go), so the prefix can be reused verbatim
// and there is no second copy of that casing rule to drift — the only other copy
// lives in the route step of .github/workflows/ci.yml, which maps the very same
// prefix back to its source path.
//
// The mapping deliberately covers nothing else: any other path under ci/, and any
// binding for a toolchain not currently in the check universe, is absent here and
// so still runs the full suite.
func AggregatorBindings(checkModule map[string]string) map[string]string {
	out := make(map[string]string, len(checkModule))
	ambiguous := map[string]bool{}
	for name, dir := range checkModule {
		if isCiCheck(name) {
			continue
		}
		prefix, _, _ := strings.Cut(name, ":")
		if prefix == "" {
			continue
		}
		path := bindingDir + prefix + bindingExt
		if prev, seen := out[path]; seen && prev != dir {
			// Two toolchains cannot share a binding; if the universe says
			// otherwise, something is off — fail open rather than guess.
			ambiguous[path] = true
		}
		out[path] = dir
	}
	for path := range ambiguous {
		delete(out, path)
	}
	delete(out, coreBinding)
	return out
}

// OwningModule returns the directory in moduleDirs that owns path, chosen by
// longest-prefix match on a path-segment boundary, and ok=false when path lies
// under no known module directory.
func OwningModule(path string, moduleDirs []string) (dir string, ok bool) {
	best := ""
	for _, d := range moduleDirs {
		if d == "" {
			continue
		}
		if path == d || strings.HasPrefix(path, d+"/") {
			if len(d) > len(best) {
				best = d
			}
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

func isResolved(closure map[string]map[string]bool, name string) bool {
	_, ok := closure[name]
	return ok
}

// isCiCheck reports whether name is a check on the root ci module (prefix "ci").
func isCiCheck(name string) bool {
	prefix, _, found := strings.Cut(name, ":")
	if !found {
		return name == "ci"
	}
	return prefix == "ci"
}

func intersects(a, b map[string]bool) bool {
	if len(b) < len(a) {
		a, b = b, a
	}
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}
