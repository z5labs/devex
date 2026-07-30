// A change-aware, memoized CI planner for a workspace of Dagger modules.
//
// Anyone maintaining a repository of modules ends up writing the same CI by
// hand: enumerate the checks, work out which ones a change could affect, route
// each to the module that owns it, and avoid re-running what a previous run
// already proved good. This module is that engine. It reads the workspace it is
// invoked from, diffs a commit range, and returns the checks to run — already
// routed, with timeouts and memoization hashes applied — so a CI system needs one
// call and at most a format shim.
//
// Nothing here loads a module the plan does not need. Checks are enumerated per
// module (Module.checks), never through a root aggregator that installs every
// suite as a toolchain, and the run-everything path emits one leg per module so
// it loads none at all. See README.md for what counts as a change, what is never
// memoized, and how base-image drift is bounded.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"dagger/workspace-ci/gitdiff"
	"dagger/workspace-ci/internal/dagger"
	"dagger/workspace-ci/planner"
)

// WorkspaceCi plans CI for the workspace it is invoked from.
type WorkspaceCi struct {
	// +private
	GlobalPaths []string
	// +private
	SplitModules []string
	// +private
	Timeouts string
	// +private
	DefaultTimeout int
	// +private
	MemoToken *dagger.Secret
	// +private
	MemoRepo string
	// +private
	MemoRefs []string
	// +private
	MemoTTL int
}

// New configures a planner.
func New(
	// Repo-relative path prefixes that govern how CI runs rather than what any
	// check computes; a change to one runs everything. They belong to no module's
	// source context, so nothing else would attribute them. Defaults to
	// .github/workflows/, which costs nothing in a workspace that has none.
	//
	// +optional
	globalPaths []string,
	// Repo-relative directories of modules whose checks must each get their own leg
	// even when everything runs. The run-everything path otherwise emits one leg per
	// module, which is right when a module's checks share their containers and wrong
	// when each one boots a stack of its own: those land in a single engine on a
	// single runner. Splitting a module costs loading it — the one thing that path
	// exists to avoid — so name only the modules that need it.
	//
	// +optional
	splitModules []string,
	// Per-leg check-step budgets in minutes, as a JSON object keyed by a leg's
	// display name or by a module directory (which covers every leg of that
	// module). It is JSON because Dagger function parameters cannot be Go maps.
	//
	// +optional
	// +default="{}"
	timeouts string,
	// The check-step budget in minutes for a leg with no override.
	//
	// +optional
	// +default=6
	defaultTimeout int,
	// A credential for reading the memoization store: a GitHub token with
	// actions:read on memoRepo. Nothing is ever written from here — see README.md.
	//
	// +optional
	memoToken *dagger.Secret,
	// The owner/name whose Actions cache holds the memoization store.
	//
	// +optional
	memoRepo string,
	// The git refs whose cache scopes may be trusted to hold recorded passes.
	// Defaults to none, which reads nothing: a scope a run can write is a scope
	// that must be chosen deliberately.
	//
	// +optional
	memoRefs []string,
	// How long, in seconds, a recorded pass may be honoured. This is the answer to
	// base-image drift, which a source-derived hash cannot see.
	//
	// +optional
	// +default=86400
	memoTTL int,
) (*WorkspaceCi, error) {
	if _, err := planner.ParseTimeouts(timeouts); err != nil {
		return nil, err
	}
	if memoToken != nil && memoRepo == "" {
		return nil, fmt.Errorf("memoToken needs memoRepo: a token with no repository to read cannot be scoped, and silently reading nothing would look like a workspace with no recorded passes")
	}
	if len(globalPaths) == 0 {
		globalPaths = planner.GlobalPathsDefault()
	}
	return &WorkspaceCi{
		GlobalPaths:    globalPaths,
		SplitModules:   splitModules,
		Timeouts:       timeouts,
		DefaultTimeout: defaultTimeout,
		MemoToken:      memoToken,
		MemoRepo:       memoRepo,
		MemoRefs:       memoRefs,
		MemoTTL:        memoTTL,
	}, nil
}

// Format is how a plan is serialized.
//
// Note on rendered names: the Dagger Go SDK derives each GraphQL enum member from
// the *constant identifier* in SCREAMING_SNAKE_CASE, and the CLI takes that member
// name rather than the value — hence `--format=GITHUB_ACTIONS`. The values here are
// spelled to match, so there is only ever one spelling to remember.
type Format string

const (
	// FormatJSON is the canonical form: an indented JSON array of legs.
	FormatJSON Format = "JSON"
	// FormatGithubActions is a single-line JSON array, ready to write to
	// GITHUB_OUTPUT and expand with fromJSON as a matrix.
	FormatGithubActions Format = "GITHUB_ACTIONS"
)

