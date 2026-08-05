package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"dagger/workspace-ci/memostore"
	"dagger/workspace-ci/refstore"
)

// What RecordPass reports. See its doc comment for what each one means. They are
// plain words rather than an enum so a CI system can compare them in shell
// without knowing anything about Dagger.
const (
	outcomeRecorded    = "RECORDED"
	outcomeDuplicate   = "ALREADY_RECORDED"
	outcomeRefused     = "REFUSED"
	outcomeSkipped     = "SKIPPED"
	outcomeUnsupported = "UNSUPPORTED"
	outcomeFailed      = "FAILED"
)

// storedPasses returns the input hashes some earlier run already passed on,
// according to the memoization store.
//
// Only the refs the caller nominated are read. Which store that is decides
// whether this module can also write it: GitHub's Actions cache cannot be written
// without ACTIONS_RUNTIME_TOKEN, which is a CI-native concern, so recording there
// stays a step in the calling workflow — while the git-ref store takes the same
// credential for both halves and RecordPass writes it from here.
//
// Either way the trust is bounded by the scopes the caller nominated. With the
// Actions cache a run can only write entries under its own ref; with git refs this
// module only writes from a ref in MemoRefs. So nominating the default branch and
// a PR's own scope admits no other branch and no fork. Within a PR's own scope the
// writer is that PR, which is why MemoTrusted refuses every recorded pass once a
// global input changed.
//
// Every failure is soft. An unreadable store, an unparseable response, a listing
// that ran long: each shrinks the known-good set, which costs CI time and never
// correctness. The one hard failure is a credential that cannot be read at all,
// which is a broken invocation rather than a store problem.
func (m *WorkspaceCi) storedPasses(ctx context.Context) (map[string]bool, error) {
	if m.MemoToken == nil || m.MemoRepo == "" || len(m.MemoRefs) == 0 {
		return nil, nil
	}
	token, err := m.MemoToken.Plaintext(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the memo token: %w", err)
	}

	cutoff := time.Now().Add(-time.Duration(m.MemoTTL) * time.Second)
	out := map[string]bool{}
	for _, ref := range m.MemoRefs {
		hashes, err := m.readScope(ctx, token, ref, cutoff)
		if err != nil {
			fmt.Fprintf(os.Stderr, "workspace-ci: cannot read the memo store for %q (%v); those legs will just run\n", ref, err)
		}
		for _, h := range hashes {
			out[h] = true
		}
	}
	return out, nil
}

// readScope reads one ref's scope out of whichever store is configured.
func (m *WorkspaceCi) readScope(ctx context.Context, token, ref string, cutoff time.Time) ([]string, error) {
	if m.MemoStore == MemoStoreGitRefs {
		return refstore.Hashes(ctx, m.api(), token, m.MemoRepo, ref, cutoff)
	}
	return memostore.Hashes(ctx, m.api(), token, m.MemoRepo, ref, cutoff)
}

// recordPass writes one entry, and swallows every reason it could not.
//
// The order of the guards is the argument for why a module-side write is safe to
// enable: the store has to be one this module can write at all, and then the ref
// the run is on has to be one the caller nominated. Neither guard is enforcement
// — a token that can create refs can create any ref — so the enforcement is the
// credential's own permissions plus whatever ruleset protects refstore.Namespace,
// which is what README.md says out loud rather than leaving implicit. What the
// refusal buys is that a run on an untrusted ref cannot record *by accident*,
// which is the failure that would otherwise arrive quietly and in bulk.
func (m *WorkspaceCi) recordPass(ctx context.Context, hash, ref, commit string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", fmt.Errorf("recordPass needs the git ref the run is on: with no ref there is no scope to judge, and refusing silently would be indistinguishable from a scope that was judged and rejected")
	}
	if hash == "" {
		// The plan says this leg may never be memoized. Recording nothing is the
		// whole point of an empty hash, so it is not worth a warning.
		return outcomeSkipped, nil
	}
	if m.MemoToken == nil || m.MemoRepo == "" {
		fmt.Fprintf(os.Stderr, "workspace-ci: no memoization store is configured; recorded nothing for %s\n", hash)
		return outcomeSkipped, nil
	}
	if m.MemoStore != MemoStoreGitRefs {
		fmt.Fprintf(os.Stderr, "workspace-ci: the %s store cannot be written from this module — a cache write needs ACTIONS_RUNTIME_TOKEN, which only a running workflow holds. Record with actions/cache/save, or configure --memo-store=%s.\n", m.MemoStore, MemoStoreGitRefs)
		return outcomeUnsupported, nil
	}
	if !slices.Contains(m.MemoRefs, ref) {
		fmt.Fprintf(os.Stderr, "workspace-ci: %q is not one of the trusted refs %v; recorded nothing for %s\n", ref, m.MemoRefs, hash)
		return outcomeRefused, nil
	}

	token, err := m.MemoToken.Plaintext(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workspace-ci: cannot read the memo token (%v); recorded nothing for %s\n", err, hash)
		return outcomeFailed, nil
	}
	created, err := refstore.Record(ctx, m.api(), token, m.MemoRepo, ref, hash, commit, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "workspace-ci: cannot record a pass for %s (%v); that leg will just run again\n", hash, err)
		return outcomeFailed, nil
	}
	if !created {
		return outcomeDuplicate, nil
	}
	return outcomeRecorded, nil
}

// api is the GitHub API root the store is reached through.
func (m *WorkspaceCi) api() string {
	if m.MemoAPI != "" {
		return m.MemoAPI
	}
	return memostore.GithubAPI
}
