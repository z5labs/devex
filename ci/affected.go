package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
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
// base/head are the commit SHAs to diff. The CI caller passes the PR base and
// head, or the push's before/after; an empty or all-zeros base (new branch,
// missing base) yields the full universe.
//
// The whole computation is in-engine. Both the check universe and the dependency
// graph are enumerated once from the live Dagger Workspace
// (dag.CurrentWorkspace().Checks().List), so nothing goes stale and no separate
// `dagger check -l` stage is needed: every check maps to its owning module via
// Check.OriginalModule, and each module's transitive dependency closure is walked
// via ModuleSource.Dependencies. The change set is the workspace's .git exported
// to scratch and diffed base...head (three-dot, merge-base) with a pure-Go git
// implementation — no workflow-side git, no helper container. The pure selection
// then runs in affectedpkg so the logic stays unit-testable.
//
// A changed path counts against a module only when it is one of that module's
// *inputs* — a member of the source context Dagger uploads for it — rather than
// merely living under its directory (#195). The source contexts are read from
// the engine itself (ModuleSource.ContextDirectory), never modelled here, so a
// file a module declared out of its context via dagger.json "include" cannot
// affect that module's checks, and Dagger enforces that rather than this code
// asserting it. Only the modules that own a changed path are resolved.
//
// Fail-safe throughout: an unusable diff range or a failed git diff yields the
// full universe; if a single check's module cannot be resolved it is kept, never
// skipped; if a module's source context cannot be read, everything under it
// counts as an input. If the Workspace enumeration itself fails (or a check has
// no readable name) the function errors — with no independent universe to fall
// back to, CI fails closed rather than risk silently under-running. The
// update-dagger workflow guards against a Dagger upgrade making this
// experimental enumeration diverge from stable `dagger check -l`.
//
// +cache="never"
func (ci *Ci) AffectedChecks(ctx context.Context, base string, head string) (string, error) {
	list, err := dag.CurrentWorkspace().Checks().List(ctx)
	if err != nil {
		return "", fmt.Errorf("list workspace checks: %w", err)
	}

	var universe []string
	checkModule := make(map[string]string, len(list))
	adj := map[string][]string{}
	sources := map[string]*dagger.ModuleSource{}
	for i := range list {
		chk := list[i]
		name, err := chk.Name(ctx)
		if err != nil {
			// Can't name a check -> can't safely account for it. Fail closed
			// rather than silently drop it from the universe.
			return "", fmt.Errorf("read check name: %w", err)
		}
		universe = append(universe, name)
		root, err := gatherDeps(ctx, chk.OriginalModule().Source(), adj, sources)
		if err != nil {
			// Leave this check out of checkModule -> Select keeps it (fail-safe).
			fmt.Fprintf(os.Stderr, "affected-checks: cannot resolve module for %q (%v); keeping it\n", name, err)
			continue
		}
		checkModule[name] = root
	}

	b, h, ok := affectedpkg.DiffRange(base, head)
	if !ok {
		// No usable diff range (new branch, missing base, ...) -> full suite.
		return marshal(universe)
	}
	changes, err := changedPaths(ctx, b, h)
	if err != nil {
		fmt.Fprintf(os.Stderr, "affected-checks: git diff failed (%v); running full suite\n", err)
		return marshal(universe)
	}

	moduleDirs, err := workspaceModuleDirs(ctx)
	if err != nil {
		// The dependency graph knows every module a check can reach, which is
		// enough to attribute; it just cannot see modules nothing depends on.
		fmt.Fprintf(os.Stderr, "affected-checks: cannot enumerate modules (%v); falling back to the dependency graph\n", err)
		moduleDirs = graphModuleDirs(adj)
	}

	srcs := resolveSources(ctx, changes, moduleDirs, sources)
	closure := affectedpkg.BuildClosures(checkModule, adj)
	bindings := affectedpkg.AggregatorBindings(checkModule)
	changed, global := affectedpkg.Attribute(changes, moduleDirs, srcs, bindings)
	kept, full := affectedpkg.Select(universe, closure, changed, global)
	if full {
		kept = universe
	}
	return marshal(kept)
}

// workspaceModuleDirs returns every module directory in the workspace, repo-
// relative, with the root module reported as ".".
//
// It walks for dagger.json rather than reading the dependency graph because the
// graph only contains modules some check depends on: an example module nothing
// imports would otherwise be invisible, and its files would fall through to the
// root module and force the full suite.
func workspaceModuleDirs(ctx context.Context) ([]string, error) {
	configs, err := dag.CurrentWorkspace().Directory(".").Glob(ctx, "**/dagger.json")
	if err != nil {
		return nil, err
	}
	if len(configs) == 0 {
		return nil, fmt.Errorf("no dagger.json found in the workspace")
	}
	dirs := make([]string, 0, len(configs))
	for _, c := range configs {
		dirs = append(dirs, path.Dir(c))
	}
	return dirs, nil
}