// Plan returns the legs of CI to run for a change, each already routed to the
// module that owns it and bounded by a timeout.
//
// Each leg is a {name, module, filter, hash, timeout, jobTimeout} object: the
// display name, the repo-relative module to invoke with `-m`, the check pattern to
// pass to `dagger check` (empty to run every check the module has), the input hash
// a pass may be recorded under (empty means never memoize), and the step and job
// budgets in minutes.
//
// base and head are the commit SHAs to diff, three-dot (merge-base) like a PR's
// change set. An empty or all-zeros base — a new branch, a missing base — means
// "run everything".
//
// A plan that cannot read the workspace is an error, never an empty plan: an empty
// matrix skips the run job and passes the gate having run nothing. Everything else
// fails safe towards running too much — an unusable diff range, an unreadable
// source context, a module whose checks cannot be enumerated.
//
// repo defaults to the calling workspace and is where everything is read from:
// module discovery is a dagger.json walk, source contexts and check enumeration
// work off the exported tree, and the change set comes from its .git. Passing it
// explicitly is also the escape hatch for a caller whose .git is a file rather
// than a directory (a git worktree), which would otherwise degrade to running
// everything.
//
// +cache="never"
func (m *WorkspaceCi) Plan(
	ctx context.Context,
	// The commit the change is measured from.
	base string,
	// The commit the change is measured to.
	head string,
	// +optional
	// +default="JSON"
	format Format,
	// The repository to plan for. Defaults to the calling workspace.
	//
	// +optional
	repo *dagger.Directory,
	// The workspace to read repo from when repo is omitted. Defaults to the
	// caller's.
	//
	// +optional
	workspace *dagger.Workspace,
	// Input hashes a previous run already proved good, as a JSON array. They are
	// honoured on the same terms as the ones read from the memoization store, and
	// are how a CI system that reads its own store — or a test — supplies them
	// without one. Anything unparseable is treated as empty: a store that cannot be
	// read must cost speed, never correctness.
	//
	// +optional
	// +default="[]"
	knownGood string,
	// Emit a diagnostics object — the plan plus which modules had to be loaded to
	// produce it, whether everything was selected, which legs a recorded pass
	// retired, and whether recorded passes were honoured at all — instead of the
	// bare plan. Intended for tests and for explaining a plan, not for CI.
	//
	// +optional
	diagnostics bool,
) (string, error) {
	result, err := m.plan(ctx, base, head, repo, workspace, knownGood)
	if err != nil {
		return "", err
	}
	if diagnostics {
		out, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return "", err
		}
		return string(out), nil
	}
	return planner.Render(result.Plan, planner.Format(format))
}

