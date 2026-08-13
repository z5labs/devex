package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/z-5-labs/internal/dagger"
)

// VariantSetSelfTest checks the rules that decide which sets of prebuilt
// executables can become an application, and which cannot.
//
// It sits on the module for the reason ContributionPathSelfTest and
// ImageEnvironmentSelfTest do: the rules are unexported pure functions, and
// driving every branch of them through the public API would mean compiling a
// real executable per row of the tables below. The end-to-end half — that the
// refusals really are wired into WithVariant and Build, and that an accepted
// set produces an image that runs — is in tests/, where it costs one
// application instead of a dozen.
//
// It runs in process and needs no container, so it is cheap enough to be a
// check of its own.
//
// +check
// +cache="session"
func (m *Z5labs) VariantSetSelfTest(ctx context.Context) error {
	if err := checkEntryNames(); err != nil {
		return err
	}
	return checkVariantConflicts()
}

// checkEntryNames drives validateEntryName, which decides what may become the
// last segment of an image's entrypoint.
//
// The name is not something a caller types — it is read off the file they
// contributed — so every refusal here is about a file whose name cannot
// describe a place in the image. A name carrying a separator is the one that
// matters: the entry lands at appDir plus its name, so accepting one would put
// the executable outside the layout every published image promises, with the
// entrypoint still pointing at it and nothing saying so.
func checkEntryNames() error {
	cases := []struct {
		name string
		// want is the accepted name, and "" when the name has to be refused.
		want string
		// refusal is a substring the refusal must carry.
		refusal string
		why     string
	}{
		{name: "hello", want: "hello", why: "the ordinary case"},
		{name: "hello-service", want: "hello-service", why: "a name with a dash"},
		{name: "my.app", want: "my.app", why: "a name with a dot, which is not a path"},

		{name: "", refusal: "no file name", why: "a file with no name at all"},
		{name: "   ", refusal: "no file name", why: "whitespace, which is not a name"},
		{
			name:    "bin/hello",
			refusal: "is not a file name",
			why:     "a path, which would land the executable a directory below the standardized one",
		},
		{
			name:    "../hello",
			refusal: "is not a file name",
			why:     "a path that climbs out of the executable directory entirely",
		},
		{name: "/hello", refusal: "is not a file name", why: "an absolute path"},
		{name: ".", refusal: "is not a file name", why: "the current directory"},
		{name: "..", refusal: "is not a file name", why: "the parent directory"},
	}
	for _, c := range cases {
		got, err := validateEntryName(c.name)
		if c.want != "" {
			if err != nil {
				return fmt.Errorf("expected %q to be accepted (%s), got: %v", c.name, c.why, err)
			}
			if got != c.want {
				return fmt.Errorf("expected %q (%s) to be accepted as %q, got %q", c.name, c.why, c.want, got)
			}
			continue
		}
		if err == nil {
			return fmt.Errorf("expected %q to be refused (%s), got nil", c.name, c.why)
		}
		if !strings.Contains(err.Error(), c.refusal) {
			return fmt.Errorf("expected the refusal of %q (%s) to carry %q, got: %v", c.name, c.why, c.refusal, err)
		}
	}
	return nil
}

// checkVariantConflicts drives variantConflict, which is the rule that a set
// of executables has to agree with itself.
//
// Two failures, and they are different failures. A platform contributed twice
// means the second silently replaces the first, so the application ships fewer
// architectures than it was asked for — the same class of loss as a
// contribution landing on top of another. An entry name that differs per
// platform means the entrypoint is a different path per architecture, which a
// consumer overriding the entrypoint or writing a COPY --from= line is right
// about on one platform and wrong about on another.
//
// Which one is reported when a set is wrong in both ways is asserted too. A
// duplicated platform is a typo in the platform argument, and telling that
// caller their names disagree sends them to look at the wrong thing.
func checkVariantConflicts() error {
	taken := []variantKey{
		{Platform: "linux/amd64", Name: "hello"},
		{Platform: "linux/arm64", Name: "hello"},
	}
	cases := []struct {
		platform dagger.Platform
		name     string
		taken    []variantKey
		// hit is a substring the refusal must carry, or "" when the variant
		// has to be accepted.
		hit string
		why string
	}{
		{platform: "linux/arm/v7", name: "hello", taken: taken, why: "a third architecture, named consistently"},
		{platform: "linux/amd64", name: "hello", why: "the first variant of an empty set"},
		{platform: "linux/amd64", name: "anything", why: "the first variant names the set, so nothing can disagree yet"},

		{
			platform: "linux/amd64",
			name:     "hello",
			taken:    taken,
			hit:      "already contributed",
			why:      "a platform contributed twice, where the second would replace the first",
		},
		{
			platform: "linux/arm/v7",
			name:     "hello-arm",
			taken:    taken,
			hit:      "is called hello",
			why:      "an entry whose name disagrees, leaving the entrypoint per-architecture",
		},
		{
			platform: "linux/amd64",
			name:     "hello-amd",
			taken:    taken,
			hit:      "already contributed",
			why:      "wrong in both ways at once: the duplicated platform is reported, because that is the caller's typo",
		},
	}
	for _, c := range cases {
		got := variantConflict(c.platform, c.name, c.taken)
		if c.hit == "" {
			if got != "" {
				return fmt.Errorf("expected %s/%s (%s) to be accepted, got %q", c.platform, c.name, c.why, got)
			}
			continue
		}
		if !strings.Contains(got, c.hit) {
			return fmt.Errorf("expected the refusal of %s/%s (%s) to carry %q, got %q", c.platform, c.name, c.why, c.hit, got)
		}
	}
	return nil
}
