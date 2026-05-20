package main

import (
	"context"
	"runtime"

	"dagger/z-5-labs/internal/dagger"
)

// Builder produces the same image GoApp.Ci would publish, single-arch
// (host platform). Used for local development to verify the artifact
// before pushing.
type Builder struct {
	// +private
	App *GoApp
}

// Container returns the host-platform scratch image containing the
// compiled binary at /app/<binaryName> with that path as entrypoint.
//
// +cache="session"
func (b *Builder) Container(ctx context.Context) (*dagger.Container, error) {
	binaryName, err := b.App.resolvedBinaryName(ctx)
	if err != nil {
		return nil, err
	}
	plat := hostPlatform()
	bin, err := b.App.buildBinaryForPlatform(ctx, plat, binaryName)
	if err != nil {
		return nil, err
	}
	return b.App.imageForPlatform(plat, binaryName, bin), nil
}

// Binary returns the host-platform compiled binary as a *dagger.File.
//
// +cache="session"
func (b *Builder) Binary(ctx context.Context) (*dagger.File, error) {
	binaryName, err := b.App.resolvedBinaryName(ctx)
	if err != nil {
		return nil, err
	}
	return b.App.buildBinaryForPlatform(ctx, hostPlatform(), binaryName)
}

// hostPlatform returns "linux/<runtime.GOARCH>". Dagger module code runs
// in a linux container regardless of the user's host OS, so the OS
// component is always "linux"; the arch reflects the engine container.
func hostPlatform() string {
	return "linux/" + runtime.GOARCH
}
