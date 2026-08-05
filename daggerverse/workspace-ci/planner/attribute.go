// Package planner computes which of a workspace's checks a change could affect,
// which of those a previous run already proved good, and how each surviving one
// should be routed and bounded.
//
// It is deliberately free of Dagger, filesystem and git dependencies: everything
// it reasons over is plain data supplied by the workspace-ci module (module
// directories, dependency edges, source contexts, git object ids). That keeps the
// rules unit-testable in isolation, and it is what SelfCheck exercises so a
// consumer's CI fails on a regression in the change -> checks mapping.
//
// The guiding rule (issue #170) is: never skip a check a change could plausibly
// affect. Correctness beats speed — when it is unclear whether a check is
// affected, it runs.
package planner

import "strings"

// RootModule is the module directory of the workspace's root module. It claims
// every path no nested module claims, so a change to broadly-shared code lands
// there rather than nowhere.
const RootModule = "."

// zeroSHA is git's all-zeros object id, which GitHub sends as
// github.event.before for the first push to a new branch.
const zeroSHA = "0000000000000000000000000000000000000000"

// DiffRange validates a (base, head) revision pair for change detection. It
// returns ok=false — meaning "no usable diff, run everything" — when either side
// is empty or is the all-zeros SHA (new branch / no base). Callers diff
// base...head (three-dot, merge-base) to obtain a change set.
//
// Anything else is passed through for the caller to resolve against the
// repository, so a branch, a tag or HEAD reaches git untouched. The all-zeros SHA
// is rejected here rather than left to that resolution on purpose: it is a
// sentinel GitHub sends and not a revision anybody means, so "no usable base"
// stays a decision this package makes and not an accident of what git happens to
// do with forty zeros.
func DiffRange(base, head string) (b, h string, ok bool) {
	if base == "" || head == "" || base == zeroSHA || head == zeroSHA {
		return "", "", false
	}
	return base, head, true
}

// Change is one path from the diff, together with whether it survives at head.
// Deleted is load-bearing: see isInput.
type Change struct {
	Path    string
	Deleted bool
}

// BuildClosures returns, per module directory, the set of module directories a
// change to which could affect it: its transitive dependency closure, including
// itself.
//
// adj is the direct-dependency adjacency: module directory -> the module
// directories it directly depends on. A module absent from adj gets no closure
// entry, which callers must treat as "unresolved, so always run".
func BuildClosures(adj map[string][]string) map[string]map[string]bool {
	memo := map[string]map[string]bool{}
	out := make(map[string]map[string]bool, len(adj))
	for dir := range adj {
		out[dir] = closureOf(dir, adj, memo)
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

// Attribute maps a change set onto the modules it is actually an input to,
// returning the set of changed module directories and whether a global input
// changed (which forces everything to run).
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
// bindings is the aggregator-binding reattribution map from AggregatorBindings;
// those generated files under the root module's source are provably owned by a
// single toolchain, so they resolve to it rather than to the root module (#179).
//
// globalPaths are the path prefixes that govern how CI runs rather than what any
// check computes.
//
// global is set — meaning run everything — when:
//   - changes is empty, which means "no usable diff", not "nothing changed";
//   - a path lies under one of globalPaths;
//   - a path is an input to the root module;
//   - a path is claimed by no module at all, not even the root.
func Attribute(
	changes []Change,
	moduleDirs []string,
	srcs map[string]map[string]bool,
	bindings map[string]string,
	globalPaths []string,
) (changed map[string]bool, global bool) {
	if len(changes) == 0 {
		return nil, true
	}
	changed = make(map[string]bool)
	for _, c := range changes {
		if dir, ok := bindings[c.Path]; ok {
			changed[dir] = true
			continue
		}
		if hasAnyPrefix(c.Path, globalPaths) {
			return nil, true
		}
		dir, ok := OwningModule(c.Path, moduleDirs)
		if !ok {
			return nil, true
		}
		if !isInput(c, dir, srcs) {
			continue // in no module's source context: an input to nothing
		}
		if dir == RootModule {
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
		if p != "" && strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// SelectModules returns the subset of moduleDirs whose checks must run, together
// with full=true when every module must run.
//
// closure is the per-module dependency closure from BuildClosures. A module
// absent from closure is "unresolved" and is always kept — never skipped.
//
// The root module is always kept. Its checks are the ones that answer questions
// about the workspace as a whole (generated-code freshness, this package's own
// self-test), it is the module every global input belongs to, and it is never
// memoized — so there is no change set for which skipping it is safe.
//
// changed and global come from Attribute. An empty changed set with global=false
// is a real answer, not a missing one: it means every changed path was an input
// to nothing, so only the root module's checks run.
//
// The result is in moduleDirs order.
func SelectModules(
	moduleDirs []string,
	closure map[string]map[string]bool,
	changed map[string]bool,
	global bool,
) (affected []string, full bool) {
	if global {
		return moduleDirs, true
	}
	for _, dir := range moduleDirs {
		switch {
		case dir == RootModule:
			affected = append(affected, dir)
		case closure[dir] == nil:
			affected = append(affected, dir) // fail-safe: never skip an unresolved module
		case intersects(changed, closure[dir]):
			affected = append(affected, dir)
		}
	}
	return affected, false
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
// RootModule, when present in moduleDirs, matches every path. Being the shortest
// possible directory it always loses to a real prefix, so it acts as the
// catch-all owner for anything no nested module claims.
func OwningModule(path string, moduleDirs []string) (dir string, ok bool) {
	best := ""
	for _, d := range moduleDirs {
		if d == "" {
			continue
		}
		if d == RootModule || path == d || strings.HasPrefix(path, d+"/") {
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
