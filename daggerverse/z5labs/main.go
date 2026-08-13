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
// # Verifying a published image, and finding what is attached to it
//
// A publish leaves four kinds of thing on the registry: the image, a cosign
// signature over every manifest beneath the tag, an SPDX and a CycloneDX
// SBOM per platform, and one signed SLSA provenance statement. The
// signature and the documents are discovered two different ways, and which
// way is not something an adopter can guess by looking at the image.
//
// The signature is written in cosign's own layout — a `sha256-<hex>.sig`
// tag beside each digest it signs — so checking it is a command a consumer
// already has:
//
//	cosign verify ghcr.io/<owner>/<app>:<version> \
//	  --certificate-identity-regexp '^https://github.com/<owner>/<repo>/\.github/workflows/' \
//	  --certificate-oidc-issuer https://token.actions.githubusercontent.com
//
// The documents are not. They are attached as OCI referrers of the
// published digest, and by referrers alone: this module writes no
// `sha256-<hex>.att` tag, so cosign's matching command finds nothing.
// Against an image published here,
//
//	cosign verify-attestation ghcr.io/<owner>/<app>:<version> --type slsaprovenance1 ...
//
// exits 1 with
//
//	Error: no matching attestations:
//
// and that message is why this section exists. It reads as "this image has
// no attestations" and it means "cosign looked under the tag convention and
// they are not there" — a distinction nothing in the output draws, so an
// adopter who runs the natural first command and stops concludes the
// publish attached nothing.
//
// # Listing and reading the attached documents
//
// Any client that speaks the referrers API finds them, and needs no flag to
// do it against a registry that implements no referrers API: GHCR answers
// 404 there and oras-go takes the referrers-tag-scheme fallback by itself.
// Measured against ghcr.io with oras v1.2.3 on 2026-08-13,
//
//	oras discover ghcr.io/<owner>/<app>:<version>
//
// lists everything attached to the release. A document is selected by its
// artifact type, which is what attaching them under distinct types is for:
// application/spdx+json and application/vnd.cyclonedx+json for the SBOMs,
// application/vnd.in-toto+json for the provenance. Each referrer manifest
// holds exactly one layer and that layer is the document, so reading one is
// a discover and two fetches:
//
//	repo=ghcr.io/<owner>/<app>
//	referrer=$(oras discover "$repo:<version>" \
//	  --artifact-type application/vnd.in-toto+json \
//	  --format json | jq -r '.manifests[0].digest')
//	layer=$(oras manifest fetch "$repo@$referrer" | jq -r '.layers[0].digest')
//	oras blob fetch "$repo@$layer" --output -
//
// There is one provenance statement per publish but one SBOM pair per
// platform, so each SBOM type has several referrers. They are told apart by
// the layer's org.opencontainers.image.title annotation —
// `<binary>-linux-amd64.spdx.json` and its CycloneDX and per-platform
// counterparts — which is `.layers[0].annotations` on the manifest fetched
// above, not something `oras discover` prints.
//
// What comes back for the provenance is a DSSE envelope, and it verifies on
// its own bytes: its signature carries cosign's `cert` extension field
// holding the Fulcio chain, so the identity that signed is recoverable from
// the envelope without fetching anything beside it.
//
// The two checks are one verification because they are anchored to one
// digest. Verify the signature, take the digest cosign resolved, and list
// the referrers of that digest — the documents that come back are attached
// to the bytes whose signature was just checked. Listing the referrers of
// the tag instead checks documents against a mutable name.
//
// One asymmetry to know rather than discover: the image signature is
// recorded in the public transparency log and the provenance envelope is
// not, so the envelope's certificate cannot be independently placed inside
// its own validity window the way the signature's can. The signature is the
// identity anchor; the envelope is bound to it by being a referrer of a
// digest that anchor covers.
//
// # Why the documents are referrers alone
//
// Writing them a second time under cosign's `.att` tag would make
// `cosign verify-attestation` work, and it is rejected. A duplicate costs
// registry storage on every publish and leaves two copies of one document
// with nothing downstream able to say which is right once they disagree.
//
// Which raises the obvious question, since the signature does copy cosign's
// tag layout rather than being written as a referrer. The layout decides
// different things in the two cases. A cosign signature means nothing
// outside cosign's layout: the signed payload is a layer and the signature
// sits in annotations beside it, in a shape cosign alone parses, so
// publishing it any other way means publishing a verifier alongside it —
// sign.go records the same reasoning from the signature's side. A DSSE
// envelope carries its own payload type, its own signature and its own
// certificate, so whatever can fetch the bytes can check them. For the
// signature the layout therefore decides whether verification is possible
// at all; for the documents it decides only how they are found. Where only
// discovery is at stake, the mechanism built for discovery wins over the
// workaround for registries that had none.
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