// graphModuleDirs is the fallback module set, derived from the dependency graph.
func graphModuleDirs(adj map[string][]string) []string {
	dirs := make([]string, 0, len(adj)+1)
	for d := range adj {
		if strings.HasPrefix(d, "daggerverse/") {
			dirs = append(dirs, d)
		}
	}
	return append(dirs, affectedpkg.RootPkg)
}

// resolveSources returns the source contexts of just those modules that own at
// least one changed path — typically one or two, never all of them.
//
// A module left out of the result is one whose context could not be read, or one
// the dependency walk never reached; Attribute treats both as "everything under
// it is an input" and so declines to narrow.
func resolveSources(ctx context.Context, changes []affectedpkg.Change, moduleDirs []string, sources map[string]*dagger.ModuleSource) map[string]map[string]bool {
	owners := map[string]bool{}
	for _, c := range changes {
		if dir, ok := affectedpkg.OwningModule(c.Path, moduleDirs); ok {
			owners[dir] = true
		}
	}

	srcs := make(map[string]map[string]bool, len(owners))
	for dir := range owners {
		src, ok := sources[dir]
		if !ok {
			continue
		}
		set, err := sourceContext(ctx, src, dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "affected-checks: cannot read the source context of %q (%v); treating everything under it as an input\n", dir, err)
			continue
		}
		srcs[dir] = set
	}
	return srcs
}

// sourceContext lists the repo-relative file paths Dagger uploads for the module
// rooted at dir. The context directory is rooted at the repository, so the glob
// is scoped back down to the module — except for the root module, whose context
// is already just its own source (ci/) plus the root dagger.json.
func sourceContext(ctx context.Context, src *dagger.ModuleSource, dir string) (map[string]bool, error) {
	pattern := "**"
	if dir != affectedpkg.RootPkg {
		pattern = dir + "/**"
	}
	paths, err := src.ContextDirectory().Glob(ctx, pattern)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		if strings.HasSuffix(p, "/") {
			continue // directory entry
		}
		set[p] = true
	}
	return set, nil
}

// changedPaths returns the repo-relative paths changed between base and head,
// each flagged with whether it survives at head. It materializes the workspace's
// .git directory into the module's scratch workdir (the Export/WorkdirFile
// runtime-I/O pattern) and diffs base...head with go-git — pure Go, no git binary
// and no helper container. Export is required because go-git reads an os
// filesystem, whereas dag.CurrentWorkspace().Directory returns a lazy Directory
// handle rather than mounted files.
func changedPaths(ctx context.Context, base, head string) ([]affectedpkg.Change, error) {
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
	changes, err := gitdiff.Changes(scratch, base, head)
	if err != nil {
		return nil, err
	}
	out := make([]affectedpkg.Change, 0, len(changes))
	for _, c := range changes {
		out = append(out, affectedpkg.Change{Path: c.Path, Deleted: c.Deleted})
	}
	return out, nil
}

// gatherDeps records src's direct dependency directories into adj (keyed by
// module directory) and recurses into each dependency, memoized by directory so
// shared and diamond dependencies are walked once. It returns src's module
// directory. The module graph is a DAG, so the visited-marker cannot deadlock.
//
// It also keeps each module's source handle in sources, which is how the source
// contexts are read later without re-resolving a module from a path string.
func gatherDeps(ctx context.Context, src *dagger.ModuleSource, adj map[string][]string, sources map[string]*dagger.ModuleSource) (string, error) {
	root, err := src.SourceRootSubpath(ctx)
	if err != nil {
		return "", err
	}
	if _, seen := adj[root]; seen {
		return root, nil
	}
	adj[root] = nil // mark visited before recursing
	sources[root] = src
	deps, err := src.Dependencies(ctx)
	if err != nil {
		return "", err
	}
	dirs := make([]string, 0, len(deps))
	for i := range deps {
		dep := deps[i]
		depDir, err := gatherDeps(ctx, &dep, adj, sources)
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

// marshal renders the selected check names as the JSON array the workflow's
// matrix consumes. A nil slice would marshal to `null`, which survives the
// workflow's non-empty check and then breaks fromJSON; emit `[]` instead.
func marshal(v []string) (string, error) {
	if v == nil {
		v = []string{}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
