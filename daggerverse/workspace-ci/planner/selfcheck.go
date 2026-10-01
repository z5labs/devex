package planner

import (
	"errors"
	"fmt"
	"slices"
	"strings"
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
	if err := unsweptSelfCheck(); err != nil {
		errs = append(errs, fmt.Errorf("unswept generated files: %w", err))
	}
	return errors.Join(errs...)
}

// unsweptSelfCheck pins the rule that decides whether a freshness sweep looked at
// every module: a committed generated file counts as swept only when its source
// directory is exactly a swept module's. Each case is a way the answer could come
// back green having missed a module.
func unsweptSelfCheck() error {
	generated := []string{
		"dagger.gen.go",
		"internal/dagger/dagger.gen.go",
		"ci/dagger.gen.go",
		"ci/internal/dagger/dagger.gen.go",
		"ci/internal/dagger/workspace-ci.gen.go",
		fxApp + "/dagger.gen.go",
		fxApp + "/internal/dagger/dagger.gen.go",
		fxTests + "/dagger.gen.go",
		fxTests + "/internal/dagger/app.gen.go",
	}
	var errs []error
	for _, tc := range []struct {
		name  string
		swept []string
		want  []string
	}{
		{
			name:  "every module swept",
			swept: []string{".", "ci", fxApp, fxTests},
			want:  nil,
		},
		{
			// The root module owns every path by prefix, and a parent's context
			// may hold a nested module's files; neither may vouch for an unswept
			// module.
			name:  "a nested module nobody swept",
			swept: []string{".", "ci", fxApp},
			want:  []string{fxTests + "/dagger.gen.go", fxTests + "/internal/dagger/app.gen.go"},
		},
		{
			name:  "a root module whose source is a subdirectory",
			swept: []string{".", fxApp, fxTests},
			want:  []string{"ci/dagger.gen.go", "ci/internal/dagger/dagger.gen.go", "ci/internal/dagger/workspace-ci.gen.go"},
		},
		{
			name:  "source subpaths spelled with a leading slash or as empty",
			swept: []string{"", "/ci", "/" + fxApp + "/", fxTests},
			want:  nil,
		},
	} {
		if got := Unswept(generated, tc.swept); !slices.Equal(got, tc.want) {
			errs = append(errs, fmt.Errorf("%s: unswept %q, want %q", tc.name, got, tc.want))
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
		BindingDir(fx.rootSource)+"shared"+bindingExt,
	)
	i := 0
	for _, set := range fx.srcs {
		for p := range set {
			i++
			fx.blobs[p] = fmt.Sprintf("%040x", i)
		}
	}
	fx.blobs[".github/workflows/ci.yml"] = fmt.Sprintf("%040x", 9999)
	fx.blobs["dagger.toml"] = fmt.Sprintf("%040x", 9998)
	fx.blobs["dagger.lock"] = fmt.Sprintf("%040x", 9997)
	return fx
}

func (fx fixture) nonGlobal() []string { return NonGlobalRootPaths(fx.rootSource) }

func (fx fixture) selectFor(changes []Change) (kept []string, full bool) {
	changed, global := Attribute(changes, fx.moduleDirs, fx.srcs, fx.globalPaths)
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
	got := MemoTrusted(c.changes, fx.moduleDirs, fx.srcs, fx.globalPaths, fx.nonGlobal())
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
			// A dependency binding under the root module is the root module's own
			// source, whichever module it was generated from: v1 generates no
			// per-toolchain aggregator bindings for it to be attributed to.
			name:    "a dependency binding under the root module runs everything",
			changes: []Change{{Path: BindingDir("ci") + "shared" + bindingExt}},
			trusted: false,
		},
		{
			name:    "a migrated workspace's dagger.toml runs everything and retires recorded passes",
			changes: []Change{{Path: "dagger.toml"}},
			trusted: false,
		},
		{
			name:    "a migrated workspace's dagger.lock runs everything and retires recorded passes",
			changes: []Change{{Path: "dagger.lock"}},
			trusted: false,
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
		return NewHasher(rootClosure, f.srcs, f.blobs, f.globalPaths, f.nonGlobal())
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

// jenkinsGolden is the Jenkins form of the two-leg plan RenderSelfCheck renders:
// one leg that runs a single check and one that runs a whole module. It is written
// out in full rather than asserted piecewise because the shape *is* the contract —
// a consumer hands it to `parallel` unread, so a stray brace or a mis-escaped quote
// is a pipeline that will not parse, and nothing between here and Jenkins would
// notice.
const jenkinsGolden = `[
  'mods/app/tests:all': {
    stage('mods/app/tests:all') {
      timeout(time: 6, unit: 'MINUTES') {
        sh 'dagger -m \'mods/app/tests\' check --module \'tests\' \'tests:all\''
      }
    }
  },
  'mods/other': {
    stage('mods/other') {
      timeout(time: 6, unit: 'MINUTES') {
        sh 'links=$(dagger -m \'mods/other\' check --module \'other\' -l -f link) || exit 1; if [ -z "$links" ]; then echo \'mods/other declares no checks\'; else dagger -m \'mods/other\' check --module \'other\'; fi'
      }
    }
  }
]
`

// selfCheckRecordCommand stands in for what a Jenkinsfile passes as
// --record-command: a `record-pass` call complete but for the hash, whose ref and
// commit come from the pipeline's own environment.
const selfCheckRecordCommand = `dagger -m workspace-ci --memo-store=GIT_REFS call record-pass --ref="$GIT_REF" --commit="$GIT_COMMIT"`

// jenkinsRecordGolden is the Jenkins form of a two-leg plan rendered with
// selfCheckRecordCommand, where only the first leg carries a hash.
//
// Three things it pins, each of which is a way memoization here goes wrong
// silently: the recording runs *after* the check, so a check that throws skips it;
// it sits *outside* the timeout, so a check that uses its whole budget does not
// take the recording down with it; and the leg with no hash — the plan's way of
// saying never memoize this — renders no recording at all.
const jenkinsRecordGolden = `[
  'mods/app/tests:all': {
    stage('mods/app/tests:all') {
      timeout(time: 6, unit: 'MINUTES') {
        sh 'dagger -m \'mods/app/tests\' check --module \'tests\' \'tests:all\''
      }
      sh 'dagger -m workspace-ci --memo-store=GIT_REFS call record-pass --ref="$GIT_REF" --commit="$GIT_COMMIT" --hash=\'abc123\''
    }
  },
  'mods/other': {
    stage('mods/other') {
      timeout(time: 6, unit: 'MINUTES') {
        sh 'links=$(dagger -m \'mods/other\' check --module \'other\' -l -f link) || exit 1; if [ -z "$links" ]; then echo \'mods/other declares no checks\'; else dagger -m \'mods/other\' check --module \'other\'; fi'
      }
    }
  }
]
`

// withHash is the plan's way of saying a leg may be memoized under hash.
func withHash(e Entry, hash string) Entry {
	e.Hash = hash
	return e
}

// RenderSelfCheck pins what each format emits for a known plan, so a regression in
// one fails CI rather than a consumer's pipeline. Like the other self-checks it is
// pure: no engine, no git, no services.
//
// The empty plan is checked for every format because each one has a different
// wrong answer that survives a superficial test — `null` breaks fromJSON after
// passing a workflow's non-empty test, and `[]` is an empty List that Jenkins'
// parallel step rejects where an empty Map is a no-op.
func RenderSelfCheck() error {
	var errs []error
	fail := func(format string, err error) { errs = append(errs, fmt.Errorf("%s: %w", format, err)) }

	for _, tc := range []struct {
		format Format
		want   string
	}{
		{FormatJSON, "[]"},
		{FormatGithubActions, "[]"},
		{FormatJenkins, jenkinsPreamble + "[:]\n"},
	} {
		got, err := Render(nil, tc.format, "")
		if err != nil {
			fail(string(tc.format), err)
			continue
		}
		if got != tc.want {
			fail(string(tc.format), fmt.Errorf("an empty plan rendered as %q, want %q", got, tc.want))
		}
	}

	legs := Timeouts{}.Apply([]Entry{
		CheckEntry(fxTests, "tests", "all"),
		ModuleEntry(fxOther, "other"),
	}, 6)
	got, err := Render(legs, FormatJenkins, "")
	if err != nil {
		fail("JENKINS", err)
	} else if want := jenkinsPreamble + jenkinsGolden; got != want {
		fail("JENKINS", fmt.Errorf("rendered\n%s\nwant\n%s", got, want))
	}

	// The same two legs with a record command, one of them memoizable and one not.
	// Pinned in full for the same reason as the plain form: this one is what a
	// pipeline runs after a green check, so a stray brace here writes to the store
	// or fails a branch that passed.
	recorded := Timeouts{}.Apply([]Entry{
		withHash(CheckEntry(fxTests, "tests", "all"), "abc123"),
		ModuleEntry(fxOther, "other"),
	}, 6)
	got, err = Render(recorded, FormatJenkins, selfCheckRecordCommand)
	if err != nil {
		fail("JENKINS", err)
	} else if want := jenkinsPreamble + jenkinsRecordGolden; got != want {
		fail("JENKINS", fmt.Errorf("with a record command, rendered\n%s\nwant\n%s", got, want))
	}

	// A record command reaches no other format, because no other format renders
	// behaviour to attach it to. Silently dropping it would leave a caller
	// believing they had configured recording.
	for _, f := range []Format{FormatJSON, FormatGithubActions, ""} {
		if _, err := Render(recorded, f, selfCheckRecordCommand); err == nil {
			fail(string(f), errors.New("a record command was accepted by a format that cannot render one"))
		}
	}

	// A leg whose budget was never applied must render without a timeout at all:
	// `timeout(time: 0)` aborts the branch the instant it starts, which would look
	// like every check failing.
	unbounded, err := Render([]Entry{CheckEntry(fxOther, "other", "ok")}, FormatJenkins, "")
	if err != nil {
		fail("JENKINS", err)
	} else if strings.Contains(unbounded, "timeout(") {
		fail("JENKINS", errors.New("a leg with no budget still rendered a timeout"))
	}

	// A leg for a module the workspace installs has no module directory to load
	// with -m, and `-m .` fails outright in a workspace with no root module. Its
	// command must select by --module from the workspace root, and list first like
	// any other leg that runs a whole module.
	sdkCmd := checkCommand(InstalledEntry("dagger-go-sdk"))
	if want := `links=$(dagger check --module 'dagger-go-sdk' -l -f link) || exit 1; if [ -z "$links" ]; then echo 'dagger-go-sdk (workspace) declares no checks'; else dagger check --module 'dagger-go-sdk'; fi`; sdkCmd != want {
		fail("JENKINS", fmt.Errorf("an installed module's leg runs\n%s\nwant\n%s", sdkCmd, want))
	}

	// The two escapers, which is where a leg name carrying a quote or a backslash
	// stops being data and starts being syntax.
	for _, tc := range []struct{ name, got, want string }{
		{"groovyString quote", groovyString(`a'b`), `'a\'b'`},
		{"groovyString backslash", groovyString(`a\b`), `'a\\b'`},
		{"groovyString newline", groovyString("a\nb"), `'a\nb'`},
		{"groovyString dollar", groovyString(`a$b`), `'a$b'`},
		{"shellQuote quote", shellQuote(`a'b`), `'a'\''b'`},
		{"shellQuote plain", shellQuote(`a b`), `'a b'`},
	} {
		if tc.got != tc.want {
			fail(tc.name, fmt.Errorf("got %s, want %s", tc.got, tc.want))
		}
	}

	if _, err := Render(legs, Format("yaml"), ""); err == nil {
		errs = append(errs, errors.New("an unknown format was accepted"))
	}
	return errors.Join(errs...)
}

func pathSet(paths ...string) map[string]bool {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	return set
}
