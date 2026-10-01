package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sync/errgroup"

	"dagger/workspace-ci/internal/dagger"
	"dagger/workspace-ci/planner"
)

// codegenParallelism bounds how many module codegens are in flight at once.
// Codegen is engine-side work (one SDK container per module), so the useful
// ceiling is engine capacity rather than CPU count in this container.
const codegenParallelism = 8

// generatedGlobs find every file codegen writes into a Go module and a
// repository commits: the module's own dagger.gen.go and its client under
// internal/dagger. Unswept matches them against the modules the sweep covered.
var generatedGlobs = []string{"**/dagger.gen.go", "**/internal/dagger/*.gen.go"}

// drift is one module whose committed generated files differ from what codegen
// produces at the pinned engineVersion.
type drift struct {
	module string // repo-relative module source root ("." for the root module)
	patch  string
}

// Generated verifies that every committed dagger.gen.go and
// internal/dagger/*.gen.go in the calling workspace matches what codegen
// produces at each module's pinned engineVersion.
//
// How depends on how the workspace is configured. One whose dagger.toml installs
// its SDKs already has a check that compares the tree — the SDK module's own
// staleness check, which Plan gives a leg in every plan — so there this check
// makes no comparison of its own and instead proves what makes that one worth
// trusting: that every module with generated files is managed by an SDK, and
// that the Go SDK's staleness check fails on a module made stale on purpose. See
// sdkGenerated. A workspace configured by dagger.json alone installs no SDK, and
// gets the sweep described below.
//
// Every module in the workspace is checked, including the root one and every
// tests or examples module. Two things make that claim more than a hope:
//
//   - It fails when a module went unswept. Every committed generated file must
//     belong to a module the sweep covered, which is found by globbing for the
//     files rather than by asking the discovery that drove the sweep — so a
//     module whose config shape discovery does not know is reported instead of
//     skipped.
//   - It proves on every run that it can fail. Alongside the sweep it builds a
//     bare module of its own, deliberately makes that module's bindings stale,
//     and runs the same comparison on it; unless the drift is reported, naming
//     the stale file, the check fails. The check this was extracted from
//     silently verified nothing for months (#184), so a green sweep is only
//     worth as much as the proof that a stale module turns it red.
//
// This check is why generated files need not be global inputs to the memoization
// hash: it proves they are derived from inputs that are, it belongs to the root
// module so a plan always runs it, and it is never memoized. The result is
// deliberately never cached either — the workspace handle the CLI fills in is a
// live view of the tree rather than a snapshot argument the cache key can
// describe, so a cached pass would be a pass for a tree the check never looked
// at.
//
// +check
// +cache="never"
func (m *WorkspaceCi) Generated(
	ctx context.Context,
	// The workspace to check. A Dagger CLI fills this in from the workspace the
	// call was made in; a module calling this one has to pass on the workspace the
	// CLI handed it, or make one out of a directory with Directory.asWorkspace.
	// It is not called "workspace" because --workspace is one of the CLI's own
	// global flags, and a function argument cannot take a name it has claimed.
	callingWorkspace *dagger.Workspace,
) error {
	if callingWorkspace == nil {
		return fmt.Errorf("no workspace to check: a Dagger CLI fills one in from the caller's own, but a module calling this one has to pass the workspace the CLI handed it")
	}
	sdks, err := callingWorkspace.Sdks(ctx)
	if err != nil {
		return fmt.Errorf("list the SDKs the workspace installs: %w", err)
	}
	if len(sdks) > 0 {
		return sdkGenerated(ctx, callingWorkspace, sdks)
	}
	modules, err := discoverModules(ctx, callingWorkspace)
	if err != nil {
		return err
	}
	sources := make(map[string]*dagger.ModuleSource, len(modules))
	for _, dir := range modules {
		sources[dir] = moduleSource(callingWorkspace, dir)
	}

	// First, and on its own: it needs no codegen, so a module the sweep would miss
	// is reported in seconds rather than after it.
	if err := sweptEverything(ctx, callingWorkspace, modules, sources, unsweptByDiscovery); err != nil {
		return err
	}

	var drifted []drift
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		pin, err := sources[modules[0]].EngineVersion(gctx)
		if err != nil {
			return fmt.Errorf("read the engine version to build the stale-module proof at: %w", err)
		}
		return staleModuleIsReported(gctx, callingWorkspace, pin)
	})
	g.Go(func() error {
		var err error
		drifted, err = codegenDrift(gctx, sources, modules)
		return err
	})
	if err := g.Wait(); err != nil {
		return err
	}
	if len(drifted) == 0 {
		return nil
	}

	names := make([]string, 0, len(drifted))
	for _, d := range drifted {
		fmt.Fprintf(os.Stderr, "==> %s is not up-to-date:\n%s\n", d.module, d.patch)
		names = append(names, d.module)
	}
	return fmt.Errorf("generated files are not up-to-date; regenerate: %s", strings.Join(names, ", "))
}

