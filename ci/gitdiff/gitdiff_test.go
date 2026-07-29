package gitdiff

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
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

func (b *repoBuilder) remove(paths ...string) {
	b.t.Helper()
	for _, p := range paths {
		if _, err := b.wt.Remove(p); err != nil {
			b.t.Fatalf("remove %s: %v", p, err)
		}
	}
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

// TestChangesMarksDeletions pins the distinction the selector depends on: a
// removed file is reported Deleted, a surviving one is not. Without it a
// deleted source file looks exactly like a file a module declared out of its
// source set, and the module's checks would be skipped.
func TestChangesMarksDeletions(t *testing.T) {
	b := newRepo(t)
	base := b.commit(map[string]string{
		"daggerverse/kicad/main.go":   "v1",
		"daggerverse/kicad/README.md": "docs",
	})
	b.remove("daggerverse/kicad/main.go")
	head := b.commit(map[string]string{"daggerverse/kicad/README.md": "docs v2"})

	got, err := Changes(b.dir, base.String(), head.String())
	if err != nil {
		t.Fatal(err)
	}
	want := []Change{
		{Path: "daggerverse/kicad/README.md", Deleted: false},
		{Path: "daggerverse/kicad/main.go", Deleted: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestChangesRenameKeepsBothSides shows a rename contributing both names, the
// old one marked gone. go-git's plain tree diff reports a rename as a delete
// plus an add, which is exactly the conservative attribution we want.
func TestChangesRenameKeepsBothSides(t *testing.T) {
	b := newRepo(t)
	base := b.commit(map[string]string{"daggerverse/kicad/main.go": "v1"})
	b.remove("daggerverse/kicad/main.go")
	head := b.commit(map[string]string{"daggerverse/kicad/renamed.go": "v1"})

	got, err := Changes(b.dir, base.String(), head.String())
	if err != nil {
		t.Fatal(err)
	}
	want := []Change{
		{Path: "daggerverse/kicad/main.go", Deleted: true},
		{Path: "daggerverse/kicad/renamed.go", Deleted: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestChangedFilesBadSHA(t *testing.T) {
	b := newRepo(t)
	head := b.commit(map[string]string{"a.txt": "a"})
	if _, err := ChangedFiles(b.dir, "deadbeef", head.String()); err == nil {
		t.Error("expected error for unresolvable base SHA")
	}
}

// TestHeadBlobs covers the property the input hashes rest on: the object id of
// a path is a function of its content and nothing else, so two runs over the
// same tree agree and any edit disagrees.
func TestHeadBlobs(t *testing.T) {
	b := newRepo(t)
	first := b.commit(map[string]string{
		"daggerverse/kicad/main.go": "v1",
		"daggerverse/go/main.go":    "g1",
		"docs/copy.go":              "v1", // byte-identical to kicad's file
	})
	second := b.commit(map[string]string{"daggerverse/kicad/main.go": "v2"})

	// Checking out by hash leaves a detached HEAD, which is the shape
	// actions/checkout produces and so the shape HeadBlobs must handle.
	b.checkout(first)
	before, err := HeadBlobs(b.dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"daggerverse/go/main.go", "daggerverse/kicad/main.go", "docs/copy.go"}
	if got := sortedKeys(before); !reflect.DeepEqual(got, want) {
		t.Errorf("paths = %v, want %v", got, want)
	}
	if before["daggerverse/kicad/main.go"] != before["docs/copy.go"] {
		t.Error("identical content must share an object id")
	}

	b.checkout(second)
	after, err := HeadBlobs(b.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := sortedKeys(after); !reflect.DeepEqual(got, want) {
		t.Errorf("paths = %v, want %v", got, want)
	}
	if after["daggerverse/kicad/main.go"] == before["daggerverse/kicad/main.go"] {
		t.Error("edited content must change its object id")
	}
	if after["daggerverse/go/main.go"] != before["daggerverse/go/main.go"] {
		t.Error("untouched content must keep its object id")
	}
}

func TestHeadBlobsNoRepo(t *testing.T) {
	if _, err := HeadBlobs(t.TempDir()); err == nil {
		t.Error("expected error when there is no repository to read")
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
