// Package main implements the opentofu Dagger module: a wrapper around the
// `tofu` CLI, so an infrastructure repo's format, validate, plan, apply and
// destroy steps become `dagger call`s instead of a wrapper script plus a pile
// of exported environment variables.
//
// It targets OpenTofu (MPL-2.0) rather than Terraform (BUSL since 1.6), and
// deliberately does not try to also drive a `terraform` binary.
//
// OpenTofu stopped supporting direct use of its official image as of 1.10.
// What upstream still publishes is `ghcr.io/opentofu/opentofu:<version>-minimal`,
// an image containing only /usr/local/bin/tofu. This module therefore assembles
// its own container: the binary is copied off the -minimal image onto a small
// base that also carries git and CA certificates, which module sources and the
// provider registry need.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"dagger/opentofu/internal/dagger"
)

const (
	// opentofuImagePath is the repository under the configured registry that
	// publishes the -minimal images.
	opentofuImagePath = "opentofu/opentofu"

	defaultRegistry = "ghcr.io"
	defaultVersion  = "1.12.5"
	defaultBase     = "alpine:3.22"

	// minimalSuffix selects the only image variant upstream still means for
	// consumption: a filesystem holding just the tofu binary.
	minimalSuffix = "-minimal"

	// tofuBinPath is where the -minimal image keeps the binary, and where it
	// is placed on the assembled container.
	tofuBinPath = "/usr/local/bin/tofu"
)

// Opentofu wraps the tofu CLI as Dagger functions. Construct via New(); call
// Container() for the assembled image, or Config(source) to bind a root
// module and reach the lifecycle functions.
type Opentofu struct {
	// +private
	Registry string
	// +private
	Tag string
	// +private
	Base *dagger.Container
}

// New returns an Opentofu module whose tofu binary comes from
// <registry>/opentofu/opentofu:<version>-minimal, placed onto base.
func New(
	// Container registry hosting the opentofu/opentofu image.
	// +default="ghcr.io"
	registry string,
	// OpenTofu release to install. The -minimal suffix is appended when
	// absent, so "1.12.5" and "1.12.5-minimal" select the same image.
	// +default="1.12.5"
	version string,
	// Base image the binary is copied onto. It must be Alpine-family: the
	// module runs `apk add git ca-certificates` on it, because module sources
	// (git) and the provider registry (TLS) need both and the -minimal image
	// carries neither.
	// +default="alpine:3.22"
	base string,
) *Opentofu {
	if registry == "" {
		registry = defaultRegistry
	}
	if version == "" {
		version = defaultVersion
	}
	if base == "" {
		base = defaultBase
	}
	return &Opentofu{
		Registry: registry,
		Tag:      resolveTag(version),
		Base:     dag.Container().From(base),
	}
}

// Container returns the assembled tofu image: the base plus git, CA
// certificates and the tofu binary. This is the escape hatch for every
// subcommand this module does not wrap — `container with-exec` keeps tofu's
// long tail reachable.
//
// +cache="session"
func (o *Opentofu) Container() *dagger.Container {
	return o.Base.
		WithoutEntrypoint().
		WithExec([]string{"apk", "add", "--no-cache", "git", "ca-certificates"}).
		WithFile(
			tofuBinPath,
			dag.Container().From(o.image()).File(tofuBinPath),
			dagger.ContainerWithFileOpts{Permissions: 0o755},
		).
		// Tells tofu it is running unattended: it drops the "run this next"
		// suggestions that assume an interactive terminal.
		WithEnvVariable("TF_IN_AUTOMATION", "1")
}

// Version returns the OpenTofu release the assembled container ships, as
// reported by `tofu version` — the first line only, so the trailing
// `on <os>_<arch>` line stays out of the value.
//
// +cache="session"
func (o *Opentofu) Version(ctx context.Context) (string, error) {
	out, err := o.Container().
		WithExec([]string{"tofu", "version"}).
		Stdout(ctx)
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(line), nil
}

// Config binds a root module directory to the toolchain. source is the whole
// tree, not a lone file: tofu resolves sub-modules, .tfvars files and the
// dependency lock file relative to the root module.
func (o *Opentofu) Config(source *dagger.Directory) *Config {
	return &Config{Opentofu: o, Source: source}
}

func (o *Opentofu) image() string {
	return fmt.Sprintf("%s/%s:%s", o.Registry, opentofuImagePath, o.Tag)
}

// resolveTag appends -minimal unless the caller already spelled it out, so
// New(version: "1.12.5") and New(version: "1.12.5-minimal") agree.
func resolveTag(version string) string {
	if strings.HasSuffix(version, minimalSuffix) {
		return version
	}
	return version + minimalSuffix
}

// randHex returns 32 hex chars of cryptographically-random data for use as a
// cache-busting nonce. The +cache="never" directive governs a *function's*
// result; the WithExec layers that function builds are still content-addressed,
// so an operation that must genuinely re-run needs a per-call unique env var.
func randHex() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// combinedOutput joins a finished exec's stdout and stderr. tofu splits its
// diagnostics onto stderr and its progress onto stdout, so an error message
// built from either stream alone drops half of what went wrong.
func combinedOutput(ctx context.Context, exec *dagger.Container) string {
	stdout, _ := exec.Stdout(ctx)
	stderr, _ := exec.Stderr(ctx)
	return strings.TrimSpace(strings.TrimSpace(stdout) + "\n" + strings.TrimSpace(stderr))
}
