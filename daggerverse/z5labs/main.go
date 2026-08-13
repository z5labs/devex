// Package main implements the z5labs daggerverse module: opinionated CI
// and release pipelines for Go projects.
//
// There is one entry point, Z5labs.Go, and everything hangs off it. The
// chain it returns carries the standardized checks over a source tree and
// its terminal Ci runs them; its other terminal, App, cross-compiles the
// application, packages one image per platform and hands back an App whose
// Publish pushes them. A library is a source tree you never call App on,
// which is why there is no library archetype.
//
// # The image contract
//
// Every image this module builds carries the same environment, and it is
// exactly one variable:
//
//	PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
//
// with /usr/local/bin as the directory an extension's executables land in.
// Both values are fixed by the module and no caller-facing method can move
// either, because a published image is something other people write `FROM`
// and `COPY` lines against: a PATH that varied per app would make "put your
// plugin on the PATH" a per-image question, and moving the directory later
// would break every line already written. The value is the conventional
// default a container runtime injects when an image sets none, so an image
// that later gains a real base layer behaves the way its base expects.
//
// The application's own entrypoint does not rely on any of that. It is an
// absolute path — /app/<binary> — so the app runs whatever the PATH says.
// PATH exists for what an extension adds, not for finding the app itself.
//
// # Versions
//
// The version is the caller's, passed to App and validated there: it has to
// be an OCI-tag-safe string, and SemVer build metadata is refused rather
// than mangled. See GoChain.App.
//
// # Lint
//
// The lint stage runs golangci-lint v2. A repository adopting this pipeline
// keeps its `.golangci.yml` in the v2 dialect — the file opens with
// `version: "2"` — because a v2 binary refuses a v1 file outright rather
// than ignoring it. Supplying no file at all takes the bundled policy in
// configs/golangci.yml, which is the v2 file to copy from. The release is
// pinned once, in the `go` module; GoChain.WithLint's version moves it,
// either major.
package main

import (
	_ "embed"

	"dagger/z-5-labs/internal/dagger"
)

//go:embed configs/golangci.yml
var defaultLintConfig []byte

// Z5labs is the root module type. Construct the Go language chain via Go;
// everything this module does is reached from there.
type Z5labs struct{}

// Go returns the Go language chain bound to source: the standardized
// checks — gofmt, go vet, golangci-lint and `go test -race` — over a Go
// source tree, configured by the chain's With* methods and run by its
// terminal Ci, plus the build terminal App.
//
// This is what a Go library needs and all it needs, which is why there is
// no library archetype: a library is a source tree you never call App on.
// An application starts here too; the build and the publish sit downstream
// of this chain rather than beside it.
//
// The returned object is GoChain rather than Go because the `go` module
// this one depends on already owns that name — see GoChain's doc comment.
//
// +cache="session"
func (m *Z5labs) Go(source *dagger.Directory) *GoChain {
	return &GoChain{Source: source}
}
