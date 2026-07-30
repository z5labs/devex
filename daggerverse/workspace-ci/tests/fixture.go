package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"dagger/tests/internal/dagger"
)

// A fixture is a synthetic repository the planner is pointed at, shaped like a
// small daggerverse: a module the root depends on, a shared module two others
// depend on, a nested module nothing depends on, a file declared out of its
// module's source context, and a module with an untracked input.
//
// Every test uses the same tree and differs only in which pair of commits it asks
// about, so the modules in it are built at most once however many tests run: a
// module's build is keyed on its own source context, which no commit in the
// history changes.
type fixture struct {
	dir *dagger.Directory
	// commits maps a commit's message to its SHA, in the order they were made.
	commits map[string]string
	order   []string
}

// Commit names. Each is one commit in the fixture's history, and each exists to
// be one end of a diff range.
const (
	cInitial     = "initial"
	cTouchA      = "touch a"
	cTouchCProse = "touch c prose"
	cDeleteCFile = "delete c extra"
	cTouchGlobal = "touch global"
	cTouchFlow   = "touch workflow"
	cTouchRoot   = "touch root"
)

// Module directories in the fixture.
const (
	fxRoot   = "."
	fxGlobal = "mods/global"
	fxA      = "mods/a"
	fxB      = "mods/b"
	fxC      = "mods/c"
	fxDirty  = "mods/dirty"
)

// fixtureSignature is fixed so the fixture's commit SHAs — and so the whole
// repository — are the same every time it is built.
var fixtureSignature = object.Signature{
	Name:  "workspace-ci fixture",
	Email: "fixture@example.invalid",
	When:  time.Unix(1700000000, 0).UTC(),
}

