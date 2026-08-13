package main

import (
	"context"
	"fmt"
	"strings"
)

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