// AffectedModules returns, as a JSON array of repo-relative directories, the
// modules whose checks a change could affect. It is the same attribution Plan
// applies, stopping before any module is loaded, and answers "what did this change
// reach" without paying for check enumeration.
//
// The arguments mean what they mean on Plan.
//
// +cache="never"
func (m *WorkspaceCi) AffectedModules(
	ctx context.Context,
	// The commit the change is measured from.
	base string,
	// The commit the change is measured to.
	head string,
	// The repository to plan for. Defaults to the calling workspace.
	//
	// +optional
	repo *dagger.Directory,
	// The workspace to read repo from when repo is omitted. Defaults to the
	// caller's.
	//
	// +optional
	workspace *dagger.Workspace,
) (string, error) {
	ws, err := m.load(ctx, repo, workspace)
	if err != nil {
		return "", err
	}
	defer ws.cleanup()

	affected, _ := ws.affected(ctx, base, head)
	out, err := json.Marshal(affected)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// planReport is what Plan reports when asked to explain itself.
//
// It is unexported on purpose: an exported struct in package main is a Dagger
// object, and this one carries a field of a type from another package, which no
// consumer could generate bindings for.
type planReport struct {
	// Plan is the plan Plan would have rendered.
	Plan []planner.Entry `json:"plan"`
	// AffectedModules is the module set the change reached.
	AffectedModules []string `json:"affectedModules"`
	// LoadedModules is every module the planner had to load to enumerate checks.
	// A narrow change must load nothing outside its own affected set, and the
	// run-everything path must load nothing at all.
	LoadedModules []string `json:"loadedModules"`
	// Full reports that everything was selected.
	Full bool `json:"full"`
	// Memoized are the legs a recorded pass retired.
	Memoized []planner.Entry `json:"memoized"`
	// MemoTrusted reports whether recorded passes were honoured at all.
	MemoTrusted bool `json:"memoTrusted"`
	// KnownGood is how many recorded passes were read from the store.
	KnownGood int `json:"knownGood"`
}

// plan does the work behind Plan, in one place so the diagnostics and the plan
// cannot drift.
func (m *WorkspaceCi) plan(
	ctx context.Context,
	base, head string,
	repo *dagger.Directory,
	workspace *dagger.Workspace,
	knownGood string,
) (*planReport, error) {
	timeouts, err := planner.ParseTimeouts(m.Timeouts)
	if err != nil {
		return nil, err
	}
	ws, err := m.load(ctx, repo, workspace)
	if err != nil {
		return nil, err
	}
	defer ws.cleanup()

	affected, full := ws.affected(ctx, base, head)
	out := &planReport{AffectedModules: affected, Full: full, MemoTrusted: true}

	if full {
		// One leg per module rather than one per check: the plan never loads a
		// module, and fewer, coarser legs mean fewer simultaneous engine boots. The
		// modules the caller named as splits are the exception, and pay a load each.
		coarse, split := ws.partitionSplits(affected)
		for _, dir := range coarse {
			out.Plan = append(out.Plan, planner.ModuleEntry(dir))
		}
		out.Plan = append(out.Plan, ws.legs(ctx, split)...)
	} else {
		out.Plan = ws.legs(ctx, affected)
	}
	out.LoadedModules = ws.loaded

	// Hashing needs the source context of every module in every affected module's
	// closure, plus the root module's closure, which is wider than attribution
	// needed.
	ws.resolveSources(ctx, ws.hashingNeeds(affected))
	out.Plan, out.Memoized, out.MemoTrusted, out.KnownGood, err = m.memoize(ctx, ws, out.Plan, knownGood)
	if err != nil {
		return nil, err
	}

	out.Plan = timeouts.Apply(out.Plan, m.DefaultTimeout)
	planner.Sort(out.Plan)
	return out, nil
}

// memoize stamps each leg with the input hash a pass may be recorded under and
// drops the legs a recorded pass already proved good.
//
// Every failure here is soft: an unreadable store, an unreadable HEAD, an
// unhashable module all shrink what can be skipped, which costs CI time and never
// correctness.
func (m *WorkspaceCi) memoize(
	ctx context.Context,
	ws *workspace,
	legs []planner.Entry,
	supplied string,
) (run, skipped []planner.Entry, trusted bool, knownGood int, err error) {
	hashed := ws.hash(legs)
	known, err := m.storedPasses(ctx)
	if err != nil {
		return nil, nil, false, 0, err
	}
	if known == nil {
		known = map[string]bool{}
	}
	for h := range parseKnownGood(supplied) {
		known[h] = true
	}
	if !planner.MemoTrusted(ws.changes, ws.moduleDirs, ws.srcs, ws.bindings, m.GlobalPaths, ws.nonGlobal()) {
		fmt.Fprintf(os.Stderr, "workspace-ci: a global input changed; ignoring recorded passes\n")
		return hashed, nil, false, len(known), nil
	}
	run, skipped = planner.MemoFilter(hashed, known)
	if len(skipped) > 0 {
		fmt.Fprintf(os.Stderr, "workspace-ci: %d leg(s) already passed on these inputs; skipping\n", len(skipped))
	}
	return run, skipped, true, len(known), nil
}

// SelectionSelfTest verifies the change -> modules -> legs mapping, and the
// properties a recorded pass depends on, against fixed fixtures — so a regression
// in either fails CI rather than silently under-running a consumer's checks. It
// runs in-process and needs no services, so it is cheap enough to run on every leg
// set.
//
// +check
func (m *WorkspaceCi) SelectionSelfTest(ctx context.Context) error {
	if err := planner.SelfCheck(); err != nil {
		return err
	}
	return planner.HashSelfCheck()
}

// parseKnownGood turns a JSON array of recorded input hashes into a set. An
// unparseable value yields the empty set, which memoizes nothing.
func parseKnownGood(raw string) map[string]bool {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var hashes []string
	if err := json.Unmarshal([]byte(raw), &hashes); err != nil {
		fmt.Fprintf(os.Stderr, "workspace-ci: cannot parse the supplied known-good hashes (%v); running every selected leg\n", err)
		return nil
	}
	set := make(map[string]bool, len(hashes))
	for _, h := range hashes {
		set[h] = true
	}
	return set
}

// changeSet reads the diff between base and head out of the repository, or reports
// that there is no usable one — in which case everything runs.
func changeSet(repoDir, base, head string) []planner.Change {
	b, h, ok := planner.DiffRange(base, head)
	if !ok {
		fmt.Fprintf(os.Stderr, "workspace-ci: no usable diff range (base=%q head=%q); running everything\n", base, head)
		return nil
	}
	changes, err := gitdiff.Changes(repoDir, b, h)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workspace-ci: git diff failed (%v); running everything\n", err)
		return nil
	}
	out := make([]planner.Change, 0, len(changes))
	for _, c := range changes {
		out = append(out, planner.Change{Path: c.Path, Deleted: c.Deleted})
	}
	return out
}
