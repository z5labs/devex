package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path"
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
// It is held three ways because the things done to it need different handles.
// go-git reads an os filesystem, so the repository is exported to disk. Modules
// are discovered and resolved through a Workspace, which is what reads each
// module's own config — either shape — and filters its context the way the engine
// does. And checks are enumerated off a Workspace built from the repository
// Directory, the one kind that can have a module installed into it (see
// moduleLegs). All three describe the same tree, which is also what lets a
// caller plan for a repository that is not their workspace.
type workspace struct {
	m       *WorkspaceCi
	ws      *dagger.Workspace // where module sources resolve
	dir     *dagger.Directory // the repository
	root    string            // absolute path of the exported copy
	cleanup func()

	// moduleDirs is every module in the repository, repo-relative and sorted, with
	// the root module reported as ".".
	moduleDirs []string
	sources    map[string]*dagger.ModuleSource
	// names is each module's own name, read from its config as it is needed.
	names map[string]string
	// rootSource is the root module's own source subpath ("ci" in this repo), or
	// "" when the repository has no root module.
	rootSource string
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
// depend on the diff: which modules exist and how they depend on each other.
//
// Modules resolve through callingWorkspace — the one a Dagger CLI fills in — or,
// when the caller names a repository instead, through that Directory made into a
// workspace. Either way it is Workspace.moduleSource that reads a module's config,
// so both config shapes resolve and each module's context is filtered by the
// engine's own rules rather than by a copy of them here.
func (m *WorkspaceCi) load(ctx context.Context, repo *dagger.Directory, callingWorkspace *dagger.Workspace) (*workspace, error) {
	ws := callingWorkspace
	switch {
	case repo != nil:
		ws = repo.AsWorkspace()
	case callingWorkspace == nil:
		return nil, fmt.Errorf("neither repo nor workspace was supplied: a Dagger CLI fills workspace in from the caller's own, but a module calling this one has no workspace to offer and has to pass repo")
	default:
		// "/" -- an absolute path, which resolves from the workspace boundary. A
		// relative "." resolves from the workspace's current directory, which is
		// the module source dir whenever the module is loaded from there.
		repo = callingWorkspace.Directory("/")
	}

	root, _, cleanup, err := exportDir(ctx, repo)
	if err != nil {
		return nil, err
	}
	out := &workspace{
		m: m, ws: ws, dir: repo, root: root, cleanup: cleanup,
		names: map[string]string{},
		srcs:  map[string]map[string]bool{},
	}

	if out.moduleDirs, err = discoverModules(ctx, ws); err != nil {
		cleanup()
		return nil, err
	}
	out.sources = make(map[string]*dagger.ModuleSource, len(out.moduleDirs))
	for _, dir := range out.moduleDirs {
		out.sources[dir] = moduleSource(ws, dir)
	}
	out.rootSource = out.rootConfig(ctx)
	out.adj = out.dependencyGraph(ctx)
	out.closures = planner.BuildClosures(out.adj)
	return out, nil
}

// configFiles are the files that make a directory a module: the legacy
// dagger.json and the dagger-module.toml a migrated workspace uses. Where a
// directory holds both, the engine reads dagger-module.toml, and since every
// module resolves through the engine that is the one that wins here too.
var configFiles = []string{"dagger.json", "dagger-module.toml"}

// discoverModules returns every module directory in the workspace, repo-relative
// and sorted, with the root module reported as ".".
//
// It looks for config files rather than reading the dependency graph because the
// graph only contains modules some other module depends on: a module nothing
// imports would otherwise be invisible, and its files would fall through to the
// root module and force everything to run. Plan and Generated both discover
// through here, so a config shape one of them recognises is one both do.
//
// A workspace with no modules is an error rather than an empty plan: an empty
// matrix skips the run job and passes the gate having run nothing.
func discoverModules(ctx context.Context, ws *dagger.Workspace) ([]string, error) {
	// From the workspace root, whatever directory the caller happened to be in:
	// findRoots answers relative to the workspace's current directory.
	found, err := ws.WithWorkdir(".").FindRoots(ctx, configFiles, dagger.WorkspaceFindRootsOpts{
		Exclude: []string{"**/.git/**"},
	})
	if err != nil {
		return nil, fmt.Errorf("discover the workspace's modules: %w", err)
	}
	seen := map[string]bool{}
	var modules []string
	for _, dir := range found {
		dir = path.Clean(strings.TrimPrefix(dir, "/"))
		if dir == "" {
			dir = planner.RootModule
		}
		if dir == ".." || strings.HasPrefix(dir, "../") || seen[dir] {
			continue
		}
		seen[dir] = true
		modules = append(modules, dir)
	}
	if len(modules) == 0 {
		return nil, fmt.Errorf("no %s found in the workspace: a plan with no modules would pass a gate having run nothing", strings.Join(configFiles, " or "))
	}
	sort.Strings(modules)
	return modules, nil
}

// moduleSource resolves the module rooted at dir -- repo-relative, "." for the
// root module -- through the workspace.
func moduleSource(ws *dagger.Workspace, dir string) *dagger.ModuleSource {
	if dir == planner.RootModule {
		return ws.ModuleSource("/")
	}
	return ws.ModuleSource("/" + dir)
}

// rootConfig reads what the root module contributes to attribution: where its own
// sources live.
//
// A repository with no root module is not an error — plenty of workspaces are a
// flat collection of modules — it just has no module-shaped global inputs.
func (ws *workspace) rootConfig(ctx context.Context) (rootSource string) {
	src, ok := ws.sources[planner.RootModule]
	if !ok {
		return ""
	}
	rootSource, err := src.SourceSubpath(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workspace-ci: cannot read the root module's source subpath (%v); its generated bindings will run everything\n", err)
		return ""
	}
	// The subpath comes back repo-relative already; a root module whose source
	// *is* the repository root reports "" or ".".
	return strings.TrimPrefix(rootSource, "/")
}

// resolveNames reads the name of each module in dirs from its config. Nothing is
// built: a name is what `dagger check --module` needs, and every leg carries one.
//
// A name that cannot be read is an error rather than a leg without one. A leg with
// no name would run the checks of the module at the workspace root as well as its
// own, and a module whose config cannot be read would fail its leg regardless.
func (ws *workspace) resolveNames(ctx context.Context, dirs []string) error {
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(engineConcurrency)
	for _, dir := range dirs {
		mu.Lock()
		_, done := ws.names[dir]
		mu.Unlock()
		if done {
			continue
		}
		g.Go(func() error {
			name, err := ws.sources[dir].ModuleName(gctx)
			if err != nil {
				return fmt.Errorf("read the name of module %q: %w", dir, err)
			}
			mu.Lock()
			defer mu.Unlock()
			ws.names[dir] = name
			return nil
		})
	}
	return g.Wait()
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
				// when the module was resolved through the workspace a CLI filled in,
				// and DIR_SOURCE when it was resolved through a Directory made into
				// one, which is what --repo and the tests do. Matching LOCAL_SOURCE
				// instead would quietly empty the graph on the second route, leaving
				// every module with no dependents and a change to a shared module
				// reaching nothing.
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

	changed, global := planner.Attribute(ws.changes, ws.moduleDirs, ws.srcs, ws.m.GlobalPaths)
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
				entries = []planner.Entry{planner.ModuleEntry(dir, ws.names[dir])}
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
//
// Checks are workspace artifacts on Dagger v1.0.0-beta.15: Module.checks is gone,
// and what lists a module's checks is Workspace.artifacts over a workspace that has
// that module installed. Three things about how that is called are load-bearing,
// and each one fails open — an empty list, no error — if it is got wrong:
//
//   - The module is installed with withModule. Artifacts come only from a
//     workspace's installed modules, so a workspace merely rooted at, or with its
//     current directory in, the module lists nothing at all.
//   - The workspace is made from the repository Directory, never the one a CLI
//     filled in: withModule on that refuses a workspace still configured by
//     dagger.json.
//   - The listing is scoped to the module's own name. A migrated workspace's
//     dagger.toml installs modules of its own, whose checks would otherwise be
//     listed as this module's.
//
// Each check's path is the installed module's name and then the check's own,
// which is the <module-name>:<check> pattern `dagger check` selects it by.
func (ws *workspace) moduleLegs(ctx context.Context, dir string) ([]planner.Entry, error) {
	name := ws.names[dir]
	if name == "" {
		return nil, fmt.Errorf("the module's name was never read")
	}
	items, err := ws.dir.AsWorkspace().
		WithModule(dir).
		Artifacts(dagger.WorkspaceArtifactsOpts{Include: []string{name}}).
		FilterTypes([]string{"Check"}).
		Items(ctx)
	if err != nil {
		return nil, fmt.Errorf("list checks: %w", err)
	}
	out := make([]planner.Entry, 0, len(items))
	for i := range items {
		item := items[i]
		if loadErr, err := item.LoadError(ctx); err != nil {
			return nil, fmt.Errorf("read a check's load error: %w", err)
		} else if loadErr != "" {
			return nil, fmt.Errorf("load: %s", loadErr)
		}
		p, err := item.Path(ctx)
		if err != nil {
			return nil, fmt.Errorf("read a check's path: %w", err)
		}
		if len(p) < 2 || p[0] != name {
			return nil, fmt.Errorf("check path %q is not under module %q", p, name)
		}
		out = append(out, planner.CheckEntry(dir, name, strings.Join(p[1:], ":")))
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
			set, err := ws.contextFiles(ctx, dir)
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

// contextFiles lists the repo-relative file paths that make up the source of the
// module rooted at dir: its config file, the subtree its config points its source
// at, and whatever its include patterns add, less whatever they take away. That
// set is what decides whether a changed path is an input to a module at all.
//
// It is the engine's own answer, read off ModuleSource.contextDirectory. That was
// not always safe to trust: before Dagger v1.0.0-beta.15, a module resolved from a
// Directory reported the whole Directory as its context, and taken at face value
// the root module would then have owned every path in the repository. The tests
// pin the filtered answer, so a return to the old one fails there rather than
// quietly making every change global.
//
// Scoping to the module also keeps a module with dependencies from claiming its
// dependencies' files as its own inputs; the dependency closure is what propagates
// those.
func (ws *workspace) contextFiles(ctx context.Context, dir string) (map[string]bool, error) {
	paths, err := ws.sources[dir].ContextDirectory().Glob(ctx, "**")
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		if strings.HasSuffix(p, "/") {
			continue // directory entry
		}
		set[strings.TrimPrefix(p, "/")] = true
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

func uniqueSuffix() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
