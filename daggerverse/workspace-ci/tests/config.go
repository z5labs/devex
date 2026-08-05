package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"dagger/tests/internal/dagger"
)

// PlanLoadsOnlyAffectedModules proves the performance property the whole design
// rests on, by counting rather than timing: producing a plan for a narrow change
// loads the affected modules and nothing else.
func (t *Tests) PlanLoadsOnlyAffectedModules(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	got, err := explain(ctx, dag.WorkspaceCi(), fx, cTouchA, "")
	if err != nil {
		return err
	}
	want := []string{fxRoot, fxA, fxB}
	slices.Sort(want)
	loaded := slices.Clone(got.LoadedModules)
	slices.Sort(loaded)
	if !slices.Equal(loaded, want) {
		return fmt.Errorf("loaded %v, want %v", loaded, want)
	}
	for _, unaffected := range []string{fxC, fxDirty, fxGlobal} {
		if slices.Contains(loaded, unaffected) {
			return fmt.Errorf("loaded %q, which the change could not reach", unaffected)
		}
	}
	return nil
}

// AffectedModulesReportsWhatChangeReached proves the attribution half of the plan
// is available on its own, for a caller that wants to know what a change reached
// without paying to enumerate checks.
func (t *Tests) AffectedModulesReportsWhatChangeReached(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	raw, err := dag.WorkspaceCi().AffectedModules(ctx, fx.before(cTouchA), fx.at(cTouchA), dagger.WorkspaceCiAffectedModulesOpts{
		Repo: fx.dir,
	})
	if err != nil {
		return err
	}
	var got []string
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		return fmt.Errorf("parse %q: %w", raw, err)
	}
	want := []string{fxRoot, fxA, fxB}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		return fmt.Errorf("reached %v, want %v", got, want)
	}
	return nil
}

// PlanEmitsGithubActionsMatrix proves the one non-canonical format carries the
// same legs on a single line, which is what a GITHUB_OUTPUT assignment and fromJSON
// need.
func (t *Tests) PlanEmitsGithubActionsMatrix(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	raw, err := dag.WorkspaceCi().Plan(ctx, fx.before(cTouchA), fx.at(cTouchA), dagger.WorkspaceCiPlanOpts{
		Repo:   fx.dir,
		Format: dagger.WorkspaceCiFormatGithubActions,
	})
	if err != nil {
		return err
	}
	if strings.Contains(strings.TrimSpace(raw), "\n") {
		return fmt.Errorf("the github-actions plan spans more than one line:\n%s", raw)
	}
	var legs []leg
	if err := json.Unmarshal([]byte(raw), &legs); err != nil {
		return fmt.Errorf("a github-actions matrix must be a JSON array: %w (%q)", err, raw)
	}
	if len(legs) == 0 {
		return fmt.Errorf("the github-actions matrix is empty: %q", raw)
	}
	for _, l := range legs {
		if l.Name == "" || l.Module == "" {
			return fmt.Errorf("matrix entry %+v is missing the fields a job needs", l)
		}
		if l.JobTimeout != l.Timeout+4 {
			return fmt.Errorf("matrix entry %+v has a job budget that is not its step budget plus headroom", l)
		}
	}
	return nil
}

// groovyImage is the runtime the Jenkins form is evaluated in. Jenkins pipelines
// are Groovy, so a real Groovy runtime is the closest thing to Jenkins that fits
// in a test.
const groovyImage = "groovy:4.0-jdk17"

