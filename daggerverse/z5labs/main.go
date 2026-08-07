// Package main implements the z5labs daggerverse module: opinionated CI
// and release pipelines for Go projects. Construct via the GoApp or GoLib
// factories on Z5labs; call the terminal Ci method to run the pipeline.
//
// The lint stage runs golangci-lint v2. A repository adopting this pipeline
// keeps its `.golangci.yml` in the v2 dialect — the file opens with
// `version: "2"` — because a v2 binary refuses a v1 file outright rather
// than ignoring it. Supplying no file at all takes the bundled policy in
// configs/golangci.yml, which is the v2 file to copy from. The release is
// pinned once, in the `go` module; lintVersion moves it, either major.
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
// registryService, when non-nil, is a Dagger-hosted registry reached over
// the session network instead of over the public network — used by tests
// against a local registry service and by callers whose private registry
// is itself a Dagger service. Its endpoint is assigned by the engine, so
// it replaces registry as the address published to; registry is still
// what decides that a publish happens at all.
//
// insecure means plain HTTP and no TLS verification, and it is off unless
// the caller asks for it. It is deliberately not inferred from
// registryService being set: that inference made a caller who supplied a
// service for their own reasons silently publish over an unverified
// connection. It is spelled insecure rather than tlsVerify because a bool
// defaulting to true cannot be turned off from the CLI.
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
	// A `.golangci.yml` replacing the bundled default policy.
	//
	// The lint stage runs golangci-lint v2, so this file must be written
	// in the v2 dialect — it has to open with `version: "2"`. The majors
	// are not interchangeable: a v2 binary refuses a v1 file outright,
	// before running any linter. Pass lintVersion to move the whole stage,
	// dialect included, to another release.
	//
	// +optional
	lintConfig *dagger.File,
	// The golangci-lint release the lint stage installs, e.g. "v2.12.2".
	// Empty takes the version pinned by the `go` module, which is a v2
	// release; pinning a "v1.x" release here rolls the stage — and the
	// config dialect it requires — back to v1.
	//
	// +optional
	lintVersion string,
	// +optional
	platforms []string,
	// +optional
	registryService *dagger.Service,
	// +optional
	insecure bool,
	// The CI provider's OIDC token request endpoint —
	// `ACTIONS_ID_TOKEN_REQUEST_URL` on GitHub Actions, and whatever the
	// equivalent is elsewhere. Required for any run that publishes.
	//
	// +optional
	idTokenRequestUrl string,
	// The bearer token for that endpoint —
	// `ACTIONS_ID_TOKEN_REQUEST_TOKEN` on GitHub Actions. A secret,
	// because it is a credential for minting identity tokens. Required
	// for any run that publishes.
	//
	// +optional
	idTokenRequestToken *dagger.Secret,
	// A Dagger-hosted OIDC token endpoint, reached over the session
	// network instead of the public one. When set, its engine-assigned
	// endpoint replaces the host in idTokenRequestUrl; the path and query
	// stay the caller's, because those are part of the provider's
	// protocol.
	//
	// This exists for the same reason registryService does: a service's
	// address is not known until the engine assigns one, so it cannot be
	// written into a URL ahead of time. It is used by the test suite,
	// which runs a real token endpoint, and by anyone whose issuer is
	// itself a Dagger service.
	//
	// +optional
	idTokenService *dagger.Service,
	// A PEM-encoded EC private key to sign the provenance with, instead
	// of an ephemeral key certified by the public sigstore CA.
	//
	// This selects the signing mode and nothing else: the workload
	// identity token is still exchanged, and the predicate still says
	// only what that token's claims say. Use it for a build that cannot
	// reach a public CA. Leaving it unset is keyless signing and is what
	// a normal CI publish should do.
	//
	// +optional
	signingKey *dagger.Secret,
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
		LintVersion:     lintVersion,
		Platforms:       platforms,
		RegistryService: registryService,
		Insecure:        insecure,

		IDTokenRequestURL:   idTokenRequestUrl,
		IDTokenRequestToken: idTokenRequestToken,
		IDTokenService:      idTokenService,
		SigningKey:          signingKey,
	}
}

// GoLib wires up the checks-only pipeline for a Go library. v1 has no
// publish equivalent for libraries.
func (m *Z5labs) GoLib(
	source *dagger.Directory,
	// A `.golangci.yml` replacing the bundled default policy.
	//
	// The lint stage runs golangci-lint v2, so this file must be written
	// in the v2 dialect — it has to open with `version: "2"`. The majors
	// are not interchangeable: a v2 binary refuses a v1 file outright,
	// before running any linter. Pass lintVersion to move the whole stage,
	// dialect included, to another release.
	//
	// +optional
	lintConfig *dagger.File,
	// The golangci-lint release the lint stage installs, e.g. "v2.12.2".
	// Empty takes the version pinned by the `go` module, which is a v2
	// release; pinning a "v1.x" release here rolls the stage — and the
	// config dialect it requires — back to v1.
	//
	// +optional
	lintVersion string,
) *GoLib {
	return &GoLib{Source: source, LintConfig: lintConfig, LintVersion: lintVersion}
}
