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

// RootPkg is the module directory of the repository root module. It claims every
// path no nested module claims, so a change to broadly-shared code lands here
// rather than nowhere.
const RootPkg = "."

// Change is one path from the diff, together with whether it survives at head.
// Deleted is load-bearing: see isInput.
type Change struct {
	Path    string
	Deleted bool
}

// globalPrefixes are the paths that govern how CI itself runs — which checks are
// selected, how the matrix is built, what the timeouts are — rather than what any
// check computes. They belong to no module's source context, so nothing else
// would attribute them, yet a change to one can invalidate the whole run.
var globalPrefixes = []string{".github/workflows/"}

// Attribute maps a change set onto the modules it is actually an input to,
// returning the set of changed module directories and whether a global input
// changed (which forces the full universe).
//
// srcs gives, per module directory, the repo-relative paths that make up that
// module's source context — precisely what Dagger uploads for it. A path that
// lies under a module but is absent from its source context is an input to
// nothing and is dropped. That is what stops a module's README from triggering
// its dependents, and it is a property Dagger enforces (the file is never
// shipped to the engine) rather than one this package asserts about which files
// look like prose.
//
// A module missing from srcs is treated as owning everything beneath it: when we
// cannot resolve a source context we decline to narrow.
//
// bindings is the per-toolchain aggregator-binding reattribution map from
// AggregatorBindings; those generated files under ci/ are provably owned by a
// single toolchain, so they resolve to it rather than to the root module (#179).
//
// global is set — meaning run everything — when:
//   - changes is empty, which means "no usable diff", not "nothing changed";
//   - a path lies under one of globalPrefixes;
//   - a path is an input to the root module (root dagger.json, ci/**);
//   - a path is claimed by no module at all, not even the root.
func Attribute(changes []Change, moduleDirs []string, srcs map[string]map[string]bool, bindings map[string]string) (changed map[string]bool, global bool) {
	if len(changes) == 0 {
		return nil, true
	}
	changed = make(map[string]bool)
	for _, c := range changes {
		if dir, ok := bindings[c.Path]; ok {
			changed[dir] = true
			continue
		}
		if hasAnyPrefix(c.Path, globalPrefixes) {
			return nil, true
		}
		dir, ok := OwningModule(c.Path, moduleDirs)
		if !ok {
			return nil, true
		}
		if !isInput(c, dir, srcs) {
			continue // in no module's source context: an input to nothing
		}
		if dir == RootPkg {
			return nil, true
		}
		changed[dir] = true
	}
	return changed, false
}

// isInput reports whether c is part of dir's source context.
//
// A deleted path can never appear in the head source context, which makes it
// indistinguishable from a path the module declared out — so deletions are
// attributed to their module instead. That over-runs on a deleted README and
// under-runs on nothing, which is the direction this package errs in.
func isInput(c Change, dir string, srcs map[string]map[string]bool) bool {
	set, resolved := srcs[dir]
	if !resolved {
		return true
	}
	return set[c.Path] || c.Deleted
}

func hasAnyPrefix(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// Select returns the subset of check names in universe reachable from the
// changed module directories, together with full=true when the entire universe
// must run.
//
// closure is the per-check dependency closure from BuildClosures. A check absent
// from closure is "unresolved" and is always kept — never skipped.
//
// changed and global come from Attribute. An empty changed set with global=false
// is a real answer, not a missing one: it means every changed path was an input
// to nothing, so only the always-on ci:* checks run.
//
// Otherwise only affected checks — plus every ci:* check, which always runs
// (repo-wide generated-code freshness and this package's own self-test) — are
// returned, in universe order.
func Select(universe []string, closure map[string]map[string]bool, changed map[string]bool, global bool) (kept []string, full bool) {
	if global {
		return universe, true
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
	// any single toolchain, so *selection* must keep failing open on it even in
	// the pathological case of a toolchain literally named "dagger" — hence its
	// removal from the AggregatorBindings map below.
	//
	// Memoization treats it differently, and deliberately: see
	// nonGlobalRootPaths. Attribution and the input hash answer different
	// questions, and only the first has to be conservative about a file dagger
	// regenerates every time the toolchain set changes.
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
//
// The innermost module wins, so daggerverse/crypto/tests/main.go belongs to
// daggerverse/crypto/tests and not to daggerverse/crypto — even though Dagger's
// own source context for crypto contains it. That is deliberate: a nested module
// is a separate Go module that never compiles into its parent, so scoping
// ownership to the innermost dagger.json is both correct and what keeps a
// tests-only edit from fanning out to the parent's dependents.
//
// RootPkg, when present in moduleDirs, matches every path. Being the shortest
// possible directory it always loses to a real prefix, so it acts as the
// catch-all owner for anything no nested module claims.
func OwningModule(path string, moduleDirs []string) (dir string, ok bool) {
	best := ""
	for _, d := range moduleDirs {
		if d == "" {
			continue
		}
		if d == RootPkg || path == d || strings.HasPrefix(path, d+"/") {
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
