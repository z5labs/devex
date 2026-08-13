package main

import (
	"context"
	"fmt"
	"strings"
)

// ContributionPathSelfTest checks the rules that decide where a caller may
// contribute content, and where they may not.
//
// It sits on the module rather than in tests/ for the reason
// ImageConfigSelfTest records: the rules are unexported pure functions,
// and driving every branch of them through the public API would mean building
// a real multi-platform app per row of the table below. The end-to-end half —
// that the refusal really is wired into WithFile and WithDirectory, and that
// the entrypoint is one of the paths it protects — is in tests/, where it
// costs one app instead of twenty.
//
// The rules exist because of one failure wearing several shapes: content that
// lands on top of other content leaves the image holding one thing while its
// documents describe two. That is the same undetectable incompleteness the
// contribution mechanism exists to prevent, arriving through the mechanism
// itself.
//
// The PATH's rows are the exception, and they answer a different question: what
// may reach a directory the image searches by name. Nothing contributed may,
// because only composition names the architecture of what it brings — see
// contribute.go. They are table rows rather than a mode bit because a rule
// enforced by a mode was bypassed by wrapping a binary in a directory, which is
// what devex#427 closed, and they cover every directory appPath names rather
// than the plugin directory alone: discovery by bare name is what the rule is
// about, and /usr/local/bin is one of six directories it can happen in.
//
// It runs in process and needs no container, so it is cheap enough to be a
// check of its own.
//
// +check
// +cache="session"
func (m *Z5labs) ContributionPathSelfTest(ctx context.Context) error {
	if err := checkContributionPaths(); err != nil {
		return err
	}
	return checkContributionOverlaps()
}

// checkContributionPaths drives validateContributionPath over every shape of
// path that decides a rule.
func checkContributionPaths() error {
	cases := []struct {
		path string
		// want is the cleaned path when it has to be accepted, and "" when it
		// has to be refused.
		want string
		// refusal is a substring the refusal must carry.
		refusal string
		// method is the helper the rule is driven as, and it is checked rather
		// than merely passed: every refusal opens with the method that hit it,
		// and a caller reading "withFile" under a withDirectory call goes
		// looking at the wrong line of their own pipeline. Empty means
		// withFile, so only the rows that exist to exercise the other helper
		// have to say so.
		method string
		why    string
	}{
		{path: "/etc/ssl/certs/ca-certificates.crt", want: "/etc/ssl/certs/ca-certificates.crt", why: "the ordinary case"},
		{path: "/srv/templates", want: "/srv/templates", why: "a directory"},
		{path: "/srv/templates/", want: "/srv/templates", why: "a trailing slash is not a different path"},
		{path: "/srv/./templates//index.html", want: "/srv/templates/index.html", why: "a path that cleans to another"},
		{
			path: "/srv/data ",
			want: "/srv/data ",
			why:  "a trailing space, which is a legal path on Linux and is taken literally rather than trimmed",
		},

		{path: "", refusal: "requires a path", why: "no path at all"},
		{path: "   ", refusal: "requires a path", why: "whitespace, which is not a path"},
		{
			path:    "etc/hosts",
			refusal: "is not an absolute path",
			why:     "a relative path, which would resolve against a working directory this module never sets",
		},
		{
			path:    " /srv/templates",
			refusal: "is not an absolute path",
			why:     "a leading space: refused rather than trimmed, because trimming would land content somewhere the caller did not say",
		},
		{path: "./config.yml", refusal: "is not an absolute path", why: "an explicitly relative path"},
		{path: "/", refusal: "root is not a path to contribute at", why: "the whole filesystem"},
		{path: "/srv/..", refusal: "root is not a path to contribute at", why: "a path that climbs back out to the root"},
		{
			path:    "/tmp",
			refusal: "/tmp is the scratch space TMPDIR names",
			why:     "the scratch space itself, which the image does not carry and the deployment mounts over",
		},
		{
			path:    "/tmp/seed.json",
			refusal: "/tmp/seed.json is inside /tmp",
			why:     "a file under the scratch space, which is in the image and its documents and invisible at runtime",
		},
		{
			path:    "/tmp/../tmp/cache",
			refusal: "is inside /tmp",
			why:     "a path that reaches the scratch space only after cleaning, which a prefix test on the raw string would miss",
		},
		{
			path: "/tmpfiles",
			want: "/tmpfiles",
			why:  "a sibling whose name merely opens with /tmp, which is not under it",
		},
		{
			path: "/home/nonroot/.config/app.yaml",
			want: "/home/nonroot/.config/app.yaml",
			why:  "read-only content under HOME, which the image does carry and which is deliberately not refused",
		},
		{
			path:    "/usr/local/bin",
			refusal: "/usr/local/bin is the plugin directory",
			method:  "withDirectory",
			why:     "the plugin directory itself, which App.WithApp fills and no contribution may",
		},
		{
			path:    "/usr/local/bin/gen",
			refusal: "/usr/local/bin/gen is inside /usr/local/bin",
			why:     "an executable contributed onto the PATH, which names no architecture and lands in every variant",
		},
		{
			path:    "/usr/local/bin/tools",
			refusal: "/usr/local/bin/tools is inside /usr/local/bin",
			method:  "withDirectory",
			why:     "a tree under the plugin directory, which is the bypass this rule was written for at its likeliest spelling",
		},
		{
			path:    "/usr/local/bin/../bin/gen",
			refusal: "is inside /usr/local/bin",
			why:     "a path that reaches the plugin directory only after cleaning, which a prefix test on the raw string would miss",
		},
		{
			path:    "/usr/local",
			refusal: "/usr/local would contain /usr/local/bin",
			method:  "withDirectory",
			why:     "a tree over the plugin directory, which is the bypass a mode bit could not refuse",
		},
		{
			path:    "/usr",
			refusal: "/usr would contain /usr/local/bin",
			why:     "a tree further up, refused for the same reason and without reading what is in it",
		},

		// The rest of the PATH. The plugin directory is one of six directories
		// the image searches, so a rule that guarded it alone would leave the
		// other five open to an executable of no stated architecture that
		// something finds by name — which is the failure the whole rule is
		// about, not a property of that one directory. These rows are what keep
		// the set derived from appPath rather than narrowed back to a constant.
		{
			path:    "/usr/bin",
			refusal: "/usr/bin is a directory the image's PATH resolves against",
			why:     "another directory on the PATH, where discovery by bare name works exactly as it does in the plugin directory",
		},
		{
			path:    "/bin/helper",
			refusal: "/bin/helper is inside /bin",
			method:  "withDirectory",
			why:     "a tree under the last directory on the PATH, which the leading-segment order must not skip",
		},
		{
			path:    "/usr/local/sbin",
			refusal: "/usr/local/sbin is a directory the image's PATH resolves against",
			why:     "the directory the PATH names first, which is not the plugin directory and is protected all the same",
		},

		{
			path: "/usr/local/binaries",
			want: "/usr/local/binaries",
			why:  "a sibling whose name merely opens with the plugin directory's, which is not inside it",
		},
		{
			path: "/binaries",
			want: "/binaries",
			why:  "a sibling of /bin, which the prefix test must not read as being under it",
		},
		{
			path: "/usr/local/share/ca-certificates/corp.crt",
			want: "/usr/local/share/ca-certificates/corp.crt",
			why:  "content beside the PATH's directories, which is refused by nothing — the rule is about the PATH",
		},
		{
			path: "/opt/thing/run",
			want: "/opt/thing/run",
			why:  "an executable off the PATH, which is the caller's own arrangement and deliberately not refused",
		},
	}
	for _, c := range cases {
		method := c.method
		if method == "" {
			method = "withFile"
		}
		got, err := validateContributionPath(method, c.path)
		if c.want != "" {
			if err != nil {
				return fmt.Errorf("expected %q to be accepted (%s), got: %v", c.path, c.why, err)
			}
			if got != c.want {
				return fmt.Errorf("expected %q (%s) to clean to %q, got %q", c.path, c.why, c.want, got)
			}
			continue
		}
		if err == nil {
			return fmt.Errorf("expected %q to be refused (%s), got nil", c.path, c.why)
		}
		if !strings.Contains(err.Error(), c.refusal) {
			return fmt.Errorf("expected the refusal of %q (%s) to carry %q, got: %v", c.path, c.why, c.refusal, err)
		}
		if !strings.HasPrefix(err.Error(), method) {
			return fmt.Errorf("expected the refusal of %q (%s) to open with %q, got: %v", c.path, c.why, method, err)
		}
	}
	return nil
}

