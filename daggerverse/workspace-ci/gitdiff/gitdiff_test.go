package gitdiff

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// signature is fixed so a repository built twice is built identically.
var signature = object.Signature{
	Name:  "gitdiff test",
	Email: "test@example.invalid",
	When:  time.Unix(1700000000, 0).UTC(),
}

// testRepo is a three-commit repository with a branch and a tag naming its first
// commit, which is the shape every revision form below is asked about.
type testRepo struct {
	dir     string
	commits []string // in the order they were made
}

// newTestRepo builds it. first.txt lands in the initial commit, second.txt in the
// next, third.txt in the last, so a range picks out exactly the files it covers.
func newTestRepo(t *testing.T) testRepo {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	tr := testRepo{dir: dir}
	for _, name := range []string{"first.txt", "second.txt", "third.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if _, err := wt.Add("."); err != nil {
			t.Fatalf("stage %s: %v", name, err)
		}
		hash, err := wt.Commit("add "+name, &git.CommitOptions{Author: &signature, Committer: &signature})
		if err != nil {
			t.Fatalf("commit %s: %v", name, err)
		}
		tr.commits = append(tr.commits, hash.String())
	}

	// A branch and a lightweight tag on the first commit, so a range can be asked
	// for the way a person asks for one.
	for _, ref := range []plumbing.ReferenceName{
		plumbing.NewBranchReferenceName("base-branch"),
		plumbing.NewTagReferenceName("v0.1.0"),
	} {
		hashRef := plumbing.NewHashReference(ref, plumbing.NewHash(tr.commits[0]))
		if err := repo.Storer.SetReference(hashRef); err != nil {
			t.Fatalf("set %s: %v", ref, err)
		}
	}

	// An annotated tag on the same commit. It is a tag *object* rather than a
	// reference straight to the commit, so resolving it has to peel one level —
	// which is a separate path through rev-parse from the lightweight tag above
	// and would otherwise go untested.
	if _, err := repo.CreateTag("v0.2.0", plumbing.NewHash(tr.commits[0]), &git.CreateTagOptions{
		Tagger:  &signature,
		Message: "annotated v0.2.0",
	}); err != nil {
		t.Fatalf("annotated tag: %v", err)
	}
	return tr
}

// paths flattens a change set to the paths it names.
func paths(changes []Change) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.Path)
	}
	return out
}

// TestChangesAcceptsSymbolicRevisions proves a range named symbolically — a
// branch, a tag, HEAD, HEAD-relative — yields exactly what the equivalent SHAs
// yield. The GitHub event payload carries SHAs; a person at a terminal has `main`
// and `HEAD`, and both have to reach the same plan.
func TestChangesAcceptsSymbolicRevisions(t *testing.T) {
	tr := newTestRepo(t)
	want := []string{"second.txt", "third.txt"}

	for _, tc := range []struct{ name, base, head string }{
		{"full shas", tr.commits[0], tr.commits[2]},
		{"branch and head", "base-branch", "HEAD"},
		{"lightweight tag and head", "v0.1.0", "HEAD"},
		{"annotated tag and head", "v0.2.0", "HEAD"},
		{"head relative", "HEAD~2", "HEAD"},
		{"branch and sha", "base-branch", tr.commits[2]},
		{"sha and branch tip", tr.commits[0], "HEAD~0"},
		{"abbreviated shas", tr.commits[0][:8], tr.commits[2][:8]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changes, err := Changes(tr.dir, tc.base, tc.head)
			if err != nil {
				t.Fatalf("Changes(%q, %q): %v", tc.base, tc.head, err)
			}
			if got := paths(changes); !slices.Equal(got, want) {
				t.Errorf("Changes(%q, %q) = %v, want %v", tc.base, tc.head, got, want)
			}
		})
	}
}

// TestChangesRejectsAnUnresolvableRevision proves an unresolvable revision is an
// error rather than an empty change set. The distinction is load-bearing: the
// caller turns an error into "run everything" and an empty change set means the
// diff really was empty, so a typo that returned no changes would silently skip
// every check.
func TestChangesRejectsAnUnresolvableRevision(t *testing.T) {
	tr := newTestRepo(t)
	for _, tc := range []struct{ name, base, head string }{
		{"unknown base branch", "no-such-branch", "HEAD"},
		{"unknown head branch", "HEAD~2", "no-such-branch"},
		{"absent sha", "0123456789abcdef0123456789abcdef01234567", "HEAD"},
		{"empty base", "", "HEAD"},
		{"zero sha", "0000000000000000000000000000000000000000", "HEAD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changes, err := Changes(tr.dir, tc.base, tc.head)
			if err == nil {
				t.Fatalf("Changes(%q, %q) = %v, want an error", tc.base, tc.head, paths(changes))
			}
		})
	}
}

// TestChangesUsesMergeBaseSemantics proves the three-dot rule survives symbolic
// revisions: a commit made on the base branch after the two diverged is not
// attributed to head. Resolving a branch name to its tip is what makes this
// reachable at all — with SHAs the caller would have had to find the tip first.
func TestChangesUsesMergeBaseSemantics(t *testing.T) {
	tr := newTestRepo(t)
	repo, err := git.PlainOpen(tr.dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	// Move onto the branch that names the first commit and commit there, so the
	// two lines of history have diverged.
	if err := wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("base-branch")}); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tr.dir, "on-base.txt"), []byte("on-base\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := wt.Add("."); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := wt.Commit("add on-base.txt", &git.CommitOptions{Author: &signature, Committer: &signature}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	changes, err := Changes(tr.dir, "base-branch", tr.commits[2])
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	want := []string{"second.txt", "third.txt"}
	if got := paths(changes); !slices.Equal(got, want) {
		t.Errorf("Changes(base-branch, head) = %v, want %v; on-base.txt was made on the base after the two diverged", got, want)
	}
}
