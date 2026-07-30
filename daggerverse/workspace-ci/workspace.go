package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"dagger/workspace-ci/gitdiff"
	"dagger/workspace-ci/internal/dagger"
	"dagger/workspace-ci/planner"
)

// engineConcurrency bounds how many engine round trips are in flight at once.
// Each is cheap on its own — resolving a module source, globbing a context
// directory — but there is one per module in the workspace, so some concurrency is
// worth real wall-clock; the cap keeps a burst of traversals from crowding out the
// rest of the session.
const engineConcurrency = 8

// workspace is the repository a plan is computed from, materialized on disk with
// everything read off it that does not depend on the diff.
//
// Module sources are resolved from the exported copy rather than from the live
// Workspace because a module loaded from a Workspace handle resolves its
// dependencies' default-path contexts against host paths this runtime cannot see.
// Everything under the exported root is visible to it, which is also what lets a
// caller plan for a repository that is not their workspace at all.
type workspace struct {
	m       *WorkspaceCi
	root    string // absolute path of the exported repository
	cleanup func()

	// moduleDirs is every module in the repository, repo-relative and sorted, with
	// the root module reported as ".".
	moduleDirs []string
	sources    map[string]*dagger.ModuleSource
	// rootSource is the root module's own source subpath ("ci" in this repo), or
	// "" when the repository has no root module.
	rootSource string
	bindings   map[string]string
	adj        map[string][]string
	closures   map[string]map[string]bool

	// srcs, blobs and changes are filled in as they are needed, and a missing
	// entry always means "decline to narrow" or "decline to memoize", never
	// "nothing there".
	srcs    map[string]map[string]bool
	blobs   map[string]string
	changes []planner.Change

	// loaded records every module the planner had to load. Loading a module builds
	// its SDK runtime, which is the one expensive thing here, so the plan is
	// designed to touch as few as possible and this is how that is asserted rather
	// than timed.
	loaded []string
}

// load materializes the repository and reads everything about it that does not
// depend on the diff: which modules exist, how they depend on each other, and
// which generated bindings are attributable to a toolchain.
func (m *WorkspaceCi) load(ctx context.Context, repo *dagger.Directory, ws *dagger.Workspace) (*workspace, error) {
	if repo == nil {
		if ws == nil {
			ws = dag.CurrentWorkspace()
		}
		// "/" -- an absolute path, which resolves from the workspace boundary. A
		// relative "." resolves from the workspace's current directory, which is
		// the module source dir whenever the module is loaded from there.
		repo = ws.Directory("/")
	}

	root, cleanup, err := exportDir(ctx, repo)
	if err != nil {
		return nil, err
	}
	out := &workspace{m: m, root: root, cleanup: cleanup, srcs: map[string]map[string]bool{}}

	if out.moduleDirs, err = moduleRoots(root); err != nil {
		cleanup()
		return nil, err
	}
	out.sources = make(map[string]*dagger.ModuleSource, len(out.moduleDirs))
	for _, dir := range out.moduleDirs {
		out.sources[dir] = dag.ModuleSource(filepath.Join(root, dir))
	}
	out.rootSource, out.bindings = out.rootConfig(ctx)
	out.adj = out.dependencyGraph(ctx)
	out.closures = planner.BuildClosures(out.adj)
	return out, nil
}

// rootConfig reads what the root module contributes to attribution: where its own
// sources live, and which generated binding belongs to which toolchain.
//
// A repository with no root module is not an error — plenty of workspaces are a
// flat collection of modules — it just has no aggregator bindings and no
// module-shaped global inputs.
func (ws *workspace) rootConfig(ctx context.Context) (rootSource string, bindings map[string]string) {
	src, ok := ws.sources[planner.RootModule]
	if !ok {
		return "", nil
	}
	rootSource, err := src.SourceSubpath(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workspace-ci: cannot read the root module's source subpath (%v); its generated bindings will run everything\n", err)
		return "", nil
	}
	// The context directory is rooted at the repository, so the subpath comes back
	// repo-relative already; a root module whose source *is* the repository root
	// reports "".
	rootSource = strings.TrimPrefix(rootSource, "/")

	toolchains, err := src.Toolchains(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workspace-ci: cannot read the root module's toolchains (%v); their generated bindings will run everything\n", err)
		return rootSource, nil
	}
	names := map[string]string{}
	for i := range toolchains {
		tc := toolchains[i]
		if kind, err := tc.Kind(ctx); err != nil || kind != dagger.ModuleSourceKindLocalSource {
			continue
		}
		name, err := tc.ModuleName(ctx)
		if err != nil {
			continue
		}
		dir, err := tc.SourceRootSubpath(ctx)
		if err != nil {
			continue
		}
		names[name] = dir
	}
	return rootSource, planner.AggregatorBindings(rootSource, names)
}

