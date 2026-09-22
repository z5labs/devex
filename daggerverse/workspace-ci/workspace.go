package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
// It is held two ways because the two things done to it need different handles.
// go-git reads an os filesystem, so the repository is exported to disk; module
// sources are resolved off the Directory, because a module runtime cannot load a
// local module source at all (see moduleSource). Both describe the same tree,
// which is also what lets a caller plan for a repository that is not their
// workspace.
type workspace struct {
	m       *WorkspaceCi
	dir     *dagger.Directory // the repository
	root    string            // absolute path of the exported copy
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
			return nil, fmt.Errorf("neither repo nor workspace was supplied: a Dagger CLI fills workspace in from the caller's own, but a module calling this one has no workspace to offer and has to pass repo")
		}
		// "/" -- an absolute path, which resolves from the workspace boundary. A
		// relative "." resolves from the workspace's current directory, which is
		// the module source dir whenever the module is loaded from there.
		repo = ws.Directory("/")
	}

	root, _, cleanup, err := exportDir(ctx, repo)
	if err != nil {
		return nil, err
	}
	out := &workspace{m: m, dir: repo, root: root, cleanup: cleanup, srcs: map[string]map[string]bool{}}

	if out.moduleDirs, err = moduleRoots(root); err != nil {
		cleanup()
		return nil, err
	}
	out.sources = make(map[string]*dagger.ModuleSource, len(out.moduleDirs))
	for _, dir := range out.moduleDirs {
		out.sources[dir] = moduleSource(repo, dir)
	}
	out.rootSource, out.bindings = out.rootConfig(ctx)
	out.adj = out.dependencyGraph(ctx)
	out.closures = planner.BuildClosures(out.adj)
	return out, nil
}

// moduleSource resolves the module rooted at dir -- repo-relative, "." for the
// root module -- out of the repository directory.
//
// It is resolved from the Directory rather than from a path under the exported
// copy because a module runtime cannot load a local module source: the engine
// serves a local path from the session's own client, which is the host, where
// this container's scratch directory does not exist. Everything the module needs
// is in the Directory, and its context is the whole repository, which is what
// lets a dependency spelled "../../crypto" resolve.
func moduleSource(repo *dagger.Directory, dir string) *dagger.ModuleSource {
	if dir == planner.RootModule {
		return repo.AsModuleSource()
	}
	return repo.AsModuleSource(dagger.DirectoryAsModuleSourceOpts{SourceRootPath: dir})
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

	return rootSource, planner.AggregatorBindings(rootSource, ws.toolchains())
}

// toolchains maps each local toolchain the root module installs to its
// repo-relative source root, read straight out of the root dagger.json.
//
// It is read from disk rather than through ModuleSource.toolchains because that
// field no longer exists: Dagger v1 moved toolchains (and blueprints) to
// workspaces and refuses to load a module whose dagger.json still declares them.
// The config is still the only place the mapping was ever written down, and the
// exported tree is already on disk, so reading it here costs no engine round trip
// and keeps #179's attribution working for the workspaces that do declare
// toolchains -- a repository whose root module cannot be loaded can still be
// planned for, since only affected modules are ever loaded.
//
// Anything unreadable yields no mapping at all, which is the fail-safe direction:
// an unattributed binding belongs to the root module and runs everything.
func (ws *workspace) toolchains() map[string]string {
	cfg, err := readModuleConfig(filepath.Join(ws.root, planner.RootModule))
	if err != nil {
		fmt.Fprintf(os.Stderr, "workspace-ci: cannot read the root module's config (%v); its generated bindings will run everything\n", err)
		return nil
	}
	out := make(map[string]string, len(cfg.Toolchains))
	for _, tc := range cfg.Toolchains {
		if tc.Source == "" {
			continue
		}
		// A toolchain's source is relative to the dagger.json that declares it,
		// which for the root module is the repository root -- so the cleaned path
		// is already the repo-relative source root. A git ref has no dagger.json
		// under the repository and is skipped: a commit here cannot change it, and
		// so is anything that climbs out of the repository, which no path in a
		// plan may name.
		dir := filepath.Clean(tc.Source)
		if filepath.IsAbs(dir) || dir == ".." || strings.HasPrefix(dir, "../") {
			continue
		}
		sub, err := readModuleConfig(filepath.Join(ws.root, dir))
		if err != nil {
			continue
		}
		name := tc.Name
		if name == "" {
			name = sub.Name
		}
		if name == "" {
			continue
		}
		out[name] = dir
	}
	return out
}

// moduleConfig is the part of a dagger.json this module reads.
type moduleConfig struct {
	Name string `json:"name"`
	// Source is where the module's own code lives, relative to the module root.
	// Empty means the root itself.
	Source string `json:"source"`
	// Include is the module's context patterns, an entry prefixed with "!" being
	// an exclusion.
	Include    []string `json:"include"`
	Toolchains []struct {
		Name   string `json:"name"`
		Source string `json:"source"`
	} `json:"toolchains"`
}