// sweptEverything fails when a committed generated file belongs to no module in
// modules — that is, when some module in the workspace would go unswept.
//
// The list it checks against is deliberately not the one that drives the sweep.
// The sweep's list is whatever discovery recognised; this one is every file
// codegen writes, found by globbing the workspace. A module discovery missed —
// configured in a shape it was never taught — still has those files committed, so
// it turns this red rather than disappearing from a green run.
//
// hint finishes the error, saying why a module might have been missed.
func sweptEverything(ctx context.Context, ws *dagger.Workspace, modules []string, sources map[string]*dagger.ModuleSource, hint string) error {
	seen := map[string]bool{}
	var generated []string
	for _, pattern := range generatedGlobs {
		// Workspace.glob matches from the workspace root, wherever the caller stood.
		found, err := ws.Glob(ctx, pattern)
		if err != nil {
			return fmt.Errorf("list the workspace's generated files: %w", err)
		}
		for _, p := range found {
			p = strings.TrimPrefix(p, "/")
			if strings.HasSuffix(p, "/") || seen[p] {
				continue
			}
			seen[p] = true
			generated = append(generated, p)
		}
	}

	sourceDirs := make([]string, len(modules))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(engineConcurrency)
	for i, dir := range modules {
		g.Go(func() error {
			sub, err := sources[dir].SourceSubpath(gctx)
			if err != nil {
				return fmt.Errorf("read the source subpath of %q: %w", dir, err)
			}
			sourceDirs[i] = sub
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	if unswept := planner.Unswept(generated, sourceDirs); len(unswept) > 0 {
		return fmt.Errorf("these generated files belong to no module this check found, so nothing verified them; %s\n  %s",
			hint, strings.Join(unswept, "\n  "))
	}
	return nil
}

// unsweptByDiscovery and unsweptBySDK are the two reasons sweptEverything gives
// for a module it was never shown.
var (
	unsweptByDiscovery = "is their module configured by something other than " + strings.Join(configFiles, " or ") + "?"
	unsweptBySDK       = "is their module missing from its SDK's scopes in the workspace config? Add [sdks.<sdk>.scopes.\"<dir>\"] with is-module = true"
)

// probeModuleSource is the synthetic module staleModuleIsReported builds: one
// object, one function, no dependencies, so its codegen costs the same however
// the workspace grows.
const probeModuleSource = `package main

// GeneratedProbe exists to be generated. workspace-ci builds it from scratch on
// every Generated run and makes its bindings stale on purpose.
type GeneratedProbe struct{}

// Ok returns ok.
func (m *GeneratedProbe) Ok() string { return "ok" }
`

// probeStaleFile is the file staleModuleIsReported tampers with.
const probeStaleFile = "internal/dagger/dagger.gen.go"

// staleModuleIsReported proves the codegen comparison can fail. It builds a bare
// module pinned at pin and generates its bindings, then runs the comparison every
// swept module goes through twice: on the module as generated, which must report
// nothing, and on a copy with those bindings made stale, which must report drift
// naming the stale file. Either other answer is an error.
//
// The pristine half is what makes the stale half mean something. Drift on a module
// that was never valid to begin with would satisfy "drift was reported" for a
// reason that has nothing to do with the tampering.
//
// The module is built here rather than borrowed from the workspace so that the
// proof has a fixed cost and no input from outside this module's own code: a
// probe that was one of the workspace's modules made the proof's cost and its
// correctness depend on whichever module that was.
//
// It is placed *into* ws, under a hidden directory with a random name, and
// resolved with ws.moduleSource exactly as every swept module is. Resolving it
// from a Directory instead would prove the comparison on a DIR_SOURCE while the
// sweep runs on the CLI's LOCAL_SOURCE, and an engine regression specific to one
// route would pass on the other — which is the shape #184 had. Workspace.with*
// returns a new workspace and writes nothing to the caller's checkout, and the
// sweep reads ws itself, so the probe is never swept or globbed as one of the
// workspace's own modules. Its bindings are generated in place as well, because
// they record the module's path relative to its context: bindings generated
// anywhere else read as drift.
func staleModuleIsReported(ctx context.Context, ws *dagger.Workspace, pin string) error {
	config := fmt.Sprintf(`{"name": "generated-probe", "engineVersion": %q, "sdk": {"source": "go"}, "codegen": {"automaticGitignore": false}}`+"\n", pin)
	src := dag.Directory().
		WithNewFile("dagger.json", config).
		WithNewFile("main.go", probeModuleSource)

	suffix, err := uniqueSuffix()
	if err != nil {
		return err
	}
	path := "/.workspace-ci-generated-probe-" + suffix
	base := ws.WithNewDirectory(path, src)
	// generatedContextDirectory holds the config and what codegen wrote, not the
	// module's own source, rooted where the module's context is rooted, so it
	// merges over the workspace rather than replacing anything — the same way it
	// is exported over a checkout to regenerate one by hand.
	// Were the workspace's context rooted anywhere else, the bindings would land
	// beside the module rather than in it and the read below would fail: closed,
	// with the proof reported as broken rather than passed.
	pristine := base.WithDirectory("/", base.ModuleSource(path).GeneratedContextDirectory())
	staleFile := path + "/" + probeStaleFile
	contents, err := pristine.File(staleFile).Contents(ctx)
	if err != nil {
		return fmt.Errorf("stale-module proof: generate the probe module: %w", err)
	}
	stale := pristine.WithNewFile(staleFile, contents+"\n// workspace-ci: deliberately stale\n")

	var clean, drifted string
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) {
		clean, err = moduleDrift(gctx, pristine.ModuleSource(path))
		return err
	})
	g.Go(func() (err error) {
		drifted, err = moduleDrift(gctx, stale.ModuleSource(path))
		return err
	})
	if err := g.Wait(); err != nil {
		return fmt.Errorf("stale-module proof: %w", err)
	}
	if clean != "" {
		return fmt.Errorf("stale-module proof: a module whose bindings were just generated reports drift, so drift proves nothing:\n%s", clean)
	}
	if drifted == "" {
		return fmt.Errorf("stale-module proof: a module with deliberately stale bindings was reported up-to-date, so this check verifies nothing")
	}
	if !strings.Contains(drifted, probeStaleFile) {
		return fmt.Errorf("stale-module proof: the drift reported for a stale %s does not name it:\n%s", probeStaleFile, drifted)
	}
	return nil
}

// codegenDrift runs codegen for each module in modules and returns those whose
// committed files differ from the generated output, ordered like modules.
func codegenDrift(ctx context.Context, sources map[string]*dagger.ModuleSource, modules []string) ([]drift, error) {
	found := make([]*drift, len(modules))

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(codegenParallelism)
	for i, mod := range modules {
		g.Go(func() error {
			patch, err := moduleDrift(ctx, sources[mod])
			if err != nil {
				return fmt.Errorf("%s: %w", mod, err)
			}
			if patch != "" {
				found[i] = &drift{module: mod, patch: patch}
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	drifted := make([]drift, 0, len(modules))
	for _, d := range found {
		if d != nil {
			drifted = append(drifted, *d)
		}
	}
	return drifted, nil
}

// moduleDrift runs codegen for one module and returns the patch that would bring
// its committed files up to date, or "" when there is none. The sweep and the
// stale-module proof both come through here, which is what makes the proof about
// the sweep.
func moduleDrift(ctx context.Context, src *dagger.ModuleSource) (string, error) {
	changes := src.GeneratedContextChangeset()
	empty, err := changes.IsEmpty(ctx)
	if err != nil {
		return "", fmt.Errorf("codegen: %w", err)
	}
	if empty {
		return "", nil
	}
	patch, err := changes.AsPatch().Contents(ctx)
	if err != nil {
		return "", fmt.Errorf("patch: %w", err)
	}
	return patch, nil
}