// jenkinsProbe stands in for the parts of Jenkins the emitted branches call. It
// evaluates the plan, invokes each branch against a recorder bound as the
// closure's delegate — which is exactly how a pipeline supplies `stage`, `timeout`
// and `sh` — and reports what each one did.
//
// DELEGATE_ONLY is deliberate: a step the recorder does not implement is a failure
// here rather than something silently resolved against the script, so a branch can
// only be reported as working if every step it calls is one Jenkins provides.
const jenkinsProbe = `import groovy.json.JsonOutput

class Recorder {
    String stageName
    Integer timeoutMinutes
    String timeoutUnit
    List<String> commands = []

    def stage(String name, Closure body) {
        stageName = name
        run(body)
    }

    def timeout(Map args, Closure body) {
        timeoutMinutes = args.time as Integer
        timeoutUnit = args.unit as String
        run(body)
    }

    def sh(String cmd) { commands << cmd }

    private run(Closure body) {
        body.delegate = this
        body.resolveStrategy = Closure.DELEGATE_ONLY
        body()
    }
}

def plan = evaluate(new File('/w/plan.groovy'))
assert plan instanceof Map : "the plan is a ${plan.getClass()}, not a Map"

def out = [:]
plan.each { name, branch ->
    assert branch instanceof Closure : "${name} is a ${branch.getClass()}, not a Closure"
    def rec = new Recorder()
    branch.delegate = rec
    branch.resolveStrategy = Closure.DELEGATE_ONLY
    branch()
    out[name] = [stage: rec.stageName, timeout: rec.timeoutMinutes, unit: rec.timeoutUnit, commands: rec.commands]
}
println JsonOutput.toJson(out)
`

// branchReport is what jenkinsProbe saw one parallel branch do.
type branchReport struct {
	Stage    string   `json:"stage"`
	Timeout  int      `json:"timeout"`
	Unit     string   `json:"unit"`
	Commands []string `json:"commands"`
}

// PlanEmitsJenkinsParallelStages proves the Jenkins form is what a declarative
// pipeline's parallel step actually takes — a Map of branch name to Closure — and
// that running a branch runs that leg's checks under that leg's budget.
//
// It evaluates the output in a real Groovy runtime rather than matching it as
// text, because escaping is the half that breaks: a plan is handed to `parallel`
// unread, so a mis-escaped quote is a pipeline that does not parse, and nothing
// between the renderer and Jenkins would report it.
func (t *Tests) PlanEmitsJenkinsParallelStages(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	ci := dag.WorkspaceCi()
	base, head := fx.before(cTouchA), fx.at(cTouchA)
	raw, err := ci.Plan(ctx, base, head, dagger.WorkspaceCiPlanOpts{
		Repo:   fx.dir,
		Format: dagger.WorkspaceCiFormatJenkins,
	})
	if err != nil {
		return err
	}
	got, err := explainRange(ctx, ci, fx, base, head, "")
	if err != nil {
		return err
	}
	if len(got.Plan) == 0 {
		return fmt.Errorf("the fixture planned no legs, so there is nothing to render")
	}

	stdout, err := dag.Container().
		From(groovyImage).
		WithWorkdir("/w").
		WithMountedDirectory("/w", dag.Directory().
			WithNewFile("plan.groovy", raw).
			WithNewFile("probe.groovy", jenkinsProbe)).
		WithExec([]string{"groovy", "/w/probe.groovy"}).
		Stdout(ctx)
	if err != nil {
		return fmt.Errorf("the jenkins plan is not usable Groovy: %w\n%s", err, raw)
	}
	var branches map[string]branchReport
	if err := json.Unmarshal([]byte(stdout), &branches); err != nil {
		return fmt.Errorf("parse the probe's report %q: %w", stdout, err)
	}

	for _, l := range got.Plan {
		b, ok := branches[l.Name]
		if !ok {
			return fmt.Errorf("leg %q has no parallel branch; got %v", l.Name, slices.Sorted(maps.Keys(branches)))
		}
		if b.Stage != l.Name {
			return fmt.Errorf("branch %q opened a stage named %q", l.Name, b.Stage)
		}
		if b.Timeout != l.Timeout || b.Unit != "MINUTES" {
			return fmt.Errorf("branch %q ran under %d %s, want %d MINUTES", l.Name, b.Timeout, b.Unit, l.Timeout)
		}
		want := "dagger -m '" + l.Module + "' check"
		if l.Filter != "" {
			want += " '" + l.Filter + "'"
		}
		if !slices.Equal(b.Commands, []string{want}) {
			return fmt.Errorf("branch %q ran %q, want [%q]", l.Name, b.Commands, want)
		}
	}
	if len(branches) != len(got.Plan) {
		return fmt.Errorf("the plan has %d legs but rendered %d branches", len(got.Plan), len(branches))
	}
	return nil
}

