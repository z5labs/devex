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
// module is three delegations and nothing else.
package main

import (
	"context"

	"dagger/ci/internal/dagger"
)

// selfTestProbeModule is the module GeneratedSelfTest deliberately makes stale.
// Left to itself workspace-ci probes the first dependency-free module in the
// workspace; naming the smallest one keeps the check's cost fixed as the
// daggerverse grows and alphabetical order moves underneath it.
const selfTestProbeModule = "daggerverse/random"

// Ci holds no state: every check it declares is workspace-ci's, invoked against
// the calling workspace.
type Ci struct{}

// Generated verifies that every committed dagger.gen.go and
// internal/dagger/*.gen.go in the workspace matches what `dagger develop`
// produces at the pinned engineVersion, naming each stale module and printing
// its patch.
//
// It is declared here rather than left to daggerverse/workspace-ci's own checks
// because only the root module's checks run for every change. That is also what
// lets a generated file stay out of the memoization hash: this check proves such
// a file is derived from inputs that are in it, which is worth nothing unless it
// has run. See daggerverse/workspace-ci/README.md.
//
// +check
// +cache="never"
func (ci *Ci) Generated(ctx context.Context) error {
	return dag.WorkspaceCi().Generated(ctx)
}

// GeneratedSelfTest proves Generated can actually fail: it runs the same
// comparison against one module twice, pristine and then deliberately made
// stale, and fails unless the stale copy is reported.
//
// The check this was extracted from silently verified nothing for months (#184),
// so a green Generated is only worth as much as the proof that a stale module
// turns it red.
//
// +check
// +cache="never"
func (ci *Ci) GeneratedSelfTest(ctx context.Context) error {
	return dag.WorkspaceCi().GeneratedSelfTest(ctx, dagger.WorkspaceCiGeneratedSelfTestOpts{
		ProbeModule: selfTestProbeModule,
	})
}

// SelectionSelfTest runs the change -> modules -> legs mapping, and the
// properties a recorded pass depends on, against workspace-ci's fixed fixtures.
//
// A regression there under-runs this repository's CI silently, so it is checked
// on every change rather than only when the planner itself is edited. It needs
// no engine and no services, which is what makes that affordable.
//
// +check
func (ci *Ci) SelectionSelfTest(ctx context.Context) error {
	return dag.WorkspaceCi().SelectionSelfTest(ctx)
}
