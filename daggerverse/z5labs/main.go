// Package main implements the z5labs daggerverse module: opinionated CI
// and release pipelines for Go projects. Construct via the GoApp or GoLib
// factories on Z5labs; call the terminal Ci method to run the pipeline.
package main

import (
	_ "embed"

	"dagger/z-5-labs/internal/dagger"
)

//go:embed configs/golangci.yml
var defaultLintConfig []byte

// Z5labs is the root module type. Construct project archetypes via
// GoApp / GoLib.
type Z5labs struct{}

// GoApp wires up an opinionated CI/release pipeline for a `package main`
// Go application. Call Ci to run checks + multi-arch build + conditional
// publish, or Builder to produce the same image single-arch locally.
//
// Every binary GoApp builds is stamped at link time with the version and
// the commit it was built from, so an application can answer "which build
// am I running" without a second build definition beside this one. Declare
// these two package-level vars in your main package and they are filled in:
//
//	var (
//		version = "dev"
//		commit  = "none"
//	)
//
// The names are fixed by the module — `main.version` and `main.commit` —
// and the values are taken from HEAD, never from a parameter. A tag
// pointing at HEAD gives version the stripped tag name; anything else
// gives "<shortSha>-<isoCommitTime>", the same rule the published image
// tag follows, so the two agree by construction. commit is the short HEAD
// SHA. Because both are functions of the commit alone, two builds of one
// commit are byte-identical; there is no caller-supplied value that could
// break that. Source without git metadata at HEAD is an error.
//
// publishOn is a regex evaluated against source repo's HEAD refs (after
// normalizing `refs/remotes/origin/X` → `refs/heads/X`); matches trigger
// publish. When registry is set, auth is required.
//
// platforms defaults to ["linux/amd64","linux/arm64"].
//
// registryService, when non-nil, is bound as the "registry" alias on the
// publishing container — used by tests against a local registry:2
// service and by callers whose private registry is itself a Dagger
// service.
func (m *Z5labs) GoApp(
	source *dagger.Directory,
	// +optional
	// +default="."
	pkg string,
	// +optional
	binaryName string,
	// +optional
	// +default="^refs/heads/main$"
	publishOn string,
	// +optional
	registry string,
	// +optional
	// +default="ci"
	authUsername string,
	// +optional
	auth *dagger.Secret,
	// +optional
	lintConfig *dagger.File,
	// +optional
	platforms []string,
	// +optional
	registryService *dagger.Service,
) *GoApp {
	if len(platforms) == 0 {
		platforms = []string{"linux/amd64", "linux/arm64"}
	}
	return &GoApp{
		Source:          source,
		Pkg:             pkg,
		BinaryName:      binaryName,
		PublishOn:       publishOn,
		Registry:        registry,
		AuthUsername:    authUsername,
		Auth:            auth,
		LintConfig:      lintConfig,
		Platforms:       platforms,
		RegistryService: registryService,
	}
}

// GoLib wires up the checks-only pipeline for a Go library. v1 has no
// publish equivalent for libraries.
func (m *Z5labs) GoLib(
	source *dagger.Directory,
	// +optional
	lintConfig *dagger.File,
) *GoLib {
	return &GoLib{Source: source, LintConfig: lintConfig}
}
