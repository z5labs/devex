// Package main implements the bruno Dagger module: a wrapper around `bru`,
// the Bruno CLI, so an API collection stops reaching CI as a hand-rolled step
// — a Node install or a `docker run` with the volume mount spelled correctly,
// the environment name passed through, and a wrapper script to turn the exit
// code into something a pipeline understands — and becomes a `dagger call`.
//
// Upstream publishes `usebruno/cli` as a Docker Verified Publisher image on
// both Docker Hub and ghcr.io, so unlike tesseract or opentofu this module
// pins a vendor image rather than assembling one. That image's entrypoint is
// `bru`, its working directory is /bruno, and it runs as the non-root `node`
// user (UID 1000) — which is why reports are staged in a module-owned writable
// directory instead of being written into the mounted collection.
//
// The boundary input is a *dagger.Directory, not a lone *dagger.File: `bru`
// exits 4 when invoked outside a collection root, and resolves environments/
// and .env relative to it.
package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/bruno/internal/dagger"
)

const (
	// brunoImagePath is the repository under the configured registry.
	brunoImagePath  = "usebruno/cli"
	defaultRegistry = "docker.io"
	defaultVersion  = "3.4.2"

	// debianSuffix selects the Debian (node:22-slim) image variant. The
	// default image is Alpine, whose musl/OpenSSL build fails against some
	// TLS endpoints; the Debian variant exists specifically for those.
	debianSuffix = "-debian"

	// collectionDir is where the caller's collection is mounted. It is the
	// image's own WORKDIR, and it is root-owned while the container runs as
	// UID 1000 — so nothing this module does writes into it.
	collectionDir = "/bruno"
)

// Bruno wraps the Bruno CLI as Dagger functions. Construct via New(); call
// Container() for the raw image, or Collection(source) to bind a collection
// and reach Run/Report.
type Bruno struct {
	// +private
	Registry string
	// +private
	Tag string
	// +private
	Debian bool
}

// New returns a Bruno module backed by <registry>/usebruno/cli:<version>.
func New(
	// Container registry hosting the usebruno/cli image. Upstream publishes
	// the same image to docker.io and ghcr.io.
	// +default="docker.io"
	registry string,
	// Image tag for usebruno/cli, which is the Bruno CLI release it ships.
	// +default="3.4.2"
	version string,
	// Select the Debian variant (node:22-slim) of the image. The suffix is
	// appended to version, so debian=true with the default 3.4.2 resolves
	// 3.4.2-debian. The Alpine default hits musl/OpenSSL failures against
	// some TLS endpoints, which is what this variant is for.
	// +default=false
	debian bool,
) *Bruno {
	if registry == "" {
		registry = defaultRegistry
	}
	if version == "" {
		version = defaultVersion
	}
	return &Bruno{Registry: registry, Tag: version, Debian: debian}
}

// Container returns the bare Bruno CLI image. This is the escape hatch for
// every flag this module does not wrap — `bru`'s long tail of proxy and cookie
// options stays reachable via `container with-exec`.
//
// +cache="session"
func (b *Bruno) Container() *dagger.Container {
	return dag.Container().From(b.image())
}

// Version returns the Bruno CLI release the pinned image ships, as reported
// by `bru --version`.
//
// +cache="session"
func (b *Bruno) Version(ctx context.Context) (string, error) {
	out, err := b.Container().
		WithExec([]string{"bru", "--version"}).
		Stdout(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (b *Bruno) image() string {
	return fmt.Sprintf("%s/%s:%s", b.Registry, brunoImagePath, b.resolvedTag())
}

// resolvedTag applies the -debian suffix when debian is set, unless the tag
// already carries it (so New(version: "3.4.2-debian") and New(debian: true)
// agree).
func (b *Bruno) resolvedTag() string {
	if b.Debian && !strings.HasSuffix(b.Tag, debianSuffix) {
		return b.Tag + debianSuffix
	}
	return b.Tag
}