// readModuleConfig reads the dagger.json of the module rooted at dir.
func readModuleConfig(dir string) (moduleConfig, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "dagger.json"))
	if err != nil {
		return moduleConfig{}, err
	}
	var cfg moduleConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return moduleConfig{}, fmt.Errorf("parse %s: %w", filepath.Join(dir, "dagger.json"), err)
	}
	return cfg, nil
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
				// GIT_SOURCE is the one kind to drop, and the test is written that
				// way round on purpose: a module's dependency reports LOCAL_SOURCE
				// when the module was resolved from a path and DIR_SOURCE when it was
				// resolved from a Directory, which is how this module resolves every
				// one of them. Matching LOCAL_SOURCE instead would quietly empty the
				// graph, leaving every module with no dependents and a change to a
				// shared module reaching nothing.
				if kind, err := dep.Kind(ctx); err != nil || kind == dagger.ModuleSourceKindGitSource {
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

// partitionSplits divides the affected modules into the ones the run-everything
// path may emit as a single coarse leg and the ones the caller asked to have
// enumerated anyway.
//
// A named module that is not in the workspace is reported rather than ignored: it
// is a typo, and its whole effect would otherwise be nothing at all.
func (ws *workspace) partitionSplits(affected []string) (coarse, split []string) {
	want := make(map[string]bool, len(ws.m.SplitModules))
	for _, dir := range ws.m.SplitModules {
		want[dir] = true
	}
	for _, dir := range affected {
		if want[dir] {
			split = append(split, dir)
			delete(want, dir)
			continue
		}
		coarse = append(coarse, dir)
	}
	for _, dir := range sortedKeys(want) {
		fmt.Fprintf(os.Stderr, "workspace-ci: %q was named as a split module but is not in the plan; its checks will share one leg\n", dir)
	}
	return coarse, split
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
		if _, known := ws.sources[dir]; !known {
			continue
		}
		mu.Lock()
		_, done := ws.srcs[dir]
		mu.Unlock()
		if done {
			continue
		}
		g.Go(func() error {
			set, err := ws.sourceContext(ctx, dir)
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

// sourceContext lists the repo-relative file paths that make up the source of the
// module rooted at dir: its config file, the subtree its config points its source
// at, and whatever its include patterns add, less whatever they take away.
//
// That set is what decides whether a changed path is an input to a module at all,
// and it is computed here rather than read off ModuleSource.contextDirectory
// because on Dagger v1 that field cannot answer the question. A module resolved
// from a Directory — the only kind a module runtime can resolve, see moduleSource
// — reports the whole Directory as its context, unfiltered and identical for
// every module in the workspace. Taken at face value the root module would own
// every path in the repository, which makes every change global and, because an
// untracked file anywhere would then be one of its inputs, every leg unhashable.
//
// The filtering itself is still the engine's: the patterns are handed to
// Directory.filter rather than matched here. What this reproduces is only how a
// module config names them — an include entry prefixed with "!" is an exclusion,
// and paths are relative to the module's own root.
//
// Scoping to the module also keeps a module with dependencies from claiming its
// dependencies' files as its own inputs; the dependency closure is what propagates
// those.
func (ws *workspace) sourceContext(ctx context.Context, dir string) (map[string]bool, error) {
	cfg, err := readModuleConfig(filepath.Join(ws.root, dir))
	if err != nil {
		return nil, err
	}

	include := []string{"dagger.json"}
	if src := strings.Trim(filepath.Clean(cfg.Source), "/"); src == "" || src == "." {
		include = append(include, "**")
	} else {
		include = append(include, src+"/**")
	}
	var exclude []string
	for _, pattern := range cfg.Include {
		if rest, found := strings.CutPrefix(pattern, "!"); found {
			exclude = append(exclude, rest)
			continue
		}
		include = append(include, pattern)
	}

	paths, err := ws.dir.Directory(dir).
		Filter(dagger.DirectoryFilterOpts{Include: include, Exclude: exclude}).
		Glob(ctx, "**")
	if err != nil {
		return nil, err
	}

	prefix := ""
	if dir != planner.RootModule {
		prefix = dir + "/"
	}
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		if strings.HasSuffix(p, "/") {
			continue // directory entry
		}
		set[prefix+p] = true
	}
	return set, nil
}

// exportDir materializes a directory into the module's scratch workdir (the
// Export/workdir runtime-I/O pattern) and returns its path relative to that
// workdir and absolutely, plus a cleanup that is always safe to call.
//
// Export is what go-git needs: it reads an os filesystem, and a lazy Directory
// handle is not one. The relative name is returned as well because it is what
// dag.CurrentModule().Workdir takes, which is how a tree this module has written
// to disk is read back as a Directory.
func exportDir(ctx context.Context, dir *dagger.Directory) (abs, rel string, cleanup func(), err error) {
	suffix, err := uniqueSuffix()
	if err != nil {
		return "", "", func() {}, err
	}
	rel = "workspace-" + suffix
	cleanup = func() { os.RemoveAll(rel) }

	if _, err := dir.Export(ctx, rel); err != nil {
		cleanup()
		return "", "", func() {}, fmt.Errorf("export the repository: %w", err)
	}
	abs, err = filepath.Abs(rel)
	if err != nil {
		cleanup()
		return "", "", func() {}, err
	}
	return abs, rel, cleanup, nil
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
