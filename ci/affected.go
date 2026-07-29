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
	"sync"

	"golang.org/x/sync/errgroup"

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
// change could plausibly affect — the input for CI's dynamic matrix. Each
// element is a {"name", "hash"} object: the check to run, and the input hash
// under which a pass may be recorded ("" when the check must never be memoized).
//
// base/head are the commit SHAs to diff. The CI caller passes the PR base and
// head, or the push's before/after; an empty or all-zeros base (new branch,
// missing base) yields the full universe.
//
// knownGood is a JSON array of input hashes that a previous run already proved
// good; a selected check whose hash appears there is dropped (#238). It is
// optional, and anything unparseable is treated as empty, so a broken or
// unavailable store costs speed and never correctness. See InputHashes for what
// the hash covers and MemoTrusted for when a recorded pass may be honoured at
// all.
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
func (ci *Ci) AffectedChecks(
	ctx context.Context,
	base string,
	head string,
	// +optional
	// +default="[]"
	knownGood string,
) (string, error) {
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

	closure := affectedpkg.BuildClosures(checkModule, adj)
	bindings := affectedpkg.AggregatorBindings(checkModule)

	// One export of the workspace's .git feeds both halves of the answer: the
	// base...head change set narrows selection, and HEAD's blob object ids are
	// what the input hashes are built from. They fail independently — a bad diff
	// range still allows memoization, and an unreadable HEAD still allows
	// narrowing.
	scratch, cleanup, err := exportGit(ctx)
	defer cleanup()
	var changes []affectedpkg.Change
	var blobs map[string]string
	if err != nil {
		fmt.Fprintf(os.Stderr, "affected-checks: cannot export .git (%v); running the full suite with no memoization\n", err)
	} else {
		if b, h, ok := affectedpkg.DiffRange(base, head); !ok {
			// No usable diff range (new branch, missing base, ...) -> full suite.
			fmt.Fprintf(os.Stderr, "affected-checks: no usable diff range (base=%q head=%q); running the full suite\n", base, head)
		} else if changes, err = diffChanges(scratch, b, h); err != nil {
			fmt.Fprintf(os.Stderr, "affected-checks: git diff failed (%v); running the full suite\n", err)
			changes = nil
		}
		if blobs, err = gitdiff.HeadBlobs(scratch); err != nil {
			fmt.Fprintf(os.Stderr, "affected-checks: cannot read HEAD (%v); memoization disabled\n", err)
			blobs = nil
		}
	}

	moduleDirs, err := workspaceModuleDirs(ctx)
	if err != nil {
		// The dependency graph knows every module a check can reach, which is
		// enough to attribute; it just cannot see modules nothing depends on.
		fmt.Fprintf(os.Stderr, "affected-checks: cannot enumerate modules (%v); falling back to the dependency graph\n", err)
		moduleDirs = graphModuleDirs(adj)
	}

	srcs := resolveSources(ctx, sourcesNeeded(changes, moduleDirs, closure, blobs != nil), sources)
	changed, global := affectedpkg.Attribute(changes, moduleDirs, srcs, bindings)
	kept, full := affectedpkg.Select(universe, closure, changed, global)
	if full {
		kept = universe
	}

	var hashes map[string]string
	if len(blobs) > 0 {
		hashes = affectedpkg.InputHashes(closure, srcs, blobs, bindings)
	}
	if affectedpkg.MemoTrusted(changes, moduleDirs, srcs, bindings) {
		run, skipped := affectedpkg.MemoFilter(kept, hashes, parseKnownGood(knownGood))
		if len(skipped) > 0 {
			fmt.Fprintf(os.Stderr, "affected-checks: %d check(s) already passed on these inputs; skipping %v\n", len(skipped), skipped)
		}
		kept = run
	} else {
		fmt.Fprintf(os.Stderr, "affected-checks: a global input changed; ignoring recorded passes\n")
	}
	return marshalJobs(kept, hashes)
}

// sourcesNeeded returns the module directories whose source contexts must be
// read from the engine.
//
// Attribution alone only needs the handful of modules that own a changed path.
// Hashing needs every module in every check's closure, plus the root module for
// the global inputs — so the wider set is only paid for when there are object
// ids to hash against.
func sourcesNeeded(changes []affectedpkg.Change, moduleDirs []string, closure map[string]map[string]bool, hashing bool) map[string]bool {
	need := map[string]bool{}
	for _, c := range changes {
		if dir, ok := affectedpkg.OwningModule(c.Path, moduleDirs); ok {
			need[dir] = true
		}
	}
	if !hashing {
		return need
	}
	need[affectedpkg.RootPkg] = true
	for _, dirs := range closure {
		for dir := range dirs {
			need[dir] = true
		}
	}
	return need
}

