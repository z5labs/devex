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
// ImageEnvironmentSelfTest records: the rules are unexported pure functions,
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
		why     string
	}{
		{path: "/etc/ssl/certs/ca-certificates.crt", want: "/etc/ssl/certs/ca-certificates.crt", why: "the ordinary case"},
		{path: "/srv/templates", want: "/srv/templates", why: "a directory"},
		{path: "/srv/templates/", want: "/srv/templates", why: "a trailing slash is not a different path"},
		{path: "/srv/./templates//index.html", want: "/srv/templates/index.html", why: "a path that cleans to another"},
		{path: "  /srv/templates  ", want: "/srv/templates", why: "surrounding whitespace, which a shell leaves behind"},

		{path: "", refusal: "requires a path", why: "no path at all"},
		{path: "   ", refusal: "requires a path", why: "whitespace, which is not a path"},
		{
			path:    "etc/hosts",
			refusal: "is not an absolute path",
			why:     "a relative path, which would resolve against a working directory this module never sets",
		},
		{path: "./config.yml", refusal: "is not an absolute path", why: "an explicitly relative path"},
		{path: "/", refusal: "root is not a path to contribute at", why: "the whole filesystem"},
		{path: "/srv/..", refusal: "root is not a path to contribute at", why: "a path that climbs back out to the root"},
	}
	for _, c := range cases {
		got, err := validateContributionPath("withFile", c.path)
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
	}
	return nil
}

// checkContributionOverlaps drives overlappingPath, which is the rule that
// two contributions may not land on one another.
//
// The taken set below mixes an entrypoint with an earlier contribution on
// purpose: they are the same kind of claim on a path, and a rule that
// protected only one of them would let a caller replace the binary the
// provenance describes.
func checkContributionOverlaps() error {
	taken := []string{"/app/hello", "/srv/templates", "/etc/ssl/certs/ca-certificates.crt"}
	cases := []struct {
		path string
		// hit is the taken path the candidate collides with, or "" when it
		// collides with nothing.
		hit string
		why string
	}{
		{path: "/etc/hosts", why: "somewhere nothing else claims"},
		{path: "/srv/templates-v2", why: "a sibling whose name merely opens with another's"},
		{path: "/app/config.yml", why: "beside the binary, which is not on top of it"},

		{path: "/srv/templates", hit: "/srv/templates", why: "exactly where something already is"},
		{path: "/srv/templates/index.html", hit: "/srv/templates", why: "inside a contributed tree, where it would shadow a described file"},
		{path: "/srv", hit: "/srv/templates", why: "a tree containing a contributed one, which would replace it wholesale"},
		{path: "/app/hello", hit: "/app/hello", why: "the entrypoint itself"},
		{path: "/app", hit: "/app/hello", why: "a tree over the entrypoint's directory"},
		{
			path: "/etc/ssl/certs/ca-certificates.crt",
			hit:  "/etc/ssl/certs/ca-certificates.crt",
			why:  "a second contribution at one path, where the later one silently wins",
		},
	}
	for _, c := range cases {
		got, why := overlappingPath(c.path, taken)
		if got != c.hit {
			return fmt.Errorf("expected %q (%s) to collide with %q, got %q", c.path, c.why, c.hit, got)
		}
		if c.hit != "" && strings.TrimSpace(why) == "" {
			return fmt.Errorf("the collision of %q with %q is reported without saying how", c.path, c.hit)
		}
	}
	return nil
}
