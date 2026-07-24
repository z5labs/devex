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
// The result is full (run everything) when any fail-safe condition holds:
//   - changedFiles is empty (no diff available or it could not be computed);
//   - any changed file does not resolve to a daggerverse module directory — i.e.
//     a change to CI infra, the ci/ aggregator, root dagger.json, or other
//     broadly-shared code.
//
// Otherwise only affected checks — plus every ci:* check, which always runs
// (repo-wide generated-code freshness and this package's own self-test) — are
// returned, in universe order.
func Select(universe []string, closure map[string]map[string]bool, changedFiles []string, moduleDirs []string) (kept []string, full bool) {
	if len(changedFiles) == 0 {
		return universe, true
	}
	changed := make(map[string]bool)
	for _, f := range changedFiles {
		dir, ok := OwningModule(f, moduleDirs)
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
