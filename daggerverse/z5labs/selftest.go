package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// VersionTagsSelfTest checks the tag family a release is published under,
// case by case, against the rule rather than against a release.
//
// It is a check of its own because the failure it guards is not one a
// publish can show you. A derivation that moves a tag it should not have —
// a prerelease that walks `v1`, a date-shaped version that invents a
// `2026.08` — succeeds at the registry and is discovered by a consumer whose
// `FROM` line resolved to something they never asked for, by which time it
// is published and someone has pulled it. So every shape that distinguishes
// the rule is stated here, including the accepting ones: a derivation that
// published one tag for everything would pass a table of prereleases alone.
//
// It sits on the module rather than in tests/ for the same reason
// ImageEnvironmentSelfTest does — the function is unexported — and because a
// table of thirty versions costs one in-process call here and thirty
// publishes there.
//
// +check
// +cache="session"
func (m *Z5labs) VersionTagsSelfTest(ctx context.Context) error {
	cases := []struct {
		version string
		// want is the exact family, or nil when the version has to be
		// refused.
		want []string
		// wantErr is a substring the refusal has to carry, set on exactly the
		// rows where want is nil. Checking the message rather than merely that
		// an error came back is what keeps a refusal specific: three rows
		// refused for three different reasons would all stay green under a
		// validator that had started refusing everything, and the one that
		// matters most — build metadata, refused rather than mangled — would
		// read as covered while nothing checked which rule fired.
		wantErr string
		why     string
	}{
		{
			version: "v1.2.3",
			want:    []string{"v1.2.3", "v1.2", "v1", "latest"},
			why:     "the ordinary tagged release",
		},
		{
			version: "1.2.3",
			want:    []string{"1.2.3", "1.2", "1", "latest"},
			why:     "the same release without the v prefix, which the family must not invent",
		},
		{
			version: "v0.1.0",
			want:    []string{"v0.1.0", "v0.1", "v0", "latest"},
			why:     "a v0 release: the family is mechanical, and v0 is a level consumers pin at",
		},
		{
			version: "v10.20.30",
			want:    []string{"v10.20.30", "v10.20", "v10", "latest"},
			why:     "multi-digit components, which a character-wise derivation would truncate",
		},
		{
			version: "2026.8.12",
			want:    []string{"2026.8.12", "2026.8", "2026", "latest"},
			why:     "a date-shaped version that is also valid SemVer, so it gets the family",
		},

		{
			version: "v1.3.0-rc.1",
			want:    []string{"v1.3.0-rc.1"},
			why:     "a prerelease publishes its own tag and moves none of the moving ones",
		},
		{
			version: "1.0.0-0",
			want:    []string{"1.0.0-0"},
			why:     "a numeric prerelease identifier is still a prerelease",
		},
		{
			version: "v2.0.0-alpha.1.2",
			want:    []string{"v2.0.0-alpha.1.2"},
			why:     "a dotted prerelease",
		},
		{
			version: "v2.0.0-rc-1",
			want:    []string{"v2.0.0-rc-1"},
			why:     "a hyphen inside the prerelease, which the split must not treat as a second one",
		},

		{
			version: "latest",
			want:    []string{"latest"},
			why:     "the moving tag named directly: not SemVer, so it publishes as itself and nothing else",
		},
		{
			version: "abc1234-2026-01-01T00-00-00Z",
			want:    []string{"abc1234-2026-01-01T00-00-00Z"},
			why:     "the shape the old HEAD-derived version had, which is the behaviour the family must preserve",
		},
		{
			version: "2026.08.12",
			want:    []string{"2026.08.12"},
			why:     `"08" is not a SemVer numeric identifier, so this moves nothing rather than inventing 2026.08`,
		},
		{
			version: "v1.2",
			want:    []string{"v1.2"},
			why:     "two components is not SemVer: a caller releasing v1.2 is not asking for a v1 that moves",
		},
		{
			version: "v1.2.3.4",
			want:    []string{"v1.2.3.4"},
			why:     "four components is not SemVer either",
		},
		{
			version: "v1.2.3-",
			want:    []string{"v1.2.3-"},
			why:     "an empty prerelease is not SemVer, and the safe reading of an unparseable version is one tag",
		},
		{
			version: "v01.2.3",
			want:    []string{"v01.2.3"},
			why:     "a leading zero is not a SemVer numeric identifier",
		},
		{
			version: "_internal",
			want:    []string{"_internal"},
			why:     "a legal tag that is not a version at all",
		},

		{
			version: "",
			wantErr: "version is required",
			why:     "an omitted version has no family and no single tag",
		},
		{
			version: "1.0.0+build.7",
			wantErr: "build metadata",
			why:     "SemVer build metadata must not be re-admitted at publish time",
		},
		{
			version: "1.0.0+build.7",
			// Deliberately not the bare "1.0.0", which is a prefix of the
			// rejected version the message already quotes: `release "1.0.0"`
			// can only come from the stripped form, which is the tag two
			// builds would silently share.
			wantErr: `release "1.0.0"`,
			why:     "the refusal has to name what the two builds would collapse to",
		},
		{
			version: "release/v1.2.3",
			wantErr: "not in the OCI tag charset",
			why:     "a version outside the OCI tag charset",
		},
	}
	for _, c := range cases {
		tags, err := versionTags(c.version)
		if c.want == nil {
			if err == nil {
				return fmt.Errorf("expected version %q to be refused (%s), got the tags %v", c.version, c.why, tags)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				return fmt.Errorf("expected the refusal of %q (%s) to carry %q, got: %v", c.version, c.why, c.wantErr, err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("expected version %q to derive %v (%s), got: %v", c.version, c.want, c.why, err)
		}
		if !slices.Equal(tags, c.want) {
			return fmt.Errorf("version %q (%s): want the tags %v, got %v", c.version, c.why, c.want, tags)
		}

		// Three invariants that hold whatever the table says, checked on every
		// accepted row rather than restated per case.
		//
		// The version itself is always the first tag written. Publish relies
		// on it: the immutable tag goes up before the moving ones, so a
		// half-written family never leaves a moving tag ahead of the release
		// it points at.
		if tags[0] != c.version {
			return fmt.Errorf("version %q: expected its own tag first, got %v", c.version, tags)
		}
		// Every derived tag has to be publishable in its own right. A
		// derivation that produced something outside the OCI tag charset would
		// fail at the registry, after the image was pushed.
		for _, tag := range tags {
			if err := validateVersion(tag); err != nil {
				return fmt.Errorf("version %q derived the tag %q, which cannot be an image tag: %v", c.version, tag, err)
			}
		}
		// A duplicate would have the publish write one tag twice and report
		// two references to it.
		for i, tag := range tags {
			if slices.Contains(tags[i+1:], tag) {
				return fmt.Errorf("version %q derived %v, which repeats %q", c.version, tags, tag)
			}
		}
	}
	return nil
}

// ImageEnvironmentSelfTest checks the rule every published image is held to:
// its environment is exactly the standardized set, and anything else — a
// stray variable, a changed value, a missing one — is refused.
//
// It sits on the module rather than in tests/ because the rule it checks is
// unexported and, more to the point, because its most important case cannot
// be reached from tests/ at all. Nothing caller-facing can put a variable on
// an image, which is by design; the consequence is that the branch refusing
// a stray variable is unexecutable through the public API, so a suite built
// out of real images can only ever exercise the passing case. Splitting the
// comparison out and driving it here is what makes the refusal a guarantee
// rather than a comment — delete it and this check goes red.
//
// It also pins the standardized set itself: that PATH is the only variable,
// and that the plugin directory is on it. Both are things other people write
// Dockerfiles against.
//
// It runs in process and needs no container, so it is cheap enough to be a
// check of its own.
//
// +check
// +cache="session"
func (m *Z5labs) ImageEnvironmentSelfTest(ctx context.Context) error {
	want := expectedImageEnv()

	// The standardized set is PATH alone, and the plugin directory is on it.
	// A caller who writes `COPY --from=... /usr/local/bin/thing` is relying
	// on the second of those, and an out-of-pipeline consumer reading the
	// image's config is relying on the first.
	if len(want) != 1 || want["PATH"] == "" {
		return fmt.Errorf("expected the standardized environment to be PATH alone, got %v", want)
	}
	onPath := false
	for _, dir := range strings.Split(want["PATH"], ":") {
		if dir == appPluginDir {
			onPath = true
		}
	}
	if !onPath {
		return fmt.Errorf("the plugin directory %s is not on the standardized PATH %q", appPluginDir, want["PATH"])
	}

	cases := []struct {
		name string
		env  map[string]string
		// want is a substring the refusal must carry, or "" when the
		// environment has to be accepted.
		want string
	}{
		{
			name: "exactly the standardized set",
			env:  map[string]string{"PATH": appPath},
		},
		{
			name: "a stray variable beside a correct PATH",
			env:  map[string]string{"PATH": appPath, "AWS_SECRET_ACCESS_KEY": "leaked"},
			want: "carries an environment variable this pipeline never sets, AWS_SECRET_ACCESS_KEY",
		},
		{
			name: "a stray variable and nothing else",
			env:  map[string]string{"DEBUG": "1"},
			want: "carries an environment variable this pipeline never sets, DEBUG",
		},
		{
			name: "a PATH that lost the plugin directory",
			env:  map[string]string{"PATH": "/usr/bin:/bin"},
			want: `sets PATH="/usr/bin:/bin"`,
		},
		{
			name: "no environment at all",
			env:  map[string]string{},
			want: "carries no PATH",
		},
	}
	for _, c := range cases {
		err := diffImageEnv(c.env, want)
		if c.want == "" {
			if err != nil {
				return fmt.Errorf("expected %s to be accepted, got: %v", c.name, err)
			}
			continue
		}
		if err == nil {
			return fmt.Errorf("expected %s to be refused, got nil", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			return fmt.Errorf("expected the refusal of %s to carry %q, got: %v", c.name, c.want, err)
		}
	}
	return nil
}