// parseKnownGood turns the workflow's JSON array of recorded input hashes into a
// set. An unparseable value yields the empty set, which memoizes nothing: a
// store that cannot be read must cost speed, never correctness.
func parseKnownGood(raw string) map[string]bool {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var hashes []string
	if err := json.Unmarshal([]byte(raw), &hashes); err != nil {
		fmt.Fprintf(os.Stderr, "affected-checks: cannot parse known-good hashes (%v); running every selected check\n", err)
		return nil
	}
	set := make(map[string]bool, len(hashes))
	for _, h := range hashes {
		set[h] = true
	}
	return set
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

// resolveSources returns the source contexts of the requested modules.
//
// A module left out of the result is one whose context could not be read, or one
// the dependency walk never reached; Attribute treats both as "everything under
// it is an input" and so declines to narrow, while InputHashes treats them as
// unhashable and so declines to memoize.
//
// The engine round-trips are independent, so they run concurrently — bounded,
// because hashing asks for every module in the workspace rather than the one or
// two that own a changed path.
func resolveSources(ctx context.Context, want map[string]bool, sources map[string]*dagger.ModuleSource) map[string]map[string]bool {
	var mu sync.Mutex
	srcs := make(map[string]map[string]bool, len(want))

	var g errgroup.Group
	g.SetLimit(sourceContextConcurrency)
	for dir := range want {
		src, ok := sources[dir]
		if !ok {
			continue
		}
		g.Go(func() error {
			set, err := sourceContext(ctx, src, dir)
			if err != nil {
				// Never fatal: the caller's fail-safes cover an unresolved module.
				fmt.Fprintf(os.Stderr, "affected-checks: cannot read the source context of %q (%v); treating everything under it as an input\n", dir, err)
				return nil
			}
			mu.Lock()
			defer mu.Unlock()
			srcs[dir] = set
			return nil
		})
	}
	g.Wait() //nolint:errcheck // every goroutine returns nil by construction
	return srcs
}

// sourceContextConcurrency bounds the in-flight ContextDirectory globs. Each is
// a round trip to the engine and there are ~50 modules, so some concurrency is
// worth real wall-clock in the list job; the cap keeps a burst of glob traversals
// from crowding out the rest of the session.
const sourceContextConcurrency = 8

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

// exportGit materializes the workspace's .git directory into the module's
// scratch workdir (the Export/WorkdirFile runtime-I/O pattern) and returns the
// repository root to read it from, plus a cleanup that is always safe to call.
//
// Export is required because go-git reads an os filesystem, whereas
// dag.CurrentWorkspace().Directory returns a lazy Directory handle rather than
// mounted files. One export serves both the diff and the HEAD tree walk.
func exportGit(ctx context.Context) (string, func(), error) {
	suffix, err := uniqueSuffix()
	if err != nil {
		return "", func() {}, err
	}
	scratch := "affected-" + suffix
	cleanup := func() { os.RemoveAll(scratch) }

	gitDir := dag.CurrentWorkspace().Directory(".git")
	if _, err := gitDir.Export(ctx, filepath.Join(scratch, ".git")); err != nil {
		return "", cleanup, fmt.Errorf("export .git: %w", err)
	}
	return scratch, cleanup, nil
}

// diffChanges returns the repo-relative paths changed between base and head,
// each flagged with whether it survives at head, diffed base...head with go-git
// — pure Go, no git binary and no helper container.
func diffChanges(repoDir, base, head string) ([]affectedpkg.Change, error) {
	changes, err := gitdiff.Changes(repoDir, base, head)
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

// selectedCheck is one entry of the JSON array the workflow's route step
// consumes. Hash is the input hash under which a pass may be recorded, and is
// empty for a check that must never be memoized — a ci:* check, or one whose
// inputs could not be hashed. The workflow records nothing for an empty hash.
type selectedCheck struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
}

// marshalJobs renders the selected checks as the JSON array the workflow's
// matrix consumes. A nil slice would marshal to `null`, which survives the
// workflow's non-empty check and then breaks fromJSON; emit `[]` instead.
func marshalJobs(names []string, hashes map[string]string) (string, error) {
	out := make([]selectedCheck, 0, len(names))
	for _, name := range names {
		out = append(out, selectedCheck{Name: name, Hash: hashes[name]})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