// jenkinsRecordProbe is jenkinsProbe with the one thing that separates a branch
// that records from a branch that records *when it should*: it runs every branch
// twice, once where every step succeeds and once where the leg's check throws,
// which is what a Jenkins `sh` does on a non-zero exit.
//
// Failing the first `sh` is the whole model of a failed check — the check is the
// first step a branch runs — and it needs no knowledge of the command's text, so
// the probe cannot accidentally agree with the renderer about what a check looks
// like.
const jenkinsRecordProbe = `import groovy.json.JsonOutput

class Recorder {
    boolean failFirst = false
    List<String> commands = []

    def stage(String name, Closure body) { run(body) }

    def timeout(Map args, Closure body) { run(body) }

    def sh(String cmd) {
        commands << cmd
        if (failFirst && commands.size() == 1) {
            throw new RuntimeException("check failed: ${cmd}")
        }
    }

    private run(Closure body) {
        body.delegate = this
        body.resolveStrategy = Closure.DELEGATE_ONLY
        body()
    }
}

def plan = evaluate(new File('/w/plan.groovy'))
assert plan instanceof Map : "the plan is a ${plan.getClass()}, not a Map"

def out = [:]
plan.each { name, branch ->
    assert branch instanceof Closure : "${name} is a ${branch.getClass()}, not a Closure"
    def outcomes = [:]
    ['pass', 'fail'].each { mode ->
        def rec = new Recorder(failFirst: mode == 'fail')
        def threw = false
        branch.delegate = rec
        branch.resolveStrategy = Closure.DELEGATE_ONLY
        try {
            branch()
        } catch (Throwable t) {
            threw = true
        }
        outcomes[mode] = [commands: rec.commands, threw: threw]
    }
    out[name] = outcomes
}
println JsonOutput.toJson(out)
`

// branchOutcome is what jenkinsRecordProbe saw one branch do under one outcome.
type branchOutcome struct {
	Commands []string `json:"commands"`
	Threw    bool     `json:"threw"`
}

// jenkinsRecordCommand is what a Jenkinsfile passes as --record-command: a
// record-pass call complete but for the hash. It is never run here — the probe
// stands in for `sh` — so the ref and commit are the pipeline-side spellings a
// consumer would use rather than anything this fixture has.
const jenkinsRecordCommand = `dagger -m workspace-ci --memo-store=GIT_REFS call record-pass --ref="$GIT_REF" --commit="$GIT_COMMIT"`

