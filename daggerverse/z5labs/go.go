package main

import (
	"context"

	"dagger/z-5-labs/internal/dagger"
)

// GoChain is the Go language chain: a source tree, and the standardized
// checks over it. Construct via Z5labs.Go and call the terminal Ci.
//
// The type is not named Go, which is what the design called for, because
// this module depends on the `go` module and that module's root object is
// literally Go. A dependency's objects occupy their bare names in the
// dependent module's type space, so a local object of the same name is
// resolved as the dependency's and the module fails to load:
//
//	failed to add object to module "z5labs": failed to validate type def:
//	object "Z5labs" function "Go" cannot return external type from
//	dependency module "go"
//
// The *method* is unaffected — Z5labs.Go is the constructor and the CLI
// path is still `dagger call go ...` — so the collision costs the type's
// spelling and nothing else. See daggerverse/CLAUDE.md.
//
// The stages are not opt-in. Ci always runs fmt, vet, lint and test, which
// is what makes "the z5labs pipeline ran" mean the same thing in every
// repository that adopts it — the same guarantee the archetype factories
// gave before this chain replaced them. The With* methods configure those
// stages; none of them switches one on or off. A caller who wants a subset
// is describing a different pipeline and should reach for the `go` module's
// own Ci builder, which is exactly that: stages enabled one by one.
//
// A library is a Go you never build an application from, so there is no
// separate library archetype.
type GoChain struct {
	// +private
	Source *dagger.Directory
	// +private
	LintConfig *dagger.File
	// +private
	LintVersion string
	// NoRace carries the test stage's race-detector setting inverted, so
	// that the zero value is the safe one. The detector is on unless a
	// caller turns it off, and a field spelled the other way round would
	// make that guarantee a property of the constructor rather than of the
	// type: every future construction path — App, a literal in this
	// package — would have to remember to set it, and forgetting would
	// weaken the check with no compile error and no failing test.
	//
	// +private
	NoRace bool
	// +private
	BuildTags []string
}

// WithLint configures the lint stage.
//
// version is the golangci-lint release the stage installs, e.g. "v2.12.2".
// Empty takes the version pinned by the `go` module, which is a v2 release;
// pinning a "v1.x" release rolls the whole stage — config dialect included
// — back to v1.
//
// config is a `.golangci.yml` replacing the bundled policy in
// configs/golangci.yml. It has to be written in the dialect the pinned
// major speaks: a v2 binary refuses a v1 file outright, before any linter
// runs, and v1 refuses a v2 file the same way.
//
// Both arguments are optional, because pinning a version and replacing the
// policy are independent decisions and requiring one to state the other
// would make every version pin also a policy fork. An argument left out
// leaves that setting alone rather than clearing it, so the independence
// holds across calls as well as within one: `with-lint --config=x
// with-lint --version=y` keeps both, where an unconditional assignment
// would have dropped the config and run the bundled policy the caller
// thought they had replaced. Nothing can un-set either one, which is not a
// use case — a caller who wants the defaults does not call this.
//
// +cache="session"
func (g *GoChain) WithLint(
	// +optional
	version string,
	// +optional
	config *dagger.File,
) *GoChain {
	if version != "" {
		g.LintVersion = version
	}
	if config != nil {
		g.LintConfig = config
	}
	return g
}

// WithTest configures the test stage. race turns the data-race detector on
// or off; it is on unless this method says otherwise.
//
// race is a required argument rather than an optional one, because an
// optional bool takes its zero value when the flag is absent, so a bare
// `with-test` would silently drop the race detector the pipeline otherwise
// guarantees. Turning it off is a real weakening of the check, so the
// caller states it.
//
// +cache="session"
func (g *GoChain) WithTest(race bool) *GoChain {
	g.NoRace = !race
	return g
}

// WithBuild records build tags for the App terminal. Ci does not build, so
// tags have no effect on it today.
//
// tags are passed to the Go toolchain as `-tags a,b,c`, selecting which
// `//go:build`-constrained files compile. The build belongs to App — the
// terminal that produces container images — which lands with the second
// half of this API, and these are the tags it will build with. The method
// is here now because the shape of the chain is what is being fixed; until
// App exists a caller who sets tags gets no error and no effect, which is
// why that is the first thing this comment says.
//
// +cache="session"
func (g *GoChain) WithBuild(tags []string) *GoChain {
	g.BuildTags = tags
	return g
}

// Ci runs the standardized check stages against the source: gofmt, go vet,
// golangci-lint against the bundled policy unless WithLint supplied one,
// and `go test ./...` with the race detector unless WithTest turned it off.
// The stages run in parallel and their errors are aggregated.
//
// +check
// +cache="session"
func (g *GoChain) Ci(ctx context.Context) error {
	return sharedCheck(ctx, g.Source, g.LintConfig, g.LintVersion, !g.NoRace)
}
