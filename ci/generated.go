package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"dagger/ci/internal/dagger"

	"golang.org/x/sync/errgroup"
)

// codegenParallelism bounds how many module codegens are in flight at once.
// Codegen is engine-side work (one Go SDK container per module), so the useful
// ceiling is engine capacity rather than CPU count in this container.
const codegenParallelism = 8

// selfTestProbeModule is the module the self-test deliberately makes stale. It
// is the smallest module in the repo (no dependencies), so regenerating it is
// the cheapest way to prove the check can fail.
const selfTestProbeModule = "daggerverse/random"

// drift is one module whose committed generated files differ from what codegen
// produces at the pinned engineVersion.
type drift struct {
	module string // repo-relative module source root ("." for the root ci module)
	patch  string
}

// Verify that committed dagger.gen.go and internal/dagger/*.gen.go files
// match what `dagger develop` would produce at the pinned engineVersion.
//
// Every module in the workspace is checked -- the root ci module (whose
// per-toolchain aggregator bindings live in ci/internal/dagger/) as well as
// every daggerverse/<mod>, its tests toolchain and any examples module.
//
// The result is deliberately never cached: the workspace is read at call time
// rather than passed as an argument, so a cached pass would be a pass for a
// tree the check never looked at.
//
// +check
// +cache="never"
func (ci *Ci) Generated(ctx context.Context) error {
	root, cleanup, err := materializeWorkspace(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	modules, err := moduleRoots(root)
	if err != nil {
		return err
	}

	drifted, err := codegenDrift(ctx, root, modules)
	if err != nil {
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
	return fmt.Errorf("generated files are not up-to-date; run `dagger develop` in: %s", strings.Join(names, ", "))
}

// GeneratedSelfTest pins that Generated can actually fail. The check it guards
// silently verified nothing for months (it routed through Workspace.Generators,
// which is empty unless a module declares a +generator function), so a green
// ci:generated is only worth as much as the proof that a stale module turns it
// red.
//
// It runs the same codegen comparison Generated does against a single module,
// first pristine (expecting no drift) and then with its committed bindings
// deliberately made stale (expecting drift naming that file).
//
// +check
// +cache="never"
func (ci *Ci) GeneratedSelfTest(ctx context.Context) error {
	probeFile := filepath.Join(selfTestProbeModule, "internal", "dagger", "dagger.gen.go")

	pristine, cleanPristine, err := materializeWorkspace(ctx)
	if err != nil {
		return err
	}
	defer cleanPristine()

	clean, err := codegenDrift(ctx, pristine, []string{selfTestProbeModule})
	if err != nil {
		return err
	}
	if len(clean) != 0 {
		return fmt.Errorf("self-test: %s reports drift on an unmodified workspace:\n%s", selfTestProbeModule, clean[0].patch)
	}

	// A second export, rather than mutating the first, so the engine cannot
	// serve the tampered tree from its snapshot of the pristine path.
	tampered, cleanTampered, err := materializeWorkspace(ctx)
	if err != nil {
		return err
	}
	defer cleanTampered()

	if err := appendLine(filepath.Join(tampered, probeFile), "// ci:generated-self-test: deliberately stale"); err != nil {
		return err
	}

	stale, err := codegenDrift(ctx, tampered, []string{selfTestProbeModule})
	if err != nil {
		return err
	}
	if len(stale) == 0 {
		return fmt.Errorf("self-test: no drift reported for %s after making %s stale; ci:generated verifies nothing", selfTestProbeModule, probeFile)
	}
	if !strings.Contains(stale[0].patch, probeFile) {
		return fmt.Errorf("self-test: drift reported for %s does not name %s:\n%s", selfTestProbeModule, probeFile, stale[0].patch)
	}
	return nil
}

// codegenDrift runs codegen for each module source root (repo-relative, "." for
// the root module) against the workspace copy at root, and returns those whose
// committed files differ from the generated output, ordered like modules.
//
// The module sources are loaded from root -- a copy of the workspace exported
// into this container -- rather than from the live Workspace, because loading a
// module's toolchains resolves their default-path context against a host path
// the module runtime cannot see. Everything under root is visible to it.
func codegenDrift(ctx context.Context, root string, modules []string) ([]drift, error) {
	found := make([]*drift, len(modules))

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(codegenParallelism)
	for i, mod := range modules {
		g.Go(func() error {
			changes := dag.ModuleSource(filepath.Join(root, mod)).GeneratedContextChangeset()
			empty, err := changes.IsEmpty(ctx)
			if err != nil {
				return fmt.Errorf("codegen %s: %w", mod, err)
			}
			if empty {
				return nil
			}
			patch, err := changes.AsPatch().Contents(ctx)
			if err != nil {
				return fmt.Errorf("patch %s: %w", mod, err)
			}
			found[i] = &drift{module: mod, patch: patch}
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

// materializeWorkspace exports the workspace into a scratch directory and
// returns its absolute path. Codegen reads module sources as local paths, which
// resolve inside this container, so the tree has to exist on disk -- a lazy
// Directory handle is not enough.
func materializeWorkspace(ctx context.Context) (string, func(), error) {
	suffix, err := uniqueSuffix()
	if err != nil {
		return "", nil, err
	}
	scratch := "generated-" + suffix
	cleanup := func() { os.RemoveAll(scratch) }

	// "/" -- an absolute path, which resolves from the workspace boundary. A
	// relative "." resolves from the workspace's current directory, which is the
	// module source dir (ci/) whenever the module is loaded from there, and that
	// tree contains no dagger.json at all.
	//
	// .git is excluded because it is dead weight for codegen, but an empty one
	// is put back: a module's context directory is found by walking up to the
	// repository root, and without that marker every module would take its own
	// source root as the context and dependencies like "../../crypto" would
	// escape it.
	ws := dag.CurrentWorkspace().Directory("/", dagger.WorkspaceDirectoryOpts{
		Exclude: []string{".git"},
	})
	if _, err := ws.Export(ctx, scratch); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("export workspace: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(scratch, ".git"), 0o755); err != nil {
		cleanup()
		return "", nil, err
	}

	abs, err := filepath.Abs(scratch)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return abs, cleanup, nil
}

// moduleRoots returns every module source root under root, repo-relative and
// sorted, with the root module reported as ".".
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
		return nil, fmt.Errorf("walk workspace: %w", err)
	}
	if len(modules) == 0 {
		entries, _ := os.ReadDir(root)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return nil, fmt.Errorf("no dagger.json found in the workspace (root=%s entries=%v)", root, names)
	}
	sort.Strings(modules)
	return modules, nil
}

// appendLine appends line, newline-terminated, to an existing file.
func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, line); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