// PlanRecordsPassesFromJenkinsBranches proves the memoization half of the Jenkins
// form: a branch records its own leg's hash when the leg passes, and records
// nothing when it fails.
//
// Both outcomes are asserted because only one of them is a property of what was
// rendered. That the recording runs on success is visible in the text; that it
// does *not* run on failure is a property of Groovy — `sh` throws, the closure
// unwinds, the step after it is never reached — and rearranging the render into
// something that still looks right would break it silently. So the plan is
// evaluated in a real Groovy runtime with a failing step, exactly as with a
// failing check.
//
// The leg the plan refuses to hash is the other half: a plan says "never memoize
// this" with an empty hash, and a branch that recorded one anyway would write an
// entry no later run could ever match — or, worse, one keyed on nothing.
func (t *Tests) PlanRecordsPassesFromJenkinsBranches(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	ci := dag.WorkspaceCi()
	base, head := fx.before(cTouchA), fx.at(cTouchA)
	raw, err := ci.Plan(ctx, base, head, dagger.WorkspaceCiPlanOpts{
		Repo:          fx.dir,
		Format:        dagger.WorkspaceCiFormatJenkins,
		RecordCommand: jenkinsRecordCommand,
	})
	if err != nil {
		return err
	}
	got, err := explainRange(ctx, ci, fx, base, head, "")
	if err != nil {
		return err
	}
	if len(got.Plan) == 0 {
		return fmt.Errorf("the fixture planned no legs, so there is nothing to render")
	}

	stdout, err := dag.Container().
		From(groovyImage).
		WithWorkdir("/w").
		WithMountedDirectory("/w", dag.Directory().
			WithNewFile("plan.groovy", raw).
			WithNewFile("probe.groovy", jenkinsRecordProbe)).
		WithExec([]string{"groovy", "/w/probe.groovy"}).
		Stdout(ctx)
	if err != nil {
		return fmt.Errorf("the jenkins plan is not usable Groovy: %w\n%s", err, raw)
	}
	var branches map[string]map[string]branchOutcome
	if err := json.Unmarshal([]byte(stdout), &branches); err != nil {
		return fmt.Errorf("parse the probe's report %q: %w", stdout, err)
	}

	var memoizable, unmemoizable int
	for _, l := range got.Plan {
		b, ok := branches[l.Name]
		if !ok {
			return fmt.Errorf("leg %q has no parallel branch; got %v", l.Name, slices.Sorted(maps.Keys(branches)))
		}
		check := "dagger -m '" + l.Module + "' check"
		if l.Filter != "" {
			check += " '" + l.Filter + "'"
		}
		wantPass := []string{check}
		if l.Hash != "" {
			memoizable++
			wantPass = append(wantPass, jenkinsRecordCommand+" --hash='"+l.Hash+"'")
		} else {
			unmemoizable++
		}
		if pass := b["pass"]; !slices.Equal(pass.Commands, wantPass) || pass.Threw {
			return fmt.Errorf("branch %q ran %q (threw=%v) when its check passed, want %q", l.Name, pass.Commands, pass.Threw, wantPass)
		}
		fail := b["fail"]
		if !slices.Equal(fail.Commands, []string{check}) {
			return fmt.Errorf("branch %q ran %q after its check failed, want only %q — a failed leg must record nothing", l.Name, fail.Commands, check)
		}
		if !fail.Threw {
			return fmt.Errorf("branch %q swallowed its check's failure, so the leg would report as passing", l.Name)
		}
	}
	// Neither half of the assertion above says anything if the plan happened to
	// contain only one kind of leg.
	if memoizable == 0 || unmemoizable == 0 {
		return fmt.Errorf("the fixture planned %d memoizable and %d unmemoizable legs; both are needed to tell a rendered recording from a missing one", memoizable, unmemoizable)
	}
	return nil
}

// PlanRefusesRecordCommandForDataFormats proves the option is not silently
// dropped by the formats that cannot render one. JSON and GITHUB_ACTIONS carry
// each leg's hash as data for a surrounding job to record; accepting a record
// command there would hand back a plan that records nothing, which a consumer
// would discover as a memoization store that never fills up.
func (t *Tests) PlanRefusesRecordCommandForDataFormats(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	for _, format := range []dagger.WorkspaceCiFormat{dagger.WorkspaceCiFormatJson, dagger.WorkspaceCiFormatGithubActions} {
		_, err := dag.WorkspaceCi().Plan(ctx, fx.before(cTouchA), fx.at(cTouchA), dagger.WorkspaceCiPlanOpts{
			Repo:          fx.dir,
			Format:        format,
			RecordCommand: jenkinsRecordCommand,
		})
		if err == nil {
			return fmt.Errorf("--format=%s accepted a record command it cannot render", format)
		}
		if !strings.Contains(err.Error(), "record command") {
			return fmt.Errorf("--format=%s failed for an unrelated reason: %w", format, err)
		}
	}
	return nil
}

// PlanAppliesTimeoutOverrides proves the timeout table: an override keyed by a
// leg's name beats one keyed by its module, both beat the default, and the job
// budget always follows the step budget.
func (t *Tests) PlanAppliesTimeoutOverrides(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	ci := dag.WorkspaceCi(dagger.WorkspaceCiOpts{
		Timeouts:       `{"mods/a:ok": 11, "mods/b": 9}`,
		DefaultTimeout: 7,
	})
	got, err := explain(ctx, ci, fx, cTouchA, "")
	if err != nil {
		return err
	}
	for name, want := range map[string]int{"mods/a:ok": 11, "mods/b:ok": 9, ".:root-ok": 7} {
		l, err := find(got, name)
		if err != nil {
			return err
		}
		if l.Timeout != want {
			return fmt.Errorf("leg %q has a step budget of %d minutes, want %d", name, l.Timeout, want)
		}
		if l.JobTimeout != want+4 {
			return fmt.Errorf("leg %q has a job budget of %d minutes, want %d", name, l.JobTimeout, want+4)
		}
	}
	return nil
}

