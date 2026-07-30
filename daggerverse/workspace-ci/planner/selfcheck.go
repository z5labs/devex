package planner

import (
	"errors"
	"fmt"
	"slices"
)

// SelfCheck exercises the change -> modules -> legs mapping against a fixed
// workspace fixture, so a regression in it fails a consumer's CI rather than
// silently under-running their checks. It is pure and needs no engine, no git and
// no services, which is what makes it cheap enough to run on every leg set.
//
// The fixture is deliberately shaped like a real daggerverse: a shared module two
// others depend on, a nested tests module, an unrelated module that must not be
// disturbed, a file declared out of its module's source context, and a root module
// that owns everything else.
func SelfCheck() error {
	var errs []error
	for _, c := range selfCheckCases() {
		if err := c.run(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.name, err))
		}
	}
	return errors.Join(errs...)
}

// fixture is the workspace SelfCheck reasons over.
type fixture struct {
	rootSource  string
	moduleDirs  []string
	adj         map[string][]string
	srcs        map[string]map[string]bool
	blobs       map[string]string
	globalPaths []string
}

const (
	fxShared = "mods/shared"
	fxApp    = "mods/app"
	fxTests  = "mods/app/tests"
	fxOther  = "mods/other"
)

func selfCheckFixture() fixture {
	fx := fixture{
		rootSource: "ci",
		moduleDirs: []string{RootModule, fxShared, fxApp, fxTests, fxOther},
		adj: map[string][]string{
			RootModule: {},
			fxShared:   {},
			fxApp:      {fxShared},
			fxTests:    {fxApp},
			fxOther:    {},
		},
		srcs:        map[string]map[string]bool{},
		blobs:       map[string]string{},
		globalPaths: GlobalPathsDefault(),
	}
	// Every module's context is its dagger.json plus its sources. Each module's
	// README is deliberately absent from it — declared out via dagger.json
	// "include" — which is what makes a prose change select nothing.
	for _, dir := range []string{fxShared, fxApp, fxTests, fxOther} {
		fx.srcs[dir] = pathSet(dir+"/dagger.json", dir+"/main.go")
	}
	fx.srcs[RootModule] = pathSet(
		"dagger.json",
		fx.rootSource+"/main.go",
		BindingDir(fx.rootSource)+"dagger"+bindingExt,
		BindingDir(fx.rootSource)+"app-tests"+bindingExt,
	)
	i := 0
	for _, set := range fx.srcs {
		for p := range set {
			i++
			fx.blobs[p] = fmt.Sprintf("%040x", i)
		}
	}
	fx.blobs[".github/workflows/ci.yml"] = fmt.Sprintf("%040x", 9999)
	return fx
}

func (fx fixture) bindings() map[string]string {
	return AggregatorBindings(fx.rootSource, map[string]string{"app-tests": fxTests})
}

func (fx fixture) nonGlobal() []string { return NonGlobalRootPaths(fx.rootSource) }

func (fx fixture) selectFor(changes []Change) (kept []string, full bool) {
	changed, global := Attribute(changes, fx.moduleDirs, fx.srcs, fx.bindings(), fx.globalPaths)
	return SelectModules(fx.moduleDirs, BuildClosures(fx.adj), changed, global)
}

type selfCheckCase struct {
	name    string
	changes []Change
	// want is the module set expected to run, or nil to expect every module.
	want []string
	// trusted is whether recorded passes may be honoured for this change set.
	trusted bool
	// dropAdj removes a module's dependency edges from the fixture, modelling a
	// module whose graph could not be resolved.
	dropAdj string
}

func (c selfCheckCase) run() error {
	fx := selfCheckFixture()
	if c.dropAdj != "" {
		delete(fx.adj, c.dropAdj)
	}
	kept, full := fx.selectFor(c.changes)
	want := c.want
	if want == nil {
		want = fx.moduleDirs
		if !full {
			return fmt.Errorf("expected every module to run, got a narrowed set %v", kept)
		}
	} else if full {
		return fmt.Errorf("expected only %v to run, got every module", want)
	}
	if !slices.Equal(kept, want) {
		return fmt.Errorf("selected %v, want %v", kept, want)
	}
	got := MemoTrusted(c.changes, fx.moduleDirs, fx.srcs, fx.bindings(), fx.globalPaths, fx.nonGlobal())
	if got != c.trusted {
		return fmt.Errorf("MemoTrusted = %v, want %v", got, c.trusted)
	}
	return nil
}

