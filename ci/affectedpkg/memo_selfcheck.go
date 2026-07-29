package affectedpkg

import (
	"fmt"
	"sort"
	"strings"
)

// hashTree is a synthetic worktree for the fixture graph: every module's Dagger
// source context, plus the global inputs, plus a git blob object id per path.
//
// It stands in for the real repository the way `fixture` stands in for the real
// dependency graph, so the memoization invariants can be asserted without an
// engine or a checkout. The object ids are opaque to InputHashes — all it ever
// does is compare them — so a synthetic id exercises the real code path.
type hashTree struct {
	srcs  map[string]map[string]bool
	blobs map[string]string
}

// workflowPath is the one global input that lives outside any module's source
// context. Its prefix is what globalPrefixes matches.
const workflowPath = ".github/workflows/ci.yml"

// hashTree builds the pristine tree: two source files per module (a dagger.json
// carrying engineVersion and a main.go), the root module's own context, one
// aggregator binding per toolchain, and the CI workflow. Each module also gets a
// README.md that is deliberately absent from its source context, so the "a
// non-input change hits every check" invariant has something to touch.
func (f fixture) hashTree() hashTree {
	t := hashTree{
		srcs:  map[string]map[string]bool{},
		blobs: map[string]string{},
	}

	for _, dir := range f.moduleDirs() {
		if dir == RootPkg {
			continue
		}
		t.addInputs(dir, dir+"/dagger.json", dir+"/main.go")
		t.blobs[dir+"/README.md"] = "blob:" + dir + "/README.md" // declared out
	}

	root := []string{
		"dagger.json",
		"ci/main.go",
		"ci/affectedpkg/affected.go",
		"ci/internal/dagger/dagger.gen.go",
	}
	for path := range AggregatorBindings(f.checkModule) {
		root = append(root, path)
	}
	t.addInputs(RootPkg, root...)

	t.blobs[workflowPath] = "blob:" + workflowPath
	return t
}

func (t hashTree) addInputs(dir string, paths ...string) {
	if t.srcs[dir] == nil {
		t.srcs[dir] = map[string]bool{}
	}
	for _, p := range paths {
		t.srcs[dir][p] = true
		t.blobs[p] = "blob:" + p
	}
}

// touch returns a copy of the tree in which every listed path has different
// content — a new object id — exactly as an edit to that file would produce.
func (t hashTree) touch(paths ...string) hashTree {
	next := hashTree{srcs: t.srcs, blobs: make(map[string]string, len(t.blobs))}
	for p, oid := range t.blobs {
		next.blobs[p] = oid
	}
	for _, p := range paths {
		next.blobs[p] = "blob:edited:" + p
	}
	return next
}

// withoutDeleted returns a copy of the tree's source contexts with every deleted
// path removed, which is the state a head context is actually in — nothing that
// no longer exists can be in it.
func (t hashTree) withoutDeleted(changes []Change) map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(t.srcs))
	for dir, set := range t.srcs {
		copied := make(map[string]bool, len(set))
		for p := range set {
			copied[p] = true
		}
		out[dir] = copied
	}
	for _, c := range changes {
		if !c.Deleted {
			continue
		}
		for _, set := range out {
			delete(set, c.Path)
		}
	}
	return out
}

// dropModule returns a copy of the tree with dir's source context unreadable,
// modelling a module whose ContextDirectory the engine could not glob.
func (t hashTree) dropModule(dir string) hashTree {
	next := hashTree{srcs: make(map[string]map[string]bool, len(t.srcs)), blobs: t.blobs}
	for d, set := range t.srcs {
		if d == dir {
			continue
		}
		next.srcs[d] = set
	}
	return next
}

