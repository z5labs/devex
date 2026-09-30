package main

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"dagger/tests/internal/dagger"
)

// tomlModuleConfig renders a dagger-module.toml, the config a module has once its
// workspace is migrated off dagger.json. source may be empty.
func tomlModuleConfig(name, source string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "name = %q\nengineVersion = %q\n", name, fixtureEngineVersion)
	if source != "" {
		fmt.Fprintf(&b, "source = %q\n", source)
	}
	b.WriteString("\n[runtime]\n  source = \"go\"\n")
	return b.String()
}

// configShapeTrees are three small workspaces that differ only in how their
// modules are configured: every module by dagger.json, every module by
// dagger-module.toml, and a mixture — including one directory holding both, where
// the engine reads dagger-module.toml.
//
// Each maps a tree to the modules it holds, directory to the name the planner must
// report for it.
func configShapeTrees() map[string]struct {
	dir  *dagger.Directory
	want map[string]string
} {
	root := checkSource("Root", "RootOk")
	x := checkSource("X", "Ok")
	both := checkSource("Both", "Ok")
	type tree = struct {
		dir  *dagger.Directory
		want map[string]string
	}
	return map[string]tree{
		"all dagger.json": {
			dir: dag.Directory().
				WithNewFile("dagger.json", moduleConfig("root", `"source": "root"`)).
				WithNewFile("root/main.go", root).
				WithNewFile("mods/x/dagger.json", moduleConfig("x", "")).
				WithNewFile("mods/x/main.go", x),
			want: map[string]string{fxRoot: "root", "mods/x": "x"},
		},
		"all dagger-module.toml": {
			dir: dag.Directory().
				WithNewFile("dagger-module.toml", tomlModuleConfig("root", "root")).
				WithNewFile("root/main.go", root).
				WithNewFile("mods/x/dagger-module.toml", tomlModuleConfig("x", "")).
				WithNewFile("mods/x/main.go", x),
			want: map[string]string{fxRoot: "root", "mods/x": "x"},
		},
		"mixed": {
			dir: dag.Directory().
				WithNewFile("dagger.json", moduleConfig("root", `"source": "root"`)).
				WithNewFile("root/main.go", root).
				WithNewFile("mods/x/dagger-module.toml", tomlModuleConfig("x", "")).
				WithNewFile("mods/x/main.go", x).
				WithNewFile("mods/both/dagger.json", moduleConfig("bothjson", "")).
				WithNewFile("mods/both/dagger-module.toml", tomlModuleConfig("bothtoml", "")).
				WithNewFile("mods/both/main.go", both),
			want: map[string]string{fxRoot: "root", "mods/x": "x", "mods/both": "bothtoml"},
		},
	}
}

// PlanFindsModulesInEveryConfigShape proves discovery knows both config shapes:
// dagger.json, and the dagger-module.toml a workspace has once it is migrated.
// Before this, a module migrated to dagger-module.toml simply vanished from the
// plan — its changes planned only the root module's legs, and the gate went green.
//
// Each tree is planned with no usable diff, so everything runs and no module is
// built: what comes back is discovery's answer and the name read from each
// module's config, which is also what shows dagger-module.toml winning where a
// directory holds both.
func (t *Tests) PlanFindsModulesInEveryConfigShape(ctx context.Context) error {
	for shape, tree := range configShapeTrees() {
		got, err := explainRange(ctx, dag.WorkspaceCi(), fixture{dir: tree.dir}, "", "", "")
		if err != nil {
			return fmt.Errorf("%s: %w", shape, err)
		}
		if err := wantLegs(got, slices.Collect(maps.Keys(tree.want))...); err != nil {
			return fmt.Errorf("%s: %w", shape, err)
		}
		for _, l := range got.Plan {
			if want := tree.want[l.Module]; l.ModuleName != want {
				return fmt.Errorf("%s: leg %q carries module name %q, want %q", shape, l.Name, l.ModuleName, want)
			}
		}
		if len(got.LoadedModules) != 0 {
			return fmt.Errorf("%s: discovering modules built %v", shape, got.LoadedModules)
		}
	}
	return nil
}

