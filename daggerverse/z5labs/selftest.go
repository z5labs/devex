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

// ImageConfigSelfTest checks the rule every published image is held to: its
// OCI configuration is exactly what expectedImageConfig describes — the
// standardized environment, the declared entrypoint, and nothing else — and
// any difference from it is refused.
//
// It sits on the module rather than in tests/ because the rule it checks is
// unexported and, more to the point, because its most important cases cannot
// be reached from tests/ at all. Nothing caller-facing can put a variable, a
// label, a port, a working directory or a default argument on an image, which
// is by design; the consequence is that every branch refusing one is
// unexecutable through the public API, so a suite built out of real images can
// only ever exercise the passing case. Splitting the comparison out and
// driving it here is what makes those refusals guarantees rather than comments
// — delete one and this check goes red.
//
// The empty expectations are the half worth insisting on. An image that
// promises no working directory, no default arguments, no exposed ports and no
// labels promises those things exactly as much as it promises its PATH, and
// every one of them would be inherited from a base layer the moment this
// module builds on one (devex#426).
//
// It also pins the shape of the standardized set: that it is PATH, HOME and
// TMPDIR and nothing else, that each is the constant that names it, and that
// the plugin directory is on the PATH.
//
// It does **not** pin the values, and the distinction is worth stating because
// the check below reads like it does. The comparison is against the same
// constants expectedImageEnv returns, so it catches a fourth variable, a
// missing one, or a literal written into expectedImageEnv beside the constant
// — drift between the map and the constants — and not a constant that moved.
// Moving one is what breaks published Dockerfiles and deployed manifests, and
// the pin that catches it is the deliberately literal one in tests/, beside
// wantImagePath. Two literal copies here would be a third place to update and
// a second place to get it wrong.
//
// It runs in process and needs no container, so it is cheap enough to be a
// check of its own.
//
// +check
// +cache="session"
func (m *Z5labs) ImageConfigSelfTest(ctx context.Context) error {
	want := expectedImageEnv()

	// The standardized set is exactly these three names, each carrying the
	// constant that names it. A caller who writes `COPY --from=...
	// /usr/local/bin/thing` is relying on the plugin directory being on the
	// PATH; a deployment mounting an emptyDir is relying on TMPDIR being in
	// the set at all; and an out-of-pipeline consumer reading the image's
	// config is relying on the set being knowable. What each constant is set
	// to is pinned in tests/, not here — see the doc comment.
	pinned := map[string]string{"PATH": appPath, "HOME": appHomeDir, "TMPDIR": appTmpDir}
	if len(want) != len(pinned) {
		return fmt.Errorf("expected the standardized environment to be %v, got %v", sortedKeys(pinned), sortedKeys(want))
	}
	for _, name := range sortedKeys(pinned) {
		if want[name] != pinned[name] {
			return fmt.Errorf("expected the standardized environment to carry %s=%q, got %q", name, pinned[name], want[name])
		}
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

	// entry is the entrypoint the expected configuration is built around: an
	// ordinary one, in the directory an application's own binary lands in,
	// because a row that breaks the entrypoint has to differ from the
	// expectation the way a real image would.
	const entry = appDir + "/hello"

	// standard is the whole expected configuration, optionally broken in one
	// way. Each case starts from expectedImageConfig rather than from a
	// literal so that a field added to the contract — or a value moved within
	// it — does not leave a row here silently asserting the *old* contract is
	// accepted.
	standard := func(mutate func(*imageConfig)) imageConfig {
		cfg := expectedImageConfig(entry)
		if mutate != nil {
			mutate(&cfg)
		}
		return cfg
	}
	// withLabels is an expectation for the rows about labels this pipeline
	// does not set today. The label branches would otherwise have only their
	// empty-expectation halves driven, which is the shape of unexecutable
	// check this whole split exists to avoid: a base layer is exactly what
	// makes a demanded label real, and it must not arrive to find the branch
	// deleted.
	withLabels := func(labels map[string]string) *imageConfig {
		cfg := expectedImageConfig(entry)
		cfg.Labels = labels
		return &cfg
	}

	cases := []struct {
		name string
		// cfg is the configuration read off an image.
		cfg imageConfig
		// expect is what the pipeline demands of it. Nil means the standard
		// expectation, which is what a publish uses.
		expect *imageConfig
		// want is a substring the refusal must carry, or "" when the
		// configuration has to be accepted.
		want string
	}{
		{
			name: "exactly the standardized configuration",
			cfg:  standard(nil),
		},
		{
			name: "a stray variable beside a correct environment",
			cfg:  standard(func(c *imageConfig) { c.Env["AWS_SECRET_ACCESS_KEY"] = "leaked" }),
			want: "carries an environment variable this pipeline never sets, AWS_SECRET_ACCESS_KEY",
		},
		{
			name: "a stray variable and nothing else",
			cfg:  standard(func(c *imageConfig) { c.Env = map[string]string{"DEBUG": "1"} }),
			want: "carries an environment variable this pipeline never sets, DEBUG",
		},
		{
			name: "a PATH that lost the plugin directory",
			cfg:  standard(func(c *imageConfig) { c.Env["PATH"] = "/usr/bin:/bin" }),
			want: `sets PATH="/usr/bin:/bin"`,
		},
		{
			// The scratch space's path is what a deployment mounts at, so an
			// image that moved it would take every manifest written against
			// the old one with it.
			name: "a TMPDIR pointing somewhere else",
			cfg:  standard(func(c *imageConfig) { c.Env["TMPDIR"] = "/var/tmp" }),
			want: `sets TMPDIR="/var/tmp"`,
		},
		{
			name: "an image that lost TMPDIR",
			cfg:  standard(func(c *imageConfig) { delete(c.Env, "TMPDIR") }),
			want: "carries no TMPDIR",
		},
		{
			// Losing HOME does not fail loudly at runtime: the process reads
			// whatever the engine supplies instead, which is exactly the
			// per-runtime answer pinning it removed.
			name: "an image that lost HOME",
			cfg:  standard(func(c *imageConfig) { delete(c.Env, "HOME") }),
			want: "carries no HOME",
		},
		{
			name: "no environment at all",
			cfg:  standard(func(c *imageConfig) { c.Env = map[string]string{} }),
			want: "carries no HOME",
		},

		{
			// An image config's User is a string the runtime resolves, so
			// "root" and an unset field are the same identity said two ways.
			// The refusal is still right: this pipeline sets no user at all,
			// and something that wrote one wrote it from somewhere nothing
			// here controls.
			name: "an image that names root as its user",
			cfg:  standard(func(c *imageConfig) { c.User = "root" }),
			want: `sets its user to "root"`,
		},
		{
			// Refused *today*, and this row is the one that inverts when
			// devex#399 lands: the expectation gains 65532:65532, imageForEntry
			// gains the WithUser that puts it there, and this becomes the
			// accepted row while an empty User becomes the refused one. Written
			// out rather than left implicit so that landing #399 is a change to
			// this file rather than a discovery.
			name: "an image that runs as the application's user",
			cfg:  standard(func(c *imageConfig) { c.User = appOwner }),
			want: `sets its user to "65532:65532"`,
		},
		{
			name: "an entrypoint in the plugin directory",
			cfg:  standard(func(c *imageConfig) { c.Entrypoint = []string{appPluginDir + "/hello"} }),
			want: `sets its entrypoint to ["/usr/local/bin/hello"]`,
		},
		{
			// An argument baked into the entrypoint answers at build time a
			// question the deployment asks at run time, and it cannot be
			// overridden by a runtime's args the way a default argument can.
			name: "an entrypoint carrying an argument",
			cfg:  standard(func(c *imageConfig) { c.Entrypoint = []string{entry, "--serve"} }),
			want: `sets its entrypoint to ["/app/hello" "--serve"]`,
		},
		{
			name: "an image with no entrypoint at all",
			cfg:  standard(func(c *imageConfig) { c.Entrypoint = nil }),
			want: "sets its entrypoint to nothing",
		},
		{
			// A working directory is refused rather than merely unset: the
			// refusal of a relative contribution path is written on the
			// grounds that nothing here sets one — see contribute.go — so an
			// image that gained one would leave that message describing a
			// world the image no longer lives in.
			name: "an image with a working directory",
			cfg:  standard(func(c *imageConfig) { c.WorkingDir = appDir }),
			want: `sets its working directory to "/app"`,
		},
		{
			name: "an image with default arguments",
			cfg:  standard(func(c *imageConfig) { c.DefaultArgs = []string{"--help"} }),
			want: `sets its default arguments to ["--help"]`,
		},
		{
			// A port in the config is a claim about what the application does,
			// made by the image rather than by the application, and a
			// deployment publishes what it publishes regardless.
			name: "an image exposing a port",
			cfg:  standard(func(c *imageConfig) { c.ExposedPorts = []string{"8080/TCP"} }),
			want: `exposes ["8080/TCP"]`,
		},
		{
			name: "an image carrying a label nothing here wrote",
			cfg: standard(func(c *imageConfig) {
				c.Labels = map[string]string{"org.opencontainers.image.source": "https://example.com/elsewhere"}
			}),
			want: `carries a label this pipeline never sets, org.opencontainers.image.source="https://example.com/elsewhere"; a published image carries no labels`,
		},
		{
			name:   "a label whose value is not the one demanded",
			cfg:    standard(func(c *imageConfig) { c.Labels = map[string]string{"org.opencontainers.image.title": "other"} }),
			expect: withLabels(map[string]string{"org.opencontainers.image.title": "hello"}),
			want:   `sets the label org.opencontainers.image.title="other"`,
		},
		{
			name:   "a demanded label the image does not carry",
			cfg:    standard(nil),
			expect: withLabels(map[string]string{"org.opencontainers.image.title": "hello"}),
			want:   "carries no org.opencontainers.image.title label",
		},
		{
			name:   "a stray label beside a demanded set",
			cfg:    standard(func(c *imageConfig) { c.Labels = map[string]string{"com.example.debug": "1"} }),
			expect: withLabels(map[string]string{"org.opencontainers.image.title": "hello"}),
			want:   "a published image's labels are exactly org.opencontainers.image.title",
		},
		{
			// The accepting side of a non-empty expectation, without which
			// every label row above would stay green under a comparison that
			// had started refusing labels outright.
			name:   "exactly the labels demanded",
			cfg:    standard(func(c *imageConfig) { c.Labels = map[string]string{"org.opencontainers.image.title": "hello"} }),
			expect: withLabels(map[string]string{"org.opencontainers.image.title": "hello"}),
		},
	}
	for _, c := range cases {
		expect := expectedImageConfig(entry)
		if c.expect != nil {
			expect = *c.expect
		}
		err := diffImageConfig(c.cfg, expect)
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
