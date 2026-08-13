package main

import (
	"context"
	"strings"

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
// tags have no effect on it.
//
// tags are passed to the Go toolchain as `-tags a,b,c`, selecting which
// `//go:build`-constrained files compile. The build belongs to App — the
// terminal that produces container images — and these are the tags it
// builds every platform's binary with.
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

// App builds the application at pkg for every platform and returns it.
//
// One binary is cross-compiled per platform, stamped at link time with
// version and with the commit; each is packaged as an image carrying the
// module's standardized environment, the absolute entrypoint and the OCI
// source annotations; and an SPDX and a CycloneDX document are generated
// for each binary. What comes back holds the images and those documents
// and knows nothing about the chain that produced them — see App.
//
// # version is the caller's, and is validated here
//
// The version was a pure function of HEAD before this chain existed, which
// suits a high-frequency install with no semantic versioning and does not
// suit a project releasing on semver. It is now stated by whoever is
// releasing, and the only thing this module has an opinion about is that it
// can be an image tag: an OCI tag is `[A-Za-z0-9_][A-Za-z0-9._-]{0,127}`,
// and a version outside that charset is refused rather than rewritten.
//
// SemVer build metadata — the `+` and everything after it — is called out
// separately when it is refused, because it is the case where rewriting
// would silently do damage: `+` is not in the tag charset, so dropping it
// would publish `1.0.0+build.1` and `1.0.0+build.2` under one tag, and the
// second would quietly replace the first.
//
// # commit still comes from HEAD and from nothing else
//
// Every binary is stamped with `main.version` and `main.commit`. Declare
// the two package-level vars in your main package and they are filled in:
//
//	var (
//		version = "dev"
//		commit  = "none"
//	)
//
// The names are fixed by the module. commit is the short HEAD SHA and is
// never a parameter — a build identity a caller could have supplied
// identifies nothing — so two builds of one (commit, version) pair are
// byte-identical. Source without git metadata at HEAD is an error.
//
// pkg is the package to build, in `go build` package syntax, relative to
// the source root. platforms defaults to linux/amd64 and linux/arm64.
//
// +cache="session"
func (g *GoChain) App(
	ctx context.Context,
	// The version every binary is stamped with and every image is
	// published under. Any OCI-tag-safe string; SemVer build metadata is
	// refused.
	version string,
	// The package to build, in go build package syntax, relative to the
	// source root.
	//
	// +optional
	// +default="."
	pkg string,
	// The platforms to build for, e.g. linux/amd64. Empty takes the
	// pipeline's pair, linux/amd64 and linux/arm64.
	//
	// +optional
	platforms []dagger.Platform,
) (*App, error) {
	if err := validateVersion(version); err != nil {
		return nil, err
	}
	if pkg == "" {
		pkg = "."
	}
	if len(platforms) == 0 {
		platforms = defaultPlatforms()
	}
	for _, platform := range platforms {
		if _, _, err := parsePlatform(string(platform)); err != nil {
			return nil, err
		}
	}
	if err := requireGitWorkingTree(ctx, g.Source); err != nil {
		return nil, err
	}
	facts, err := g.gitFacts(ctx)
	if err != nil {
		return nil, err
	}
	binaryName, err := g.resolvedBinaryName(ctx)
	if err != nil {
		return nil, err
	}
	annotations := ociAnnotations(facts, version)

	variants := make([]*variant, 0, len(platforms))
	for _, platform := range platforms {
		binary := g.buildBinaryForPlatform(string(platform), pkg, binaryName, version, facts.ShortSHA)
		// The SBOMs are generated here, by the chain, because this is the
		// last place that holds both the binary and the source they need.
		// App carries what comes back as an opaque file and an artifact
		// type, and the publish attaches it without ever learning it is an
		// SBOM. Nothing is evaluated yet: dag.Go().Spdx returns a lazy
		// *dagger.File, so an app nobody publishes costs no scan.
		stem := binaryName + "-" + strings.ReplaceAll(string(platform), "/", "-")
		variants = append(variants, &variant{
			Platform:  platform,
			Container: imageForPlatform(platform, binaryName, binary, annotations),
			Documents: []document{
				{Name: stem + ".spdx.json", Type: spdxArtifactType, File: renamed(dag.Go().Spdx(binary, g.Source), stem+".spdx.json")},
				{Name: stem + ".cdx.json", Type: cycloneDxArtifactType, File: renamed(dag.Go().CycloneDx(binary, g.Source), stem+".cdx.json")},
			},
		})
	}
	return &App{
		Version:   version,
		Commit:    facts.SHA,
		SourceURI: annotations[annotationSource],
		Pkg:       pkg,
		Variants:  variants,
	}, nil
}

// defaultPlatforms is the pair every z5labs application is built for
// unless the caller narrows or widens it. It is a property of the pipeline
// rather than of an application, which is why it is here and not a field.
func defaultPlatforms() []dagger.Platform {
	return []dagger.Platform{"linux/amd64", "linux/arm64"}
}
