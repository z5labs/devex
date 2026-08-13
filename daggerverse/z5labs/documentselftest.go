package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ContributedTreeSelfTest checks what a contributed tree may hold: directories
// and regular files, and nothing else.
//
// It sits on the module for the reason ContributionPathSelfTest does — walkTree
// is an unexported function over a real filesystem, and every row below would
// otherwise cost a Dagger call to build a tree that differs from its neighbour
// by one entry. What cannot be checked in process is that the rule is wired
// into both readers of a contributed tree, that Dagger's own export and copy
// preserve a link at all, and that a publish carrying one is refused before
// anything is pushed; those are in tests/.
//
// The rule is a refusal rather than a skip because a skipped link is in no
// document and no digest while still being in the image — see the "A symbolic
// link is not content" section in contribute.go, which carries the decision and
// the measurements behind it.
//
// It runs in process and needs no container, so it is cheap enough to be a
// check of its own.
//
// +check
// +cache="session"
func (m *Z5labs) ContributedTreeSelfTest(ctx context.Context) error {
	root, err := os.MkdirTemp("", "z5labs-treeselftest-*")
	if err != nil {
		return fmt.Errorf("create work dir: %v", err)
	}
	defer os.RemoveAll(root)

	cases := []struct {
		name string
		// build populates a tree of its own under root.
		build func(dir string) error
		// want is the file list walkTree must return, and nil when the tree
		// has to be refused instead.
		want []string
		// refusal is every substring the refusal must carry. A link is
		// refused so that a caller can go and find it, so naming the entry and
		// what it pointed at is the whole value of the message.
		refusal []string
	}{
		{
			name: "a tree of regular files",
			build: func(dir string) error {
				return writeTreeFiles(dir, "index.html", "partials/nav.html")
			},
			want: []string{"index.html", "partials/nav.html"},
		},
		{
			name: "a relative link beside the file it points at",
			build: func(dir string) error {
				if err := writeTreeFiles(dir, "real.txt"); err != nil {
					return err
				}
				return os.Symlink("real.txt", filepath.Join(dir, "link.txt"))
			},
			refusal: []string{"link.txt is a symbolic link to \"real.txt\"", "not content this module can contribute"},
		},
		{
			// The target is named verbatim rather than resolved. It resolves
			// against this module's own filesystem here and against the image's
			// at runtime, which is exactly why "a link that stays inside the
			// contributed tree is fine" is not a rule anything here could check.
			name: "an absolute link out of the tree",
			build: func(dir string) error {
				return os.Symlink("/etc/passwd", filepath.Join(dir, "passwd"))
			},
			refusal: []string{"passwd is a symbolic link to \"/etc/passwd\""},
		},
		{
			// A dangling link is refused for the same reason a resolvable one
			// is. Refusing only the ones that resolve would make the rule a
			// property of what happened to be beside the link.
			name: "a link pointing at nothing",
			build: func(dir string) error {
				return os.Symlink("gone.txt", filepath.Join(dir, "dangling.txt"))
			},
			refusal: []string{"dangling.txt is a symbolic link to \"gone.txt\""},
		},
		{
			// The path in the refusal is relative to the tree's root: the
			// absolute one names a temporary directory inside this module,
			// which is not a path the caller has ever seen.
			name: "a link inside a subdirectory",
			build: func(dir string) error {
				if err := writeTreeFiles(dir, "assets/logo.svg"); err != nil {
					return err
				}
				return os.Symlink("logo.svg", filepath.Join(dir, "assets", "current.svg"))
			},
			refusal: []string{"assets/current.svg is a symbolic link"},
		},
		{
			// A link to a directory is not descended into, so without the
			// refusal the tree it points at would be in the image and in
			// nothing else.
			name: "a link to a directory",
			build: func(dir string) error {
				if err := writeTreeFiles(dir, "v1.0.0/index.html"); err != nil {
					return err
				}
				return os.Symlink("v1.0.0", filepath.Join(dir, "current"))
			},
			refusal: []string{"current is a symbolic link to \"v1.0.0\""},
		},
		{
			// The walk finishes and the refusal names every offending entry,
			// so a tree that is full of links says so once rather than one
			// link per export.
			name: "several links in one tree",
			build: func(dir string) error {
				if err := writeTreeFiles(dir, "v1/index.html", "v2/index.html"); err != nil {
					return err
				}
				for _, link := range []struct{ target, name string }{
					{"v2", "current"},
					{"v1/index.html", "old.html"},
					{"v2/index.html", "new.html"},
				} {
					if err := os.Symlink(link.target, filepath.Join(dir, link.name)); err != nil {
						return err
					}
				}
				return nil
			},
			refusal: []string{
				`current is a symbolic link to "v2"`,
				"The same is true of new.html, old.html in the same tree",
			},
		},
		{
			// The kinds below are the rest of what an image cannot carry. They
			// are here because DirectoryDocument's doc comment promises them by
			// name, and because a mislabelled entry — the wrong arm of the mode
			// switch — is invisible until somebody hits one.
			name: "a named pipe",
			build: func(dir string) error {
				return syscall.Mkfifo(filepath.Join(dir, "events"), 0o644)
			},
			refusal: []string{"events is a named pipe", "not content this module can contribute"},
		},
		{
			name: "a socket",
			build: func(dir string) error {
				addr, err := net.ResolveUnixAddr("unix", filepath.Join(dir, "control.sock"))
				if err != nil {
					return err
				}
				l, err := net.ListenUnix("unix", addr)
				if err != nil {
					return err
				}
				// The socket has to outlive the listener, because what is
				// under test is a tree that holds one.
				l.SetUnlinkOnClose(false)
				return l.Close()
			},
			refusal: []string{"control.sock is a socket"},
		},
	}

	for i, c := range cases {
		dir := filepath.Join(root, fmt.Sprintf("case-%d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("%s: create the tree: %v", c.name, err)
		}
		if err := c.build(dir); err != nil {
			return fmt.Errorf("%s: build the tree: %v", c.name, err)
		}

		files, err := walkTree(dir)
		if len(c.refusal) == 0 {
			if err != nil {
				return fmt.Errorf("%s should be described, got: %v", c.name, err)
			}
			var got []string
			for _, f := range files {
				got = append(got, f.Path)
			}
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				return fmt.Errorf("%s lists %v, want %v", c.name, got, c.want)
			}
			continue
		}
		if err == nil {
			return fmt.Errorf("%s should be refused, got the file list %v", c.name, files)
		}
		// The type is checked as well as the wording, because it is what
		// DirectoryDocument, contentDigest and resolveContributions each
		// branch on to tell a refused entry apart from a tree they could not
		// read. A refusal that arrived as a plain error would be reported as
		// an I/O failure by all three.
		var bad *unsupportedEntry
		if !errors.As(err, &bad) {
			return fmt.Errorf("%s should be refused as an unsupported entry, got %T: %v", c.name, err, err)
		}
		for _, want := range c.refusal {
			if !strings.Contains(err.Error(), want) {
				return fmt.Errorf("the refusal of %s should carry %q, got: %v", c.name, want, err)
			}
		}
	}
	return nil
}

// writeTreeFiles writes a file at each of paths, relative to dir, creating the
// directories on the way.
func writeTreeFiles(dir string, paths ...string) error {
	for _, p := range paths {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(p+"\n"), 0o644); err != nil { //nolint:gosec // fixture content in this module's own temp dir
			return err
		}
	}
	return nil
}