// dependencyGraph returns each module's direct local dependencies, keyed by module
// directory.
//
// Only local dependencies are edges: a module pulled in by git ref cannot be
// changed by a commit in this repository, and its subpath inside its own repo
// would collide with a directory in this one. A module whose dependencies cannot
// be read is left out of the graph entirely, which makes it unresolved — and an
// unresolved module always runs.
func (ws *workspace) dependencyGraph(ctx context.Context) map[string][]string {
	var mu sync.Mutex
	adj := make(map[string][]string, len(ws.moduleDirs))

	var g errgroup.Group
	g.SetLimit(engineConcurrency)
	for _, dir := range ws.moduleDirs {
		g.Go(func() error {
			deps, err := ws.sources[dir].Dependencies(ctx)
			if err != nil {
				fmt.Fprintf(os.Stderr, "workspace-ci: cannot read the dependencies of %q (%v); it will always run\n", dir, err)
				return nil
			}
			var dirs []string
			for i := range deps {
				dep := deps[i]
				if kind, err := dep.Kind(ctx); err != nil || kind != dagger.ModuleSourceKindLocalSource {
					continue
				}
				sub, err := dep.SourceRootSubpath(ctx)
				if err != nil {
					fmt.Fprintf(os.Stderr, "workspace-ci: cannot resolve a dependency of %q (%v); it will always run\n", dir, err)
					return nil
				}
				dirs = append(dirs, sub)
			}
			mu.Lock()
			defer mu.Unlock()
			adj[dir] = dirs
			return nil
		})
	}
	g.Wait() //nolint:errcheck // every goroutine returns nil by construction
	return adj
}

// affected returns the modules whose checks the change between base and head could
// reach, and whether that is every module in the workspace.
func (ws *workspace) affected(ctx context.Context, base, head string) ([]string, bool) {
	ws.changes = changeSet(ws.root, base, head)

	var err error
	if ws.blobs, err = gitdiff.HeadBlobs(ws.root); err != nil {
		fmt.Fprintf(os.Stderr, "workspace-ci: cannot read HEAD (%v); memoization disabled\n", err)
		ws.blobs = nil
	}

	// Attribution only needs the source contexts of the handful of modules that own
	// a changed path.
	need := map[string]bool{}
	for _, c := range ws.changes {
		if dir, ok := planner.OwningModule(c.Path, ws.moduleDirs); ok {
			need[dir] = true
		}
	}
	ws.resolveSources(ctx, need)

	changed, global := planner.Attribute(ws.changes, ws.moduleDirs, ws.srcs, ws.bindings, ws.m.GlobalPaths)
	return planner.SelectModules(ws.moduleDirs, ws.closures, changed, global)
}

// legs enumerates the checks of each affected module and returns one leg per
// check.
//
// This is the only place a module is loaded, and only affected ones ever are. A
// module whose checks cannot be enumerated falls back to a single leg that runs
// all of them — coarser, never fewer.
func (ws *workspace) legs(ctx context.Context, affected []string) []planner.Entry {
	type result struct {
		dir     string
		entries []planner.Entry
	}
	results := make([]result, len(affected))

	var g errgroup.Group
	g.SetLimit(engineConcurrency)
	for i, dir := range affected {
		g.Go(func() error {
			entries, err := ws.moduleLegs(ctx, dir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "workspace-ci: cannot enumerate the checks of %q (%v); running all of them in one leg\n", dir, err)
				entries = []planner.Entry{planner.ModuleEntry(dir)}
			}
			results[i] = result{dir: dir, entries: entries}
			return nil
		})
	}
	g.Wait() //nolint:errcheck // every goroutine returns nil by construction

	var out []planner.Entry
	for _, r := range results {
		ws.loaded = append(ws.loaded, r.dir)
		out = append(out, r.entries...)
	}
	sort.Strings(ws.loaded)
	return out
}

// moduleLegs loads one module and returns a leg per check it declares. A module
// with no checks contributes none, which is why a workspace may hold modules that
// are only ever dependencies.
func (ws *workspace) moduleLegs(ctx context.Context, dir string) ([]planner.Entry, error) {
	mod := ws.sources[dir].AsModule()
	name, err := mod.Name(ctx)
	if err != nil {
		return nil, fmt.Errorf("read module name: %w", err)
	}
	checks, err := mod.Checks().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list checks: %w", err)
	}
	out := make([]planner.Entry, 0, len(checks))
	for i := range checks {
		check := checks[i]
		checkName, err := check.Name(ctx)
		if err != nil {
			return nil, fmt.Errorf("read check name: %w", err)
		}
		out = append(out, planner.CheckEntry(dir, name, checkName))
	}
	return out, nil
}

// hashingNeeds returns the module directories whose source contexts must be read
// to hash the plan: every module in every affected module's closure, plus the root
// module's closure, which is where the global inputs come from.
func (ws *workspace) hashingNeeds(affected []string) map[string]bool {
	need := map[string]bool{}
	for _, dir := range append([]string{planner.RootModule}, affected...) {
		for d := range ws.closures[dir] {
			need[d] = true
		}
	}
	return need
}

