// Package gitdiff computes the set of paths changed between two commits using a
// pure-Go git implementation (go-git), so CI change-detection needs no git
// binary and no helper container. It operates on a materialized repository
// directory (the ci module exports the workspace's .git into scratch, then calls
// ChangedFiles); keeping it a plain package makes the diff logic unit-testable
// against a synthetic repository.
package gitdiff

import (
	"fmt"
	"sort"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// ChangedFiles returns the repo-relative paths changed between base and head,
// using three-dot (merge-base) semantics: the diff is taken from the
// merge-base of base and head to head, so changes made on the base branch after
// the two diverged are not attributed to head. This mirrors
// `git diff base...head` and is what a PR "changes" set means.
//
// repoDir is a working-tree root containing a .git directory. Renamed paths
// contribute both their old and new names (conservative — the change could
// affect the module on either side). base/head are full commit SHAs.
func ChangedFiles(repoDir, base, head string) ([]string, error) {
	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		return nil, fmt.Errorf("open repo %q: %w", repoDir, err)
	}
	baseCommit, err := repo.CommitObject(plumbing.NewHash(base))
	if err != nil {
		return nil, fmt.Errorf("resolve base %q: %w", base, err)
	}
	headCommit, err := repo.CommitObject(plumbing.NewHash(head))
	if err != nil {
		return nil, fmt.Errorf("resolve head %q: %w", head, err)
	}

	from := baseCommit
	if mergeBases, err := baseCommit.MergeBase(headCommit); err != nil {
		return nil, fmt.Errorf("merge-base: %w", err)
	} else if len(mergeBases) > 0 {
		from = mergeBases[0]
	}

	fromTree, err := from.Tree()
	if err != nil {
		return nil, fmt.Errorf("base tree: %w", err)
	}
	headTree, err := headCommit.Tree()
	if err != nil {
		return nil, fmt.Errorf("head tree: %w", err)
	}
	changes, err := fromTree.Diff(headTree)
	if err != nil {
		return nil, fmt.Errorf("diff trees: %w", err)
	}

	set := make(map[string]struct{})
	for _, c := range changes {
		if c.From.Name != "" {
			set[c.From.Name] = struct{}{}
		}
		if c.To.Name != "" {
			set[c.To.Name] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}
