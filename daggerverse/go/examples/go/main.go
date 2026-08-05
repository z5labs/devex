// Package main is the go-examples Dagger module: a runnable cookbook of go
// recipes. Each one walks a common Go-toolchain operation -- compile, test,
// tidy up, install a tool -- the way a downstream consumer would call it,
// passing only directories, files and primitives across the module boundary.
//
// Every recipe defaults to the sample module vendored in `sample/`
// (a two-package, stdlib-only `example.com/greeter`), so `dagger call` works
// with no arguments; pass --source to point any recipe at your own tree.
package main

import (
	"context"
	"fmt"

	"dagger/go-examples/internal/dagger"
)

// GoExamples is the module's main object: a namespace for the go usage
// recipes.
type GoExamples struct{}

// sourceOrSample returns source, or the built-in sample module when the
// caller passed nothing.
//
// The sample is read straight off this module's own source directory rather
// than embedded with //go:embed: it carries its own go.mod, and go:embed
// refuses to walk into a nested module.
func sourceOrSample(source *dagger.Directory) *dagger.Directory {
	if source != nil {
		return source
	}
	return dag.CurrentModule().Source().Directory("sample")
}

// BuildBinary compiles a Go source tree and returns the single produced
// executable as a *dagger.File -- the recipe to reach for when the artifact
// you actually want out of a pipeline is one binary to ship:
//
//	dagger call build-binary export --path ./greeter
//
// Two details worth copying. Build returns the whole /out directory (go
// build can emit one binary per main package), so you select the file you
// want out of it. And passing output pins that filename, which is what makes
// the .File("app") selection below deterministic -- leave output empty and
// go names each binary after its own package instead.
//
// Note the toolchain is never named here: go infers it from the source's
// go.mod `go` directive, so the sample builds on golang:1.23. Pin it
// explicitly with dag.Go(dagger.GoOpts{Version: "1.24"}) when a project
// needs a newer compiler than it declares.
func (m *GoExamples) BuildBinary(
	// The Go source tree to build. Defaults to the built-in sample module.
	//
	// +optional
	source *dagger.Directory,
	// The package to build, relative to the source root. Must resolve to a
	// single main package, since output names one file.
	//
	// +default="."
	pkg string,
) *dagger.File {
	return dag.Go().
		Build(sourceOrSample(source), dagger.GoBuildOpts{Pkg: pkg, ArtifactName: "app"}).
		File("app")
}

// TestPackage runs `go test ./...` over a source tree and returns the
// combined output, so a failing package shows up as text you can read rather
// than an opaque exit code:
//
//	dagger call test-package
//
// go always passes -count=1, which disables Go's own test cache: inside a
// pipeline you want the tests to actually execute, not replay a previous
// "ok (cached)" line. Set race to add the data-race detector, the same knob
// the Ci builder's WithTest(race) flips.
func (m *GoExamples) TestPackage(
	ctx context.Context,
	// The Go source tree to test. Defaults to the built-in sample module.
	//
	// +optional
	source *dagger.Directory,
	// The package pattern to test.
	//
	// +default="./..."
	pkg string,
	// Enable the data-race detector (`go test -race`).
	//
	// +default=false
	race bool,
) (string, error) {
	return dag.Go().Test(ctx, sourceOrSample(source), dagger.GoTestOpts{Pkg: pkg, Race: race})
}

// ModuleHygiene runs the three housekeeping checks a Go repo wants before it
// commits -- gofmt, go vet, go mod tidy -- and returns the tidied source
// tree:
//
//	dagger call module-hygiene export --path ./tidied
//
// The ordering is the lesson. Fmt and Vet are gates: each returns an error
// the moment it finds a problem, so an unformatted file or a bad Printf verb
// stops the recipe before anything is rewritten. Only once both pass does
// ModTidy run and hand back /src, which is why the exported directory is
// always a tree that already passed its checks.
//
// Fmt also returns the gofmt diff alongside its error. That output is the
// useful half of the failure, so it is folded into the error message here
// instead of being dropped.
func (m *GoExamples) ModuleHygiene(
	ctx context.Context,
	// The Go source tree to check and tidy. Defaults to the built-in sample
	// module.
	//
	// +optional
	source *dagger.Directory,
) (*dagger.Directory, error) {
	src := sourceOrSample(source)

	diff, err := dag.Go().Fmt(ctx, src)
	if err != nil {
		return nil, fmt.Errorf("gofmt: %w (diff: %s)", err, diff)
	}

	if err := dag.Go().Vet(ctx, src); err != nil {
		return nil, fmt.Errorf("go vet: %w", err)
	}

	return dag.Go().ModTidy(src), nil
}

// InstallTool `go install`s a command-line tool and returns the compiled
// binary as a *dagger.File, ready to mount into any other container:
//
//	dagger call install-tool export --path ./yamlfmt
//
// This is how you get a Go-distributed tool into a pipeline without building
// an image for it. The returned file is named after the package's last path
// segment (`yamlfmt`), matching go install's own naming rules, and the
// install runs in a source-less container -- so the same tool installed by
// several stages resolves to one shared build.
//
// pkg must pin an explicit version. go rejects `@latest` and bare paths,
// because Install is cached for the session: without a pin the proxy could
// resolve a different version on a later call and the pipeline would
// silently keep serving the first binary it happened to build.
func (m *GoExamples) InstallTool(
	// The tool package to install, pinned to an explicit version.
	//
	// +default="github.com/google/yamlfmt/cmd/yamlfmt@v0.21.0"
	pkg string,
) *dagger.File {
	return dag.Go().Install(pkg)
}
