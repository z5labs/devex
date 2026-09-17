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
// internal/dagger/*.gen.go in the calling workspace matches what codegen
// produces at each module's pinned engineVersion.
//
// Every module in the workspace is checked, including the root one and every
// tests or examples module.
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
	root, _, dir, cleanup, err := materializeWorkspace(ctx, callingWorkspace)
	if err != nil {
		return err
	}
	defer cleanup()

	modules, err := moduleRoots(root)
	if err != nil {
		return err
	}

	drifted, err := codegenDrift(ctx, dir, modules)
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
	return fmt.Errorf("generated files are not up-to-date; regenerate: %s", strings.Join(names, ", "))
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
	// The workspace to check. A Dagger CLI fills this in from the workspace the
	// call was made in; a module calling this one has to pass on the workspace the
	// CLI handed it, or make one out of a directory with Directory.asWorkspace.
	// It is not called "workspace" because --workspace is one of the CLI's own
	// global flags, and a function argument cannot take a name it has claimed.
	callingWorkspace *dagger.Workspace,
	// The module to make stale, repo-relative. Defaults to the first
	// dependency-free module in the workspace, which is the cheapest one to
	// regenerate.
	//
	// +optional
	probeModule string,
) error {
	pristine, _, pristineDir, cleanPristine, err := materializeWorkspace(ctx, callingWorkspace)
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

	clean, err := codegenDrift(ctx, pristineDir, []string{probeModule})
	if err != nil {
		return err
	}
	if len(clean) != 0 {
		return fmt.Errorf("self-test: %s reports drift on an unmodified workspace:\n%s", probeModule, clean[0].patch)
	}

	// A second export, rather than mutating the first, so the engine cannot serve
	// the tampered tree from its snapshot of the pristine path.
	tampered, tamperedRel, _, cleanTampered, err := materializeWorkspace(ctx, callingWorkspace)
	if err != nil {
		return err
	}
	defer cleanTampered()

	if err := appendLine(filepath.Join(tampered, probeFile), "// workspace-ci:generated-self-test: deliberately stale"); err != nil {
		return fmt.Errorf("self-test: cannot make %s stale: %w", probeFile, err)
	}

	// Read the tampered tree back off this container's own disk rather than reusing
	// the Directory it was exported from, which still describes the pristine tree.
	stale, err := codegenDrift(ctx, dag.CurrentModule().Workdir(tamperedRel), []string{probeModule})
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
// the root module) against the workspace directory, and returns those whose
// committed files differ from the generated output, ordered like modules.
func codegenDrift(ctx context.Context, dir *dagger.Directory, modules []string) ([]drift, error) {
	found := make([]*drift, len(modules))

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(codegenParallelism)
	for i, mod := range modules {
		g.Go(func() error {
			changes := moduleSource(dir, mod).GeneratedContextChangeset()
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

// materializeWorkspace exports the given workspace into a scratch directory and
// returns that directory both as a path in this container and as the Directory
// codegen runs against, plus the name to read the path back under.
//
// The export exists because moduleRoots walks an os filesystem, and because the
// self-test has to edit a file before regenerating it. Codegen itself runs off
// the Directory: a module runtime cannot load a local module source, so the path
// is never what a module is resolved from.
//
// .git is excluded: it is dead weight for codegen, and with the module's context
// given explicitly nothing has to walk up to a repository root to find one.
func materializeWorkspace(ctx context.Context, callingWorkspace *dagger.Workspace) (root, rel string, dir *dagger.Directory, cleanup func(), err error) {
	if callingWorkspace == nil {
		return "", "", nil, nil, fmt.Errorf("no workspace to check: a Dagger CLI fills one in from the caller's own, but a module calling this one has to pass the workspace the CLI handed it")
	}
	dir = callingWorkspace.Directory("/", dagger.WorkspaceDirectoryOpts{
		Exclude: []string{".git"},
	})
	root, rel, cleanup, err = exportDir(ctx, dir)
	if err != nil {
		return "", "", nil, nil, err
	}
	return root, rel, dir, cleanup, nil
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