func selfCheckCases() []selfCheckCase {
	return []selfCheckCase{
		{
			name:    "a shared module reaches its dependents and nothing else",
			changes: []Change{{Path: fxShared + "/main.go"}},
			want:    []string{RootModule, fxShared, fxApp, fxTests},
			trusted: true,
		},
		{
			name:    "a nested tests module does not fan out to its parent's dependents",
			changes: []Change{{Path: fxTests + "/main.go"}},
			want:    []string{RootModule, fxTests},
			trusted: true,
		},
		{
			name:    "a path in no source context selects nothing",
			changes: []Change{{Path: fxOther + "/README.md"}},
			want:    []string{RootModule},
			trusted: true,
		},
		{
			name:    "a deleted path is attributed to its module",
			changes: []Change{{Path: fxOther + "/main.go", Deleted: true}},
			want:    []string{RootModule, fxOther},
			trusted: true,
		},
		{
			name:    "a deleted path outside any source context still counts",
			changes: []Change{{Path: fxOther + "/README.md", Deleted: true}},
			want:    []string{RootModule, fxOther},
			trusted: true,
		},
		{
			name:    "a global path runs everything and retires recorded passes",
			changes: []Change{{Path: ".github/workflows/ci.yml"}},
			trusted: false,
		},
		{
			name:    "the root module's own sources run everything and retire recorded passes",
			changes: []Change{{Path: "ci/main.go"}},
			trusted: false,
		},
		{
			name:    "an empty change set means no usable diff, so everything runs",
			changes: nil,
			trusted: true,
		},
		{
			name:    "the root dagger.json runs everything but keeps recorded passes",
			changes: []Change{{Path: "dagger.json"}},
			trusted: true,
		},
		{
			name:    "the core binding runs everything but keeps recorded passes",
			changes: []Change{{Path: BindingDir("ci") + "dagger" + bindingExt}},
			trusted: true,
		},
		{
			name:    "a per-toolchain binding is attributed to its toolchain",
			changes: []Change{{Path: BindingDir("ci") + "app-tests" + bindingExt}},
			want:    []string{RootModule, fxTests},
			trusted: true,
		},
		{
			name:    "a module whose dependency graph is unresolved always runs",
			changes: []Change{{Path: fxShared + "/main.go"}},
			want:    []string{RootModule, fxShared, fxApp, fxTests, fxOther},
			trusted: true,
			dropAdj: fxOther,
		},
	}
}

// HashSelfCheck verifies the properties a recorded pass depends on: a hash is
// stable across runs over identical inputs, moves when anything the check can read
// moves, and is refused outright when any part of the answer is missing.
func HashSelfCheck() error {
	fx := selfCheckFixture()
	closures := BuildClosures(fx.adj)
	rootClosure := closures[RootModule]

	newHasher := func(f fixture) (*Hasher, bool) {
		return NewHasher(rootClosure, f.srcs, f.blobs, f.bindings(), f.globalPaths, f.nonGlobal())
	}

	h, ok := newHasher(fx)
	if !ok {
		return errors.New("the fixture's global inputs are unhashable")
	}
	base, ok := h.Check(fxTests+":all", closures[fxTests])
	if !ok {
		return errors.New("the fixture's tests module is unhashable")
	}

	again, _ := newHasher(fx)
	stable, _ := again.Check(fxTests+":all", closures[fxTests])
	if stable != base {
		return fmt.Errorf("the same inputs hashed to %s then %s", base, stable)
	}

	other, _ := h.Check(fxOther+":all", closures[fxOther])
	if other == base {
		return errors.New("two different checks share a hash")
	}

	// A change to a transitive dependency must move the hash: that is the whole
	// point of hashing the closure rather than the module.
	moved := selfCheckFixture()
	moved.blobs[fxShared+"/main.go"] = fmt.Sprintf("%040x", 12345)
	mh, _ := newHasher(moved)
	if got, _ := mh.Check(fxTests+":all", closures[fxTests]); got == base {
		return errors.New("a change to a transitive dependency did not move the hash")
	}

	// A change to a global input must move every hash, including a check whose own
	// closure never reaches it.
	globalMoved := selfCheckFixture()
	globalMoved.blobs[".github/workflows/ci.yml"] = fmt.Sprintf("%040x", 54321)
	gh, _ := newHasher(globalMoved)
	if got, _ := gh.Check(fxTests+":all", closures[fxTests]); got == base {
		return errors.New("a change to a global input did not move the hash")
	}

	// An unreadable source context makes that module's checks unhashable, and so
	// unmemoizable, rather than hashable on partial inputs.
	blind := selfCheckFixture()
	delete(blind.srcs, fxShared)
	bh, ok := newHasher(blind)
	if !ok {
		return errors.New("an unreadable non-root context should not sink the global hash")
	}
	if _, ok := bh.Check(fxTests+":all", closures[fxTests]); ok {
		return errors.New("a check whose dependency's context is unreadable was still hashed")
	}

	// An untracked file in a module's context has no object id, which is the local
	// dirty-tree case; it must be unhashable rather than hashed as absent.
	untracked := selfCheckFixture()
	delete(untracked.blobs, fxShared+"/main.go")
	uh, _ := newHasher(untracked)
	if _, ok := uh.Check(fxTests+":all", closures[fxTests]); ok {
		return errors.New("a check with an untracked source file was still hashed")
	}

	// And with the root closure unreadable, nothing may be memoized at all.
	noRoot := selfCheckFixture()
	delete(noRoot.srcs, RootModule)
	if _, ok := newHasher(noRoot); ok {
		return errors.New("an unreadable root context still produced a global hash")
	}
	return nil
}

func pathSet(paths ...string) map[string]bool {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	return set
}
