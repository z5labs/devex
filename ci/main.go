// Ci is the workspace's root module: the checks that must run for every change,
// whatever it touched.
//
// Planning, routing and memoization live in daggerverse/workspace-ci, which
// .github/workflows/change-aware-ci.yml calls directly — this module is not in
// that path, and
// no run leg loads it to reach another module's suite. What has to live here is
// only the set of checks a plan treats as global: workspace-ci always runs the
// root module's checks and never memoizes them, because they are the ones that
// read the workspace as a whole rather than any one module's closure. So this
// module is one delegation and nothing else.
package main

import (
	"context"
	"errors"

	"dagger/ci/internal/dagger"
)

// Ci holds no state: every check it declares is workspace-ci's, invoked against
// the calling workspace.
type Ci struct{}

// Generated verifies that every committed dagger.gen.go and
// internal/dagger/*.gen.go in the workspace matches what codegen produces at
// the pinned engineVersion, naming each stale module and printing its patch.
//
// It also proves, on every run, that it can fail and that it looked at every
// module: see daggerverse/workspace-ci's Generated. That is why this module no
// longer carries separate self-test checks of its own.
//
// It is declared here rather than left to daggerverse/workspace-ci's own checks
// because only the root module's checks run for every change. That is also what
// lets a generated file stay out of the memoization hash: this check proves such
// a file is derived from inputs that are in it, which is worth nothing unless it
// has run. See daggerverse/workspace-ci/README.md.
//
// The workspace is a parameter rather than something workspace-ci reaches for,
// because a module cannot reach for it: Dagger v1 marks the field experimental
// and leaves it out of the client a module is generated against. The CLI fills
// this in from the workspace the check was invoked in, and it is passed straight
// through.
//
// +check
// +cache="never"
func (ci *Ci) Generated(ctx context.Context, callingWorkspace *dagger.Workspace) error {
	return checkErr(ctx, dag.WorkspaceCi().Generated(callingWorkspace))
}

// checkErr runs a dependency's check and returns its failure as an error.
//
// Since Dagger v1.0.0-beta.15 a +check function reaches a consumer as a
// *dagger.Check, a deferred check, rather than as the error it returns, so the
// failure has to be read back off it.
func checkErr(ctx context.Context, check *dagger.Check) error {
	failure, err := check.Error(ctx)
	if err != nil {
		return err
	}
	if failure == nil {
		return nil
	}
	msg, err := failure.Message(ctx)
	if err != nil {
		return err
	}
	return errors.New(msg)
}