// hash stamps each leg with the input hash a pass on it may be recorded under.
//
// A leg keeps an empty hash — meaning "never memoize" — when it belongs to the
// root module, when its module's closure is unresolved, or when anything in that
// closure could not be hashed. A root-module leg is never memoized because its
// checks are the ones that read state their declared closure does not describe:
// Generated runs codegen for every module in the workspace, and the guarantee that
// generated files are derived from inputs that *are* hashed rests on it having run
// unconditionally.
func (ws *workspace) hash(legs []planner.Entry) []planner.Entry {
	if len(ws.blobs) == 0 {
		return legs
	}
	hasher, ok := planner.NewHasher(
		ws.closures[planner.RootModule],
		ws.srcs,
		ws.blobs,
		ws.bindings,
		ws.m.GlobalPaths,
		ws.nonGlobal(),
	)
	if !ok {
		fmt.Fprintf(os.Stderr, "workspace-ci: the global inputs are unhashable; memoization disabled\n")
		return legs
	}
	unhashable := map[string]bool{}
	out := make([]planner.Entry, 0, len(legs))
	for _, leg := range legs {
		if leg.Module != planner.RootModule {
			if closure, resolved := ws.closures[leg.Module]; !resolved {
				unhashable[leg.Module] = true
			} else if h, ok := hasher.Check(leg.Name, closure); !ok {
				unhashable[leg.Module] = true
			} else {
				leg.Hash = h
			}
		}
		out = append(out, leg)
	}
	for _, dir := range sortedKeys(unhashable) {
		fmt.Fprintf(os.Stderr, "workspace-ci: %q has inputs that cannot be hashed from git objects; its legs always run\n", dir)
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (ws *workspace) nonGlobal() []string {
	return planner.NonGlobalRootPaths(ws.rootSource)
}

// resolveSources reads the source contexts of the requested modules that have not
// been read yet.
//
// A module left out of the result is one whose context could not be read:
// attribution then treats everything under it as an input, and hashing treats it as
// unhashable, so both decline to narrow rather than guess.
func (ws *workspace) resolveSources(ctx context.Context, want map[string]bool) {
	var mu sync.Mutex

	var g errgroup.Group
	g.SetLimit(engineConcurrency)
	for dir := range want {
		src, known := ws.sources[dir]
		if !known {
			continue
		}
		mu.Lock()
		_, done := ws.srcs[dir]
		mu.Unlock()
		if done {
			continue
		}
		g.Go(func() error {
			set, err := sourceContext(ctx, src, dir)
			if err != nil {
				// Never fatal: the caller's fail-safes cover an unresolved module.
				fmt.Fprintf(os.Stderr, "workspace-ci: cannot read the source context of %q (%v); treating everything under it as an input\n", dir, err)
				return nil
			}
			mu.Lock()
			defer mu.Unlock()
			ws.srcs[dir] = set
			return nil
		})
	}
	g.Wait() //nolint:errcheck // every goroutine returns nil by construction
}

// sourceContext lists the repo-relative file paths Dagger uploads for the module
// rooted at dir. The context directory is rooted at the repository, so the glob is
// scoped back down to the module — otherwise a module with dependencies would
// claim its dependencies' files as its own inputs, and the dependency closure is
// what propagates those. The root module is the exception: its context is already
// just its own source plus the root dagger.json.
func sourceContext(ctx context.Context, src *dagger.ModuleSource, dir string) (map[string]bool, error) {
	pattern := "**"
	if dir != planner.RootModule {
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

// exportDir materializes a directory into the module's scratch workdir (the
// Export/workdir runtime-I/O pattern) and returns its absolute path plus a cleanup
// that is always safe to call.
//
// Export is required twice over: go-git reads an os filesystem, and module sources
// are resolved as local paths, which have to exist on disk. A lazy Directory
// handle is neither.
func exportDir(ctx context.Context, dir *dagger.Directory) (string, func(), error) {
	suffix, err := uniqueSuffix()
	if err != nil {
		return "", func() {}, err
	}
	scratch := "workspace-" + suffix
	cleanup := func() { os.RemoveAll(scratch) }

	if _, err := dir.Export(ctx, scratch); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("export the repository: %w", err)
	}
	abs, err := filepath.Abs(scratch)
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	return abs, cleanup, nil
}

// moduleRoots returns every module source root under root, repo-relative and
// sorted, with the root module reported as ".".
//
// It walks for dagger.json rather than reading the dependency graph because the
// graph only contains modules some other module depends on: a module nothing
// imports would otherwise be invisible, and its files would fall through to the
// root module and force everything to run.
//
// A workspace with no modules is an error rather than an empty plan: an empty
// matrix skips the run job and passes the gate having run nothing.
func moduleRoots(root string) ([]string, error) {
	var modules []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() != "dagger.json" {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		modules = append(modules, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk the workspace: %w", err)
	}
	if len(modules) == 0 {
		return nil, fmt.Errorf("no dagger.json found in %s: a plan with no modules would pass a gate having run nothing", root)
	}
	sort.Strings(modules)
	return modules, nil
}

func uniqueSuffix() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