// newFixture builds the fixture repository on disk and hands back a Directory
// handle to it.
//
// variant is baked into the module the root module depends on, and is the only
// way two fixtures differ: comparing a leg's input hash between variants is how
// "the global inputs are the root module's dependency closure" is asserted, since
// a hash is read out of the tree and never depends on the diff.
func newFixture(ctx context.Context, variant string) (fixture, error) {
	// A unique directory per call, so tests running concurrently under All cannot
	// clobber one another's tree. The content is what the engine caches on, and
	// that is identical for a given variant however many copies exist.
	suffix, err := uniqueSuffix()
	if err != nil {
		return fixture{}, err
	}
	dir := "fixture-" + hashName(variant) + "-" + suffix
	defer os.RemoveAll(dir)

	fx := fixture{commits: map[string]string{}}
	write := func(path, contents string) error {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		return os.WriteFile(full, []byte(contents), 0o644)
	}

	// The tree at the initial commit.
	files := map[string]string{
		"dagger.json":              moduleConfig("root", `"source": "root", "dependencies": [{"name": "global", "source": "mods/global"}]`),
		"root/main.go":             checkSource("Root", "RootOk"),
		".github/workflows/ci.yml": "name: CI\non: [push]\n",
		"README.md":                "# fixture\n",
		fxGlobal + "/dagger.json":  moduleConfig("global", ""),
		fxGlobal + "/main.go":      "package main\n\ntype Global struct{}\n\nfunc (g *Global) Version() string { return \"" + variant + "\" }\n",
		fxA + "/dagger.json":       moduleConfig("a", ""),
		fxA + "/main.go":           checkSource("A", "Ok"),
		fxB + "/dagger.json":       moduleConfig("b", `"dependencies": [{"name": "a", "source": "../a"}]`),
		fxB + "/main.go":           checkSource("B", "Ok"),
		fxC + "/dagger.json":       moduleConfig("c", `"include": ["!README.md"]`),
		fxC + "/main.go":           checkSource("C", "Ok"),
		fxC + "/README.md":         "# c\n",
		fxC + "/extra.go":          "package main\n\nconst extra = 1\n",
		fxDirty + "/dagger.json":   moduleConfig("dirty", ""),
		fxDirty + "/main.go":       checkSource("Dirty", "Ok"),
	}
	for path, contents := range files {
		if err := write(path, contents); err != nil {
			return fixture{}, err
		}
	}

	repo, err := git.PlainInit(dir, false)
	if err != nil {
		return fixture{}, fmt.Errorf("init the fixture repository: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return fixture{}, err
	}
	commit := func(name string) error {
		if _, err := wt.Add("."); err != nil {
			return fmt.Errorf("stage %q: %w", name, err)
		}
		hash, err := wt.Commit(name, &git.CommitOptions{Author: &fixtureSignature, Committer: &fixtureSignature})
		if err != nil {
			return fmt.Errorf("commit %q: %w", name, err)
		}
		fx.commits[name] = hash.String()
		fx.order = append(fx.order, name)
		return nil
	}
	if err := commit(cInitial); err != nil {
		return fixture{}, err
	}

	// One commit per kind of change a plan has to reason about. They are separate
	// commits rather than separate fixtures so every test shares one tree.
	steps := []struct {
		name string
		do   func() error
	}{
		{cTouchA, func() error { return write(fxA+"/main.go", checkSource("A", "Ok")+"\n// touched\n") }},
		{cTouchCProse, func() error { return write(fxC+"/README.md", "# c\n\ntouched\n") }},
		{cDeleteCFile, func() error { _, err := wt.Remove(fxC + "/extra.go"); return err }},
		{cTouchGlobal, func() error {
			return write(fxGlobal+"/main.go", files[fxGlobal+"/main.go"]+"\n// touched\n")
		}},
		{cTouchFlow, func() error { return write(".github/workflows/ci.yml", "name: CI\non: [push, pull_request]\n") }},
		{cTouchRoot, func() error { return write("root/main.go", checkSource("Root", "RootOk")+"\n// touched\n") }},
	}
	for _, step := range steps {
		if err := step.do(); err != nil {
			return fixture{}, fmt.Errorf("%s: %w", step.name, err)
		}
		if err := commit(step.name); err != nil {
			return fixture{}, err
		}
	}

	// An input no commit contains: a module with one of these can never be hashed
	// from git objects, and so can never be memoized.
	if err := write(fxDirty+"/untracked.go", "package main\n\nconst untracked = 1\n"); err != nil {
		return fixture{}, err
	}

	// Read the tree back through the engine before the deferred cleanup removes it
	// from disk: Sync forces the upload now rather than when the plan asks for it.
	uploaded, err := dag.CurrentModule().Workdir(dir).Sync(ctx)
	if err != nil {
		return fixture{}, fmt.Errorf("upload the fixture: %w", err)
	}
	fx.dir = uploaded
	return fx, nil
}

// uniqueSuffix returns a short random string.
func uniqueSuffix() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// at returns the SHA of the named commit.
func (fx fixture) at(name string) string { return fx.commits[name] }

// before returns the SHA of the commit preceding the named one, so a test can ask
// about exactly one change.
func (fx fixture) before(name string) string {
	for i, n := range fx.order {
		if n == name && i > 0 {
			return fx.commits[fx.order[i-1]]
		}
	}
	return ""
}

// moduleConfig renders a dagger.json with the pinned engine version, so a fixture
// module resolves and builds the same way a real one does.
func moduleConfig(name, extra string) string {
	if extra != "" {
		extra = ", " + extra
	}
	return fmt.Sprintf(`{"name": %q, "engineVersion": "v0.21.7", "sdk": {"source": "go"}%s}`+"\n", name, extra)
}

// checkSource renders a module whose only function is a check, which is what makes
// it appear in a plan.
func checkSource(object, check string) string {
	return fmt.Sprintf("package main\n\ntype %s struct{}\n\n// +check\nfunc (o *%s) %s() error { return nil }\n", object, object, check)
}

// hashName turns a variant into a directory-safe suffix.
func hashName(variant string) string {
	if variant == "" {
		return "default"
	}
	return variant
}