// PlanSplitsNamedModulesOnTheRunEverythingPath proves the escape hatch for a
// module whose checks must not share a leg: named modules are enumerated even when
// everything runs, and every other module still gets one coarse leg and is still
// never loaded.
func (t *Tests) PlanSplitsNamedModulesOnTheRunEverythingPath(ctx context.Context) error {
	fx, err := newFixture(ctx, "")
	if err != nil {
		return err
	}
	ci := dag.WorkspaceCi(dagger.WorkspaceCiOpts{SplitModules: []string{fxA}})
	got, err := explain(ctx, ci, fx, cTouchFlow, "")
	if err != nil {
		return err
	}
	if !got.Full {
		return fmt.Errorf("a workflow change did not run everything: %v", names(got.Plan))
	}
	if err := wantLegs(got, fxRoot, fxGlobal, "mods/a:ok", fxB, fxC, fxDirty); err != nil {
		return err
	}
	split, err := find(got, "mods/a:ok")
	if err != nil {
		return err
	}
	if split.Filter != "a:ok" {
		return fmt.Errorf("split leg %+v does not carry its own check pattern", split)
	}
	coarse, err := find(got, fxB)
	if err != nil {
		return err
	}
	if coarse.Filter != "" {
		return fmt.Errorf("leg %q carries the filter %q; only the named modules are split", coarse.Name, coarse.Filter)
	}
	if !slices.Equal(got.LoadedModules, []string{fxA}) {
		return fmt.Errorf("loaded %v; splitting one module must load exactly that module", got.LoadedModules)
	}
	return nil
}

// NewRejectsMalformedTimeouts proves a timeout table that cannot be read is an
// error. A typo'd key already fails quietly — the default applies — so the one
// thing left to catch loudly is a table nothing could be read from.
func (t *Tests) NewRejectsMalformedTimeouts(ctx context.Context) error {
	_, err := dag.WorkspaceCi(dagger.WorkspaceCiOpts{Timeouts: `{"mods/a:ok": `}).
		Plan(ctx, "", "", dagger.WorkspaceCiPlanOpts{Repo: dag.Directory()})
	if err == nil {
		return fmt.Errorf("a malformed timeout table was accepted")
	}
	if !strings.Contains(err.Error(), "parse timeouts") {
		return fmt.Errorf("a malformed timeout table failed for the wrong reason: %v", err)
	}
	return nil
}

// NewRejectsMemoTokenWithoutRepo proves a credential with nothing to scope it is
// an error rather than a store that silently reads nothing — which would look
// exactly like a workspace with no recorded passes.
func (t *Tests) NewRejectsMemoTokenWithoutRepo(ctx context.Context) error {
	token, err := randomSecret()
	if err != nil {
		return err
	}
	_, err = dag.WorkspaceCi(dagger.WorkspaceCiOpts{MemoToken: token}).
		Plan(ctx, "", "", dagger.WorkspaceCiPlanOpts{Repo: dag.Directory()})
	if err == nil {
		return fmt.Errorf("a memo token with no repository was accepted")
	}
	if !strings.Contains(err.Error(), "memoRepo") {
		return fmt.Errorf("a memo token with no repository failed for the wrong reason: %v", err)
	}
	return nil
}

// SelectionSelfTestPasses runs the module's own check the way a consumer's CI
// would, so a regression in the pure selection and hashing rules fails here too
// rather than only where it is installed.
func (t *Tests) SelectionSelfTestPasses(ctx context.Context) error {
	return dag.WorkspaceCi().SelectionSelfTest(ctx)
}

// randomSecret mints a throwaway credential at run time, so no test ever carries
// a literal one.
func randomSecret() (*dagger.Secret, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	return dag.SetSecret("workspace-ci-tests-memo-token", hex.EncodeToString(b[:])), nil
}
