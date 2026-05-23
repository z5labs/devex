// Ci is the root module that aggregates each daggerverse module's tests
// suite as a toolchain. With toolchains, `dagger check -l` enumerates
// every dep's +check functions directly (e.g. kafka-tests:all), so no
// wrapper methods are needed here.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"dagger/ci/internal/dagger"
)

type Ci struct{}

// Verify that committed dagger.gen.go and internal/dagger/*.gen.go files
// match what `dagger develop` would produce at the pinned engineVersion.
//
// +check
func (ci *Ci) Generated(ctx context.Context, ws *dagger.Workspace) error {
	generated := ws.Generators().Run()
	empty, err := generated.IsEmpty(ctx)
	if err != nil {
		return err
	}
	if empty {
		return nil
	}
	patch, err := generated.Changes().AsPatch().Contents(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, patch)
	return errors.New("generated files are not up-to-date; run `dagger develop` in the affected module(s)")
}
