package main

import (
	"context"
	"fmt"

	"dagger/tests/internal/dagger"
)

// trustedRef is the ref these tests nominate as writable, and untrustedRef one
// they never do. The second is deliberately an extension of the first: a store
// that keyed entries on a raw ref name would let it reach into the first's scope,
// so nothing about the trust boundary should be prefix-shaped either.
const (
	trustedRef   = "refs/heads/main"
	untrustedRef = "refs/heads/main-2"
	// someHash is shaped like a real input hash. Nothing here reaches a store that
	// could hold it.
	someHash = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	// someCommit is a well-formed object id, for the same reason.
	someCommit = "0123456789abcdef0123456789abcdef01234567"
	// unreachableAPI is a port nothing listens on, so a call that gets as far as
	// the store fails immediately and provably without leaving the engine.
	unreachableAPI = "http://127.0.0.1:1"
)

// recorder builds a planner configured against a module-owned store, with a
// throwaway credential and one trusted ref.
func recorder(store dagger.WorkspaceCiMemoStore, api string) (*dagger.WorkspaceCi, error) {
	token, err := randomSecret()
	if err != nil {
		return nil, err
	}
	return dag.WorkspaceCi(dagger.WorkspaceCiOpts{
		MemoStore: store,
		MemoToken: token,
		MemoRepo:  "z5labs/devex",
		MemoAPI:   api,
		MemoRefs:  []string{trustedRef},
	}), nil
}

// RecordPassRefusesAnUntrustedRef is the trust boundary for module-side writes.
// A run whose ref the caller never nominated must record nothing, and it must
// refuse before it reaches the store rather than relying on the store to say no —
// so this points it at an API that cannot answer, and still expects REFUSED.
func (t *Tests) RecordPassRefusesAnUntrustedRef(ctx context.Context) error {
	ci, err := recorder(dagger.WorkspaceCiMemoStoreGitRefs, unreachableAPI)
	if err != nil {
		return err
	}
	got, err := ci.RecordPass(ctx, someHash, untrustedRef, someCommit)
	if err != nil {
		return err
	}
	if got != "REFUSED" {
		return fmt.Errorf("recording from %q reported %q, want REFUSED", untrustedRef, got)
	}
	return nil
}

// RecordPassReachesTheStoreFromTrustedRef is the other half of the same
// boundary, and the reason the test above proves anything: from a nominated ref
// the module really does go on to write, so REFUSED is a judgement about the ref
// and not a planner that never writes at all. The store here cannot answer, so
// the outcome is FAILED — which is also the point of the next test.
func (t *Tests) RecordPassReachesTheStoreFromTrustedRef(ctx context.Context) error {
	ci, err := recorder(dagger.WorkspaceCiMemoStoreGitRefs, unreachableAPI)
	if err != nil {
		return err
	}
	got, err := ci.RecordPass(ctx, someHash, trustedRef, someCommit)
	if err != nil {
		return err
	}
	if got != "FAILED" {
		return fmt.Errorf("recording from %q reported %q, want FAILED against an unreachable store", trustedRef, got)
	}
	return nil
}

// RecordPassNeverFailsThePassingCheck pins the contract the whole function
// hangs on: recording runs after the work is already green, so a store that
// cannot be reached at all reports itself in the return value and never as an
// error. A store outage that turned a passing suite red would be strictly worse
// than no memoization.
func (t *Tests) RecordPassNeverFailsThePassingCheck(ctx context.Context) error {
	ci, err := recorder(dagger.WorkspaceCiMemoStoreGitRefs, unreachableAPI)
	if err != nil {
		return err
	}
	if _, err := ci.RecordPass(ctx, someHash, trustedRef, someCommit); err != nil {
		return fmt.Errorf("an unreachable store failed the call: %w", err)
	}
	return nil
}

// RecordPassSkipsAnUnhashableLeg proves the empty hash means the same thing on
// the write side as on the read side. A leg the planner could not hash must never
// have anything recorded for it — a hash of "" would otherwise become an entry
// every later unhashable leg matched.
func (t *Tests) RecordPassSkipsAnUnhashableLeg(ctx context.Context) error {
	ci, err := recorder(dagger.WorkspaceCiMemoStoreGitRefs, unreachableAPI)
	if err != nil {
		return err
	}
	got, err := ci.RecordPass(ctx, "", trustedRef, someCommit)
	if err != nil {
		return err
	}
	if got != "SKIPPED" {
		return fmt.Errorf("recording an unhashable leg reported %q, want SKIPPED", got)
	}
	return nil
}

// RecordPassSaysTheActionsCacheIsUnwritable keeps the old constraint honest. The
// Actions cache still needs ACTIONS_RUNTIME_TOKEN, so a consumer who configures it
// and then calls RecordPass has to be told, in the return value, rather than
// getting a silent no-op that looks exactly like a store nobody has recorded into.
func (t *Tests) RecordPassSaysTheActionsCacheIsUnwritable(ctx context.Context) error {
	ci, err := recorder(dagger.WorkspaceCiMemoStoreActionsCache, unreachableAPI)
	if err != nil {
		return err
	}
	got, err := ci.RecordPass(ctx, someHash, trustedRef, someCommit)
	if err != nil {
		return err
	}
	if got != "UNSUPPORTED" {
		return fmt.Errorf("recording into the Actions cache reported %q, want UNSUPPORTED", got)
	}
	return nil
}

// RecordPassSkipsWithNoStoreConfigured covers the default posture: memoization
// off is a planner that records nothing, not one that errors.
func (t *Tests) RecordPassSkipsWithNoStoreConfigured(ctx context.Context) error {
	got, err := dag.WorkspaceCi().RecordPass(ctx, someHash, trustedRef, someCommit)
	if err != nil {
		return err
	}
	if got != "SKIPPED" {
		return fmt.Errorf("recording with no store configured reported %q, want SKIPPED", got)
	}
	return nil
}

// RecordPassNeedsTheRunsRef is the one hard error. With no ref there is no scope
// to judge, so a silent refusal would be indistinguishable from a scope that was
// judged and rejected — and a CI system that never passes its ref would record
// nothing forever while reading as if it were.
func (t *Tests) RecordPassNeedsTheRunsRef(ctx context.Context) error {
	ci, err := recorder(dagger.WorkspaceCiMemoStoreGitRefs, unreachableAPI)
	if err != nil {
		return err
	}
	if _, err := ci.RecordPass(ctx, someHash, "", someCommit); err == nil {
		return fmt.Errorf("recording without the run's ref was accepted")
	}
	return nil
}

// NewRejectsAnUnknownMemoStore proves a misspelled store is a configuration
// error rather than a silent fall back to the read-only one, which would record
// nothing and look like a store nobody has written to.
func (t *Tests) NewRejectsAnUnknownMemoStore(ctx context.Context) error {
	_, err := dag.WorkspaceCi(dagger.WorkspaceCiOpts{MemoStore: "S3"}).
		RecordPass(ctx, someHash, trustedRef, someCommit)
	if err == nil {
		return fmt.Errorf("an unknown memo store was accepted")
	}
	return nil
}

// MemoStoreSelfTestPasses runs the module's own store check the way a consumer's
// CI would, so a regression in recording, idempotence, TTL filtering or scope
// isolation fails here too rather than only where it is installed.
func (t *Tests) MemoStoreSelfTestPasses(ctx context.Context) error {
	return dag.WorkspaceCi().MemoStoreSelfTest(ctx)
}
