package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"dagger/ci/affectedpkg"
	"dagger/ci/internal/dagger"
)

// SelectionSelfTest verifies the affected-check selection logic against a fixed
// dependency-graph fixture, so a regression in the change->checks mapping — for
// example a shared module like certificate-management ceasing to trigger its
// dependents (kafka, skill-gen, ...) — fails CI. It runs in-process and needs no
// services, so it is cheap enough to run on every CI leg set.
//
// +check
func (ci *Ci) SelectionSelfTest(ctx context.Context) error {
	return affectedpkg.SelfCheck()
}

// AffectedChecks returns, as a JSON array string, the subset of checks that a
// change could plausibly affect — the input for CI's dynamic matrix.
//
//   - checks is the full check universe as a JSON array of names (exactly what
//     `dagger check -l` / the dagger/checks action emits).
//   - changed is the newline-separated list of repo-relative changed file paths.
//     An empty value means "could not determine what changed" and, like any
//     change outside a daggerverse module, yields the full universe.
//
// The dependency graph is read from the live Dagger Workspace (Dagger's own
// resolved module graph, so it never goes stale): every check maps to its owning
// module via Check.OriginalModule, and each module's transitive dependency
// closure is walked via ModuleSource.Dependencies. The pure selection then runs
// in affectedpkg so the logic stays unit-testable.
//
// Fail-safe throughout: if the Workspace cannot be read at all, the full universe
// is returned; if a single check cannot be resolved, it is left unresolved and
// therefore always kept. A check is never skipped because of a resolution gap.
//
// +cache="never"
func (ci *Ci) AffectedChecks(ctx context.Context, checks string, changed string) (string, error) {
	var universe []string
	if err := json.Unmarshal([]byte(checks), &universe); err != nil {
		return "", fmt.Errorf("parse checks universe: %w", err)
	}
	changedFiles := nonEmptyLines(changed)

	list, err := dag.CurrentWorkspace().Checks().List(ctx)
	if err != nil {
		// Cannot resolve the graph — run everything rather than risk a false skip.
		fmt.Fprintf(os.Stderr, "affected-checks: cannot list workspace checks (%v); running full suite\n", err)
		return marshal(universe)
	}

	checkModule := make(map[string]string, len(list))
	adj := map[string][]string{}
	for i := range list {
		chk := list[i]
		name, err := chk.Name(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "affected-checks: cannot read a check name (%v); leaving it unresolved\n", err)
			continue
		}
		root, err := gatherDeps(ctx, chk.OriginalModule().Source(), adj)
		if err != nil {
			// Leave this check out of checkModule -> Select keeps it (fail-safe).
			fmt.Fprintf(os.Stderr, "affected-checks: cannot resolve module for %q (%v); leaving it unresolved\n", name, err)
			continue
		}
		checkModule[name] = root
	}

	moduleDirs := make([]string, 0, len(adj))
	for d := range adj {
		if strings.HasPrefix(d, "daggerverse/") {
			moduleDirs = append(moduleDirs, d)
		}
	}

	closure := affectedpkg.BuildClosures(checkModule, adj)
	kept, full := affectedpkg.Select(universe, closure, changedFiles, moduleDirs)
	if full {
		kept = universe
	}
	return marshal(kept)
}

// gatherDeps records src's direct dependency directories into adj (keyed by
// module directory) and recurses into each dependency, memoized by directory so
// shared and diamond dependencies are walked once. It returns src's module
// directory. The module graph is a DAG, so the visited-marker cannot deadlock.
func gatherDeps(ctx context.Context, src *dagger.ModuleSource, adj map[string][]string) (string, error) {
	root, err := src.SourceRootSubpath(ctx)
	if err != nil {
		return "", err
	}
	if _, seen := adj[root]; seen {
		return root, nil
	}
	adj[root] = nil // mark visited before recursing
	deps, err := src.Dependencies(ctx)
	if err != nil {
		return "", err
	}
	dirs := make([]string, 0, len(deps))
	for i := range deps {
		dep := deps[i]
		depDir, err := gatherDeps(ctx, &dep, adj)
		if err != nil {
			return "", err
		}
		dirs = append(dirs, depDir)
	}
	adj[root] = dirs
	return root, nil
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func marshal(v []string) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
