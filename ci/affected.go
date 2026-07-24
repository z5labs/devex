package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dagger/ci/affectedpkg"
	"dagger/ci/gitdiff"
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
//   - base/head are the commit SHAs to diff. The CI caller passes the PR base and
//     head, or the push's before/after; an empty or all-zeros base (new branch,
//     missing base) yields the full universe.
//
// The whole computation is in-engine: the workspace's .git directory is exported
// to scratch and diffed base...head (three-dot, merge-base) with a pure-Go git
// implementation — no workflow-side git, no helper container. The dependency
// graph is read from the live Dagger Workspace (Dagger's own resolved module
// graph, so it never goes stale): every check maps to its owning module via
// Check.OriginalModule, and each module's transitive dependency closure is walked
// via ModuleSource.Dependencies. The pure selection then runs in affectedpkg so
// the logic stays unit-testable.
//
// Fail-safe throughout: an unusable diff range or a failed git diff yields the
// full universe; if the Workspace cannot be read at all, the full universe is
// returned; if a single check cannot be resolved, it is left unresolved and
// therefore always kept. A check is never skipped because of a gap.
//
// +cache="never"
func (ci *Ci) AffectedChecks(ctx context.Context, checks string, base string, head string) (string, error) {
	var universe []string
	if err := json.Unmarshal([]byte(checks), &universe); err != nil {
		return "", fmt.Errorf("parse checks universe: %w", err)
	}

	b, h, ok := affectedpkg.DiffRange(base, head)
	if !ok {
		// No usable diff range (new branch, missing base, ...) -> full suite.
		return marshal(universe)
	}
	changedFiles, err := changedPaths(ctx, b, h)
	if err != nil {
		fmt.Fprintf(os.Stderr, "affected-checks: git diff failed (%v); running full suite\n", err)
		return marshal(universe)
	}

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

// changedPaths returns the repo-relative paths changed between base and head. It
// materializes the workspace's .git directory into the module's scratch workdir
// (the Export/WorkdirFile runtime-I/O pattern) and diffs base...head with go-git
// — pure Go, no git binary and no helper container. Export is required because
// go-git reads an os filesystem, whereas dag.CurrentWorkspace().Directory returns
// a lazy Directory handle rather than mounted files.
func changedPaths(ctx context.Context, base, head string) ([]string, error) {
	suffix, err := uniqueSuffix()
	if err != nil {
		return nil, err
	}
	scratch := "affected-" + suffix
	defer os.RemoveAll(scratch)

	gitDir := dag.CurrentWorkspace().Directory(".git")
	if _, err := gitDir.Export(ctx, filepath.Join(scratch, ".git")); err != nil {
		return nil, fmt.Errorf("export .git: %w", err)
	}
	return gitdiff.ChangedFiles(scratch, base, head)
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

func uniqueSuffix() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func marshal(v []string) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
