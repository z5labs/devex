package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sync/errgroup"

	"dagger/workspace-ci/internal/dagger"
)

// codegenParallelism bounds how many module codegens are in flight at once.
// Codegen is engine-side work (one SDK container per module), so the useful
// ceiling is engine capacity rather than CPU count in this container.
const codegenParallelism = 8

// drift is one module whose committed generated files differ from what codegen
// produces at the pinned engineVersion.
type drift struct {
	module string // repo-relative module source root ("." for the root module)
	patch  string
}

// Generated verifies that every committed dagger.gen.go and
// internal/dagger/*.gen.go in the calling workspace matches what `dagger develop`
// would produce at each module's pinned engineVersion.
//
// Every module in the workspace is checked, including the root one and every
// tests or examples module.
//
// This check is why generated files need not be global inputs to the memoization
// hash: it proves they are derived from inputs that are, it belongs to the root
// module so a plan always runs it, and it is never memoized. The result is
// deliberately never cached either — the workspace is read at call time rather
// than passed as an argument, so a cached pass would be a pass for a tree the
// check never looked at.
//
// +check
// +cache="never"
func (m *WorkspaceCi) Generated(ctx context.Context) error {
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

// GeneratedSelfTest pins that Generated can actually fail.
//
// The check this repo extracted it from silently verified nothing for months (it
// routed through Workspace.Generators, which is empty unless a module declares a
// +generator function), so a green Generated is only worth as much as the proof
// that a stale module turns it red (#184).
//
// It runs the same codegen comparison against a single module, first pristine
// (expecting no drift) and then with that module's committed bindings deliberately
// made stale (expecting drift naming the file).
//
// +check
// +cache="never"
func (m *WorkspaceCi) GeneratedSelfTest(
	ctx context.Context,
	// The module to make stale, repo-relative. Defaults to the first
	// dependency-free module in the workspace, which is the cheapest one to
	// regenerate.
	//
	// +optional
	probeModule string,
) error {
	pristine, cleanPristine, err := materializeWorkspace(ctx)
	if err != nil {
		return err
	}
	defer cleanPristine()

	if probeModule == "" {
		if probeModule, err = defaultProbeModule(pristine); err != nil {
			return err
		}
	}
	probeFile := filepath.Join(probeModule, "internal", "dagger", "dagger.gen.go")

	clean, err := codegenDrift(ctx, pristine, []string{probeModule})
	if err != nil {
		return err
	}
	if len(clean) != 0 {
		return fmt.Errorf("self-test: %s reports drift on an unmodified workspace:\n%s", probeModule, clean[0].patch)
	}

	// A second export, rather than mutating the first, so the engine cannot serve
	// the tampered tree from its snapshot of the pristine path.
	tampered, cleanTampered, err := materializeWorkspace(ctx)
	if err != nil {
		return err
	}
	defer cleanTampered()

	if err := appendLine(filepath.Join(tampered, probeFile), "// workspace-ci:generated-self-test: deliberately stale"); err != nil {
		return fmt.Errorf("self-test: cannot make %s stale: %w", probeFile, err)
	}

	stale, err := codegenDrift(ctx, tampered, []string{probeModule})
	if err != nil {
		return err
	}
	if len(stale) == 0 {
		return fmt.Errorf("self-test: no drift reported for %s after making %s stale; the generated check verifies nothing", probeModule, probeFile)
	}
	if !strings.Contains(stale[0].patch, probeFile) {
		return fmt.Errorf("self-test: drift reported for %s does not name %s:\n%s", probeModule, probeFile, stale[0].patch)
	}
	return nil
}

// defaultProbeModule returns the first module in the workspace with no
// dependencies of its own, which is the cheapest one to regenerate and the least
// likely to fail for a reason other than the tampering.
//
// The root module is excluded: it is the one module whose generated bindings
// depend on every toolchain it installs, so it is both the most expensive to
// regenerate and the one whose drift is least specific.
func defaultProbeModule(root string) (string, error) {
	modules, err := moduleRoots(root)
	if err != nil {
		return "", err
	}
	for _, dir := range modules {
		if dir == "." {
			continue
		}
		var cfg struct {
			Dependencies []struct{} `json:"dependencies"`
			SDK          struct {
				Source string `json:"source"`
			} `json:"sdk"`
		}
		raw, err := os.ReadFile(filepath.Join(root, dir, "dagger.json"))
		if err != nil {
			continue
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			continue
		}
		if len(cfg.Dependencies) > 0 || cfg.SDK.Source == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, dir, "internal", "dagger", "dagger.gen.go")); err != nil {
			continue // nothing committed to make stale
		}
		return dir, nil
	}
	return "", fmt.Errorf("no dependency-free module with committed bindings to probe; pass probeModule")
}

// codegenDrift runs codegen for each module source root (repo-relative, "." for
// the root module) against the workspace copy at root, and returns those whose
// committed files differ from the generated output, ordered like modules.
//
// The module sources are loaded from root -- a copy of the workspace exported into
// this container -- rather than from the live Workspace, because loading a
// module's toolchains resolves their default-path context against a host path the
// module runtime cannot see. Everything under root is visible to it.
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

// materializeWorkspace exports the calling workspace into a scratch directory and
// returns its absolute path. Codegen reads module sources as local paths, which
// resolve inside this container, so the tree has to exist on disk -- a lazy
// Directory handle is not enough.
func materializeWorkspace(ctx context.Context) (string, func(), error) {
	// .git is excluded because it is dead weight for codegen, but an empty one is
	// put back: a module's context directory is found by walking up to the
	// repository root, and without that marker every module would take its own
	// source root as the context and a dependency like "../../crypto" would escape
	// it.
	ws := dag.CurrentWorkspace().Directory("/", dagger.WorkspaceDirectoryOpts{
		Exclude: []string{".git"},
	})
	root, cleanup, err := exportDir(ctx, ws)
	if err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		cleanup()
		return "", nil, err
	}
	return root, cleanup, nil
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