// everyDaggerJSON is the shape of an engineVersion bump: the field lives in all
// ~50 dagger.json files, so every module's source context changes at once.
func (t hashTree) everyDaggerJSON() []string {
	var out []string
	for p := range t.blobs {
		if p == "dagger.json" || strings.HasSuffix(p, "/dagger.json") {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// memoCase asserts which checks a given edit must invalidate: after touching
// `touch`, exactly the checks in wantMiss must compute a different input hash
// than they did on the pristine tree, and every other check must still match.
type memoCase struct {
	name  string
	touch []string
	// engineBump touches every dagger.json instead, which is what bumping
	// engineVersion does; the paths are only known once a tree exists.
	engineBump bool
	// wantMiss is the exact set of checks whose hash must change. Empty means
	// every check still hits.
	wantMiss []string
	// wantAllMiss asserts no check may hit — the fail-safe direction.
	wantAllMiss bool
}

// cryptoDependents is every check that reaches daggerverse/crypto in the fixture
// graph, i.e. the blast radius memoization must reproduce exactly.
var cryptoDependents = []string{
	"certificate-management-tests:all",
	"crypto-tests:all",
	"kafka-tests:native",
	"skill-gen-tests:all",
	"postgres-tests:cluster",
	"envoy-tests:admin",
	"otel-tests:core",
}

func memoCases() []memoCase {
	return []memoCase{
		{
			// The whole point: an unchanged input closure reproduces the same
			// hash, so a recorded pass is reusable.
			name: "an untouched tree hits every check",
		},
		{
			name:     "a leaf module misses only its own suite",
			touch:    []string{"daggerverse/kicad/main.go"},
			wantMiss: []string{"kicad-tests:all"},
		},
		{
			name:     "a module's own tests dir misses only its own suite",
			touch:    []string{"daggerverse/kicad/tests/main.go"},
			wantMiss: []string{"kicad-tests:all"},
		},
		{
			// Any member of the closure, however deep, invalidates the check.
			name:     "a shared module misses every dependent",
			touch:    []string{"daggerverse/crypto/main.go"},
			wantMiss: cryptoDependents,
		},
		{
			// The reason the aggregator bindings are attributed to their
			// toolchain (#179) rather than folded into the global inputs: repo
			// convention regenerates one on almost every module change, and
			// folding it in globally would make memoization never hit.
			name:     "a per-toolchain aggregator binding misses only its own suite",
			touch:    []string{bindingDir + "kicad-tests" + bindingExt},
			wantMiss: []string{"kicad-tests:all"},
		},
		{
			name:        "an engine-version bump misses every check",
			engineBump:  true,
			wantAllMiss: true,
		},
		{
			// A workflow decides which checks run and how a pass is recorded, so
			// it must reach every hash.
			name:        "a CI workflow change misses every check",
			touch:       []string{workflowPath},
			wantAllMiss: true,
		},
		{
			name:        "the ci module's own source misses every check",
			touch:       []string{"ci/affectedpkg/affected.go"},
			wantAllMiss: true,
		},
		{
			// Not global, unlike every other path under ci/: dagger regenerates
			// it whenever the toolchain set changes, adding only an ID type and
			// a loader for the new toolchain. ci:generated is what keeps that
			// safe. See nonGlobalRootPaths.
			name:  "the ci module's core binding misses nothing",
			touch: []string{coreBinding},
		},
		{
			// A path in no module's source context is an input to nothing, so it
			// cannot invalidate a recorded pass any more than it can select a
			// check (#195).
			name:  "a module README, an input to nothing, misses nothing",
			touch: []string{"daggerverse/crypto/README.md"},
		},
		{
			name:  "adding a toolchain to the root dagger.json misses nothing",
			touch: []string{rootConfig},
		},
		{
			// The case nonGlobalRootPaths exists for: adding a daggerverse module
			// edits the toolchain entry and makes dagger regenerate the core
			// binding alongside it. Every pre-existing check must keep its hash.
			// (The third file such a PR adds, the new toolchain's own binding,
			// is reattributed by AggregatorBindings to a toolchain that has no
			// recorded pass yet — covered by the per-toolchain case above.)
			name:  "adding a daggerverse module misses no pre-existing check",
			touch: []string{rootConfig, coreBinding},
		},
		{
			// Those exclusions are of two paths' content, not of the root
			// context as a whole — every other path there still reaches every
			// hash.
			name:        "the exclusions do not spare the rest of ci/",
			touch:       []string{rootConfig, coreBinding, "ci/affectedpkg/affected.go"},
			wantAllMiss: true,
		},
	}
}

// memoSelfCheck asserts the input-hash invariants (#238) against the fixture
// graph and its synthetic worktree.
func memoSelfCheck() error {
	f := repoFixture()
	closure := BuildClosures(f.checkModule, f.adj)
	bindings := AggregatorBindings(f.checkModule)
	base := f.hashTree()

	recorded := InputHashes(closure, base.srcs, base.blobs, bindings)
	if len(recorded) == 0 {
		return fmt.Errorf("InputHashes produced no hashes for the fixture tree")
	}
	for name := range f.checkModule {
		_, hashed := recorded[name]
		if isCiCheck(name) == hashed {
			return fmt.Errorf("%s: hashed=%v, want %v (ci:* checks are never memoized)", name, hashed, !hashed)
		}
	}

	for _, tc := range memoCases() {
		touch := tc.touch
		if tc.engineBump {
			touch = base.everyDaggerJSON()
		}
		after := InputHashes(closure, base.srcs, base.touch(touch...).blobs, bindings)

		var miss []string
		for name, h := range recorded {
			next, ok := after[name]
			if !ok {
				return fmt.Errorf("%s: %s lost its hash", tc.name, name)
			}
			if next != h {
				miss = append(miss, name)
			}
		}
		if tc.wantAllMiss {
			if len(miss) != len(recorded) {
				return fmt.Errorf("%s: only %v missed, want every check", tc.name, sortedCopy(miss))
			}
			continue
		}
		if !sameSet(miss, tc.wantMiss) {
			return fmt.Errorf("%s: missed %v, want %v", tc.name, sortedCopy(miss), sortedCopy(tc.wantMiss))
		}
	}

	if err := memoFailSafeSelfCheck(f, closure, bindings, base); err != nil {
		return err
	}
	return memoTrustSelfCheck(f, base, bindings)
}

// memoFailSafeSelfCheck asserts that anything the hash cannot account for leaves
// the check unhashed, and that an unhashed check is never skipped.
func memoFailSafeSelfCheck(f fixture, closure map[string]map[string]bool, bindings map[string]string, base hashTree) error {
	// A module whose source context could not be read poisons every check that
	// depends on it, and only those.
	blind := InputHashes(closure, base.dropModule("daggerverse/crypto").srcs, base.blobs, bindings)
	for _, name := range cryptoDependents {
		if _, hashed := blind[name]; hashed {
			return fmt.Errorf("%s kept a hash despite an unreadable dependency source context", name)
		}
	}
	if _, hashed := blind["kicad-tests:all"]; !hashed {
		return fmt.Errorf("kicad-tests:all lost its hash to an unrelated module's unreadable source context")
	}

	// A source path with no object id at HEAD — untracked, or a dirty tree —
	// is equally unaccountable.
	dirty := base.touch()
	delete(dirty.blobs, "daggerverse/kicad/main.go")
	if h := InputHashes(closure, base.srcs, dirty.blobs, bindings); len(h) > 0 {
		if _, hashed := h["kicad-tests:all"]; hashed {
			return fmt.Errorf("kicad-tests:all kept a hash despite a source path missing from HEAD")
		}
	}

	// Unhashed checks — ci:* and the unresolvable — survive the filter even when
	// every recorded hash is offered as known-good.
	hashes := InputHashes(closure, base.srcs, base.blobs, bindings)
	knownGood := map[string]bool{}
	for _, h := range hashes {
		knownGood[h] = true
	}
	universe := f.universe()
	run, skipped := MemoFilter(universe, hashes, knownGood)
	for _, name := range run {
		if !isCiCheck(name) {
			return fmt.Errorf("%s ran despite a recorded pass on identical inputs", name)
		}
	}
	if len(run)+len(skipped) != len(universe) {
		return fmt.Errorf("MemoFilter dropped checks: %d run + %d skipped != %d", len(run), len(skipped), len(universe))
	}
	if got := len(skipped); got != len(hashes) {
		return fmt.Errorf("MemoFilter skipped %d checks, want %d", got, len(hashes))
	}
	return nil
}

// memoTrustSelfCheck asserts the trust boundary: a change that could alter how a
// pass is recorded must disable memoization outright, while a change that only
// alters which checks exist must not.
func memoTrustSelfCheck(f fixture, base hashTree, bindings map[string]string) error {
	dirs := f.moduleDirs()
	cases := []struct {
		name    string
		changes []Change
		want    bool
	}{
		{"an ordinary module change honours recorded passes", []Change{{Path: "daggerverse/kicad/main.go"}}, true},
		{"a ci/ change ignores recorded passes", []Change{{Path: "ci/main.go"}}, false},
		{"a workflow change ignores recorded passes", []Change{{Path: workflowPath}}, false},
		{"a deleted ci/ file ignores recorded passes", []Change{{Path: "ci/main.go", Deleted: true}}, false},
		{"an unusable diff still honours recorded passes", nil, true},
		// Adding a daggerverse module. Neither of nonGlobalRootPaths may retire
		// the store, alone or together, but neither may shield anything else.
		{"a root dagger.json change alone honours recorded passes", []Change{{Path: rootConfig}}, true},
		{"a core binding change alone honours recorded passes", []Change{{Path: coreBinding}}, true},
		{
			"adding a daggerverse module honours recorded passes",
			[]Change{
				{Path: rootConfig},
				{Path: coreBinding},
				{Path: bindingDir + "kicad-tests" + bindingExt},
				{Path: "daggerverse/kicad/main.go"},
			},
			true,
		},
		{
			"the exclusions alongside a ci/ change still ignore recorded passes",
			[]Change{{Path: rootConfig}, {Path: coreBinding}, {Path: "ci/main.go"}},
			false,
		},
		{
			"the exclusions alongside a workflow change still ignore recorded passes",
			[]Change{{Path: coreBinding}, {Path: workflowPath}},
			false,
		},
	}
	for _, tc := range cases {
		srcs := base.srcs
		if len(tc.changes) > 0 {
			// Deleted paths cannot appear in a head source context; model that
			// so the deletion case exercises Attribute's real behaviour.
			srcs = base.withoutDeleted(tc.changes)
		}
		if got := MemoTrusted(tc.changes, dirs, srcs, bindings); got != tc.want {
			return fmt.Errorf("%s: MemoTrusted = %v, want %v", tc.name, got, tc.want)
		}
	}
	return nil
}
