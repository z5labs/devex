// Package gitdiff reads the change set and the object ids a plan is built from
// using a pure-Go git implementation (go-git), so change detection needs no git
// binary and no helper container. It operates on a materialized repository
// directory — the workspace-ci module exports the repository it plans for into
// scratch and calls in here — which also keeps the diff logic unit-testable
// against a synthetic repository.
package gitdiff

import (
	"fmt"
	"sort"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// HeadBlobs returns the blob object id of every file in the commit HEAD points
// at, keyed by repo-relative path.
//
// It reads HEAD rather than a caller-supplied SHA on purpose: the object ids are
// paired with file lists read out of the *checked-out* tree (a module's Dagger
// source context), and only HEAD is guaranteed to be the commit those files came
// from. On a pull_request event GitHub checks out refs/pull/N/merge, whose tree is
// not the head SHA's tree, so hashing against the event's head SHA would pair
// paths from one tree with object ids from another. A detached HEAD — which is
// what actions/checkout leaves behind — resolves fine.
//
// Git blob ids are content hashes computed by git itself, so this is a Merkle
// digest of the tree that costs nothing to recompute and, unlike a Dagger
// directory digest, is stable across engine releases.
func HeadBlobs(repoDir string) (map[string]string, error) {
	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		return nil, fmt.Errorf("open repo %q: %w", repoDir, err)
	}
	ref, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("resolve HEAD: %w", err)
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("resolve HEAD commit %s: %w", ref.Hash(), err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("HEAD tree: %w", err)
	}
	blobs := map[string]string{}
	err = tree.Files().ForEach(func(f *object.File) error {
		blobs[f.Name] = f.Hash.String()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk HEAD tree: %w", err)
	}
	return blobs, nil
}

// Change is one repo-relative path touched between two commits, together with
// whether it still exists at head.
//
// The caller needs the distinction because a path that is absent from a module's
// source set is indistinguishable, by path alone, from a path that was deleted:
// the first must be ignored (it is not an input), the second must not (its module
// really did change). Deleted is what tells them apart.
type Change struct {
	// Path is the repo-relative path.
	Path string
	// Deleted reports that Path does not exist at head, either because it was
	// removed or because it is the old name of a rename.
	Deleted bool
}

// Changes returns the repo-relative paths changed between base and head, using
// three-dot (merge-base) semantics: the diff is taken from the merge-base of base
// and head to head, so changes made on the base branch after the two diverged are
// not attributed to head. This mirrors `git diff base...head` and is what a PR
// "changes" set means.
//
// repoDir is a working-tree root containing a .git directory. Renamed paths
// contribute both their old and new names (conservative — the change could affect
// the module on either side), with the old name marked Deleted. base and head are
// revisions in every form git's rev-parse takes — see resolveCommit. The result is
// sorted by path.
func Changes(repoDir, base, head string) ([]Change, error) {
	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		return nil, fmt.Errorf("open repo %q: %w", repoDir, err)
	}
	baseCommit, err := resolveCommit(repo, base)
	if err != nil {
		return nil, fmt.Errorf("resolve base %q: %w", base, err)
	}
	headCommit, err := resolveCommit(repo, head)
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

	// A path exists at head exactly when it is the destination of some change.
	// Collect those first so a path that is both the source of one rename and the
	// destination of another (a swap) is not mistaken for deleted.
	atHead := make(map[string]struct{}, len(changes))
	for _, c := range changes {
		if c.To.Name != "" {
			atHead[c.To.Name] = struct{}{}
		}
	}

	deleted := make(map[string]bool, len(changes))
	for _, c := range changes {
		if n := c.From.Name; n != "" {
			_, present := atHead[n]
			deleted[n] = !present
		}
		if n := c.To.Name; n != "" {
			deleted[n] = false
		}
	}

	out := make([]Change, 0, len(deleted))
	for p, gone := range deleted {
		out = append(out, Change{Path: p, Deleted: gone})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// resolveCommit turns a revision into the commit it names, accepting everything
// git's rev-parse does: a full or abbreviated commit SHA, a branch or tag name,
// HEAD, and any of those carrying git's ~ and ^ suffixes. An annotated tag is
// dereferenced to the commit it points at.
//
// It goes through go-git's rev-parse rather than hashing the string directly
// because plumbing.NewHash zero-fills anything that is not forty hex digits: a
// branch name used to arrive as the all-zeros object id and fail as though its
// commit were missing. A commit SHA is still the form CI supplies — it is what
// the event payload carries — but anyone running the planner by hand has `main`
// and `HEAD`, not the SHAs those name.
//
// An unresolvable revision is an error, which Changes' caller turns into "no
// usable diff, run everything" rather than a failure. That is the same fail-safe
// an unusable diff range takes, and it is what keeps a typo costing time instead
// of correctness.
func resolveCommit(repo *git.Repository, rev string) (*object.Commit, error) {
	hash, err := repo.ResolveRevision(plumbing.Revision(rev))
	if err != nil {
		return nil, err
	}
	return repo.CommitObject(*hash)
}