// checkContributionOverlaps drives overlappingPath, which is the rule that
// two contributions may not land on one another.
//
// The taken set below mixes an entrypoint with an earlier contribution on
// purpose: they are the same kind of claim on a path, and a rule that
// protected only one of them would let a caller replace the binary the
// provenance describes. What each refusal *says* is asserted too, because the
// two cases read very differently to whoever hits them — being told that
// something was "already contributed" at the path your own application's
// binary occupies sends you looking for a contribution nobody made.
func checkContributionOverlaps() error {
	const binary = "the application's own binary"
	const contributed = "content already contributed"
	taken := []occupied{
		{Path: "/app/hello", Holder: binary + ", which the image runs"},
		{Path: "/srv/templates", Holder: contributed},
		{Path: "/etc/ssl/certs/ca-certificates.crt", Holder: contributed},
	}
	cases := []struct {
		path string
		// hit is a substring the refusal must carry, naming what the candidate
		// collided with, or "" when it collides with nothing.
		hit string
		// holder is a substring naming what holds the path it collided with.
		holder string
		why    string
	}{
		{path: "/etc/hosts", why: "somewhere nothing else claims"},
		{path: "/srv/templates-v2", why: "a sibling whose name merely opens with another's"},
		{path: "/app/config.yml", why: "beside the binary, which is not on top of it"},
		{path: "/srv/data ", why: "a path whose trailing space keeps it clear of /srv/data"},

		{path: "/srv/templates", hit: "already there", holder: contributed, why: "exactly where something already is"},
		{
			path:   "/srv/templates/index.html",
			hit:    "inside /srv/templates",
			holder: contributed,
			why:    "inside a contributed tree, where it would shadow a described file",
		},
		{
			path:   "/srv",
			hit:    "would contain /srv/templates",
			holder: contributed,
			why:    "a tree containing a contributed one, which would replace it wholesale",
		},
		{path: "/app/hello", hit: "already there", holder: binary, why: "the entrypoint itself"},
		{path: "/app", hit: "would contain /app/hello", holder: binary, why: "a tree over the entrypoint's directory"},
		{
			path:   "/etc/ssl/certs/ca-certificates.crt",
			hit:    "already there",
			holder: contributed,
			why:    "a second contribution at one path, where the later one silently wins",
		},
	}
	for _, c := range cases {
		got := overlappingPath(c.path, taken)
		if c.hit == "" {
			if got != "" {
				return fmt.Errorf("expected %q (%s) to collide with nothing, got %q", c.path, c.why, got)
			}
			continue
		}
		if !strings.Contains(got, c.hit) {
			return fmt.Errorf("expected the refusal of %q (%s) to carry %q, got %q", c.path, c.why, c.hit, got)
		}
		if !strings.Contains(got, c.holder) {
			return fmt.Errorf("expected the refusal of %q (%s) to name %q as what holds the path, got %q",
				c.path, c.why, c.holder, got)
		}
	}
	return nil
}
