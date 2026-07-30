package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"dagger/workspace-ci/memostore"
)

// storedPasses returns the input hashes some earlier run already passed on,
// according to the memoization store.
//
// Only the refs the caller nominated are read, and reading is all this module ever
// does — a write needs ACTIONS_RUNTIME_TOKEN, which is a CI-native concern, so
// recording a pass stays a step in the calling workflow. That asymmetry is also
// what bounds the trust: a run can only write cache entries under its own ref, so
// nominating the default branch and a PR's own scope admits no other branch and no
// fork. Within a PR's own scope the writer is that PR, which is why MemoTrusted
// refuses every recorded pass once a global input changed.
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
		hashes, err := memostore.Hashes(ctx, memostore.GithubAPI, token, m.MemoRepo, ref, cutoff)
		if err != nil {
			fmt.Fprintf(os.Stderr, "workspace-ci: cannot read the memo store for %q (%v); those legs will just run\n", ref, err)
		}
		for _, h := range hashes {
			out[h] = true
		}
	}
	return out, nil
}
