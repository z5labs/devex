package gitdiff

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// repoBuilder writes files and commits them into a temp repo, for tests.
type repoBuilder struct {
	t   *testing.T
	dir string
	wt  *git.Worktree
	n   int
}

func newRepo(t *testing.T) *repoBuilder {
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
	return &repoBuilder{t: t, dir: dir, wt: wt}
}

func (b *repoBuilder) commit(files map[string]string) plumbing.Hash {
	b.t.Helper()
	for name, content := range files {
		full := filepath.Join(b.dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			b.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			b.t.Fatal(err)
		}
		if _, err := b.wt.Add(name); err != nil {
			b.t.Fatalf("add %s: %v", name, err)
		}
	}
	b.n++
	h, err := b.wt.Commit("commit", &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Unix(int64(b.n), 0)},
	})
	if err != nil {
		b.t.Fatalf("commit: %v", err)
	}
	return h
}

func (b *repoBuilder) checkout(h plumbing.Hash) {
	b.t.Helper()
	if err := b.wt.Checkout(&git.CheckoutOptions{Hash: h}); err != nil {
		b.t.Fatalf("checkout %s: %v", h, err)
	}
}

func TestChangedFilesLinear(t *testing.T) {
	b := newRepo(t)
	base := b.commit(map[string]string{
		"daggerverse/kicad/main.go": "v1",
		"daggerverse/go/main.go":    "g1",
	})
	head := b.commit(map[string]string{
		"daggerverse/kicad/main.go": "v2", // modified
		"daggerverse/new/main.go":   "n1", // added
	})
	got, err := ChangedFiles(b.dir, base.String(), head.String())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"daggerverse/kicad/main.go", "daggerverse/new/main.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestChangedFilesMergeBase proves three-dot semantics: a change made on the
// base branch after divergence must NOT appear in base...head.
func TestChangedFilesMergeBase(t *testing.T) {
	b := newRepo(t)
	root := b.commit(map[string]string{"root.txt": "r0"})

	// head diverges from root, touching kicad.
	b.checkout(root)
	head := b.commit(map[string]string{"daggerverse/kicad/main.go": "changed on head"})

	// base branch advances from root, touching kafka — this is base-only work.
	b.checkout(root)
	base := b.commit(map[string]string{"daggerverse/kafka/main.go": "changed on base"})

	got, err := ChangedFiles(b.dir, base.String(), head.String())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"daggerverse/kicad/main.go"} // NOT kafka
	if !reflect.DeepEqual(got, want) {
		t.Errorf("three-dot diff = %v, want %v (base-only change must be excluded)", got, want)
	}
}

func TestChangedFilesBadSHA(t *testing.T) {
	b := newRepo(t)
	head := b.commit(map[string]string{"a.txt": "a"})
	if _, err := ChangedFiles(b.dir, "deadbeef", head.String()); err == nil {
		t.Error("expected error for unresolvable base SHA")
	}
}