// GeneratedReportsAnUnsweptModule proves Generated fails when a module goes
// unswept, rather than passing having looked at less than the whole workspace.
//
// The tree is the mixed config-shape tree plus one module configured by a file
// discovery has never heard of, each with a committed dagger.gen.go. Only the
// unknown one may be reported: the others are what shows Generated discovers both
// config shapes, since a module it missed would be reported beside it. The check
// runs before any codegen, which is what keeps this test cheap — nothing in the
// tree is ever generated.
func (t *Tests) GeneratedReportsAnUnsweptModule(ctx context.Context) error {
	const unknown = "mods/unknown/dagger.gen.go"
	dir := configShapeTrees()["mixed"].dir
	for _, p := range []string{
		"root/dagger.gen.go",
		"root/internal/dagger/dagger.gen.go",
		"mods/x/dagger.gen.go",
		"mods/both/internal/dagger/dagger.gen.go",
		unknown,
	} {
		dir = dir.WithNewFile(p, "package main\n")
	}
	dir = dir.
		WithNewFile("mods/unknown/dagger.yaml", "name: unknown\n").
		WithNewFile("mods/unknown/main.go", checkSource("Unknown", "Ok"))

	err := checkErr(ctx, dag.WorkspaceCi().Generated(dir.AsWorkspace()))
	if err == nil {
		return fmt.Errorf("a module discovery does not recognise went unswept and Generated passed")
	}
	msg := err.Error()
	if !strings.Contains(msg, unknown) || !strings.Contains(msg, "belong to no module") {
		return fmt.Errorf("Generated failed, but not by reporting %s as unswept: %v", unknown, err)
	}
	for _, swept := range []string{"root/", "mods/x/", "mods/both/"} {
		if strings.Contains(msg, swept) {
			return fmt.Errorf("Generated reported a module under %s as unswept, so discovery missed it: %v", swept, err)
		}
	}
	return nil
}

// generatedTree is a one-module workspace with its bindings freshly generated, so
// the only thing that can make Generated red on it is a change made afterwards.
//
// generatedContextDirectory holds the config and what codegen wrote but not the
// module's own source, so it is laid over the source rather than used alone.
func generatedTree() *dagger.Directory {
	src := dag.Directory().
		WithNewFile("dagger.json", moduleConfig("root", `"source": "root", "codegen": {"automaticGitignore": false}`)).
		WithNewFile("root/main.go", checkSource("Root", "RootOk"))
	return src.WithDirectory(".", src.AsModuleSource().GeneratedContextDirectory())
}

// GeneratedPassesOnFreshBindingsAndFailsOnStaleOnes runs the whole check end to
// end — discovery, the unswept check, the stale-module proof and the sweep —
// against a workspace whose bindings are fresh, then against the same workspace
// with one binding made stale.
//
// The first half is what shows the proof and the unswept check do not fail a
// workspace that is fine; the second, that the sweep itself still names the
// module and the file.
func (t *Tests) GeneratedPassesOnFreshBindingsAndFailsOnStaleOnes(ctx context.Context) error {
	fresh := generatedTree()
	if err := checkErr(ctx, dag.WorkspaceCi().Generated(fresh.AsWorkspace())); err != nil {
		return fmt.Errorf("Generated failed on freshly generated bindings: %w", err)
	}

	const binding = "root/internal/dagger/dagger.gen.go"
	contents, err := fresh.File(binding).Contents(ctx)
	if err != nil {
		return err
	}
	stale := fresh.WithNewFile(binding, contents+"\n// stale\n")
	err = checkErr(ctx, dag.WorkspaceCi().Generated(stale.AsWorkspace()))
	if err == nil {
		return fmt.Errorf("Generated passed with %s deliberately stale", binding)
	}
	if !strings.Contains(err.Error(), "not up-to-date") {
		return fmt.Errorf("Generated failed on a stale binding for the wrong reason: %w", err)
	}
	return nil
}
