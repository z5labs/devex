package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/z-5-labs/internal/dagger"
)

// ComposeSelfTest checks the rules that decide whether one application's
// payload may be composed into another's image.
//
// It sits on the module rather than in tests/ for the reason
// ImageEnvironmentSelfTest and ContributionPathSelfTest record, and here the
// reason is stronger than economy. Two of these rules cannot be driven through
// the public API at all: no constructor in this module can declare a payload
// of more than one file, and no caller-facing seam can put an environment
// variable on an image — so a multi-file payload and a conflicting variable
// are branches that would be unexecutable end to end, and therefore deletable
// tomorrow with every test still green. Split out as free functions over
// declarations and maps, every branch runs in process.
//
// The end-to-end half — that the refusals really are wired into WithApp, that
// a composed payload lands where the plugin directory promises, and that the
// derived image is published, annotated and attested like any other — is in
// tests/, where it costs a pair of applications instead of a table.
//
// It runs in process and needs no container, so it is cheap enough to be a
// check of its own.
//
// +check
// +cache="session"
func (m *Z5labs) ComposeSelfTest(ctx context.Context) error {
	if err := checkComposablePayloads(); err != nil {
		return err
	}
	if err := checkComposedEnvConflicts(); err != nil {
		return err
	}
	if err := checkPlatformMatching(); err != nil {
		return err
	}
	if err := checkExecRefusals(); err != nil {
		return err
	}
	if err := checkComposedCrossings(); err != nil {
		return err
	}
	return checkComposeWiring(ctx)
}

// checkComposeWiring drives WithApp itself over the refusals whose rules the
// tables above check in isolation.
//
// It exists because a rule and its wiring are two things, and two of these
// rules have no route in through the public API at all: nothing caller-facing
// can declare a multi-file payload or put an environment variable on an image,
// so `composablePayload` and `composedEnvConflict` could be left unreferenced
// by WithApp and every other test here would still pass. That is the failure
// mode a table alone cannot see.
//
// The applications are constructed as struct literals rather than through a
// constructor, which is the whole reason this is a module self-test and not a
// test in tests/: nothing outside this package can build an App that is wrong
// in these particular ways, and an assertion that cannot be driven is an
// assertion that gets deleted.
//
// The containers are real, because the environment check reads them. They are
// empty containers with variables set and nothing else, so no image is built
// and nothing is compiled.
func checkComposeWiring(ctx context.Context) error {
	const platform = dagger.Platform("linux/amd64")
	build := func(entry string, env map[string]string, mutate func(*App)) *App {
		ctr := dag.Container(dagger.ContainerOpts{Platform: platform})
		for _, name := range sortedKeys(env) {
			ctr = ctr.WithEnvVariable(name, env[name])
		}
		app := &App{
			Version: "v1.0.0",
			Variants: []*variant{{
				Platform:      platform,
				Container:     ctr,
				Contributions: []contribution{{Name: "gen", Path: entry}},
			}},
			Payload: appPayload{Entry: entry, Files: []string{entry}},
		}
		if mutate != nil {
			mutate(app)
		}
		return app
	}
	standard := map[string]string{"PATH": appPath}

	cases := []struct {
		base, from *App
		refusal    string
		why        string
	}{
		{
			base:    build("/app/hello", standard, nil),
			from:    build("/app/gen", map[string]string{"PATH": "/opt/bin"}, nil),
			refusal: "one image has one environment",
			why:     "an environment conflict, which no caller-facing seam can produce and which last-writer-wins would hide",
		},
		{
			base: build("/app/hello", standard, nil),
			from: build("/app/gen", standard, func(a *App) {
				a.Payload.Files = []string{"/app/gen", "/app/lib/runtime.so"}
			}),
			refusal: "composing more than one file is not supported yet",
			why:     "a multi-file declared payload, which no constructor here can produce",
		},
		{
			base: build("/app/hello", standard, nil),
			from: build("/app/gen", standard, func(a *App) {
				// A declaration naming an entry, and a variant carrying
				// something else entirely: everything the variant does carry
				// is accounted for, so only the other direction can see this.
				a.ContributedPaths = []occupied{{Path: "/etc/thing.conf", Holder: contributedHolder}}
				a.Variants[0].Contributions = []contribution{{Name: "thing.conf", Path: "/etc/thing.conf"}}
			}),
			refusal: "carries nothing for",
			why:     "a declaration whose entry no contribution answers to, which would record a composed path holding no bytes",
		},
	}
	for _, c := range cases {
		if _, err := c.base.WithApp(ctx, c.from); err == nil {
			return fmt.Errorf("expected withApp to refuse %s, got nil", c.why)
		} else if !strings.Contains(err.Error(), c.refusal) {
			return fmt.Errorf("expected the refusal of %s to mention %q, got: %v", c.why, c.refusal, err)
		}
	}

	// A refusal must leave the base untouched. It is checked on the last case
	// above rather than on a case of its own because the refusal that fires
	// latest is the one with the most work already done behind it.
	base := build("/app/hello", standard, nil)
	from := build("/app/gen", standard, func(a *App) {
		a.Variants[0].Contributions[0].Path = "/app/somewhere-else"
	})
	if _, err := base.WithApp(ctx, from); err == nil {
		return fmt.Errorf("expected withApp to refuse a declaration nothing answers to, got nil")
	}
	if len(base.Variants[0].Contributions) != 1 || len(base.ContributedPaths) != 0 || len(base.Composed) != 0 {
		return fmt.Errorf("a refused withApp left the base holding %d contributions, %d paths and %d composed applications, want 1, 0 and 0",
			len(base.Variants[0].Contributions), len(base.ContributedPaths), len(base.Composed))
	}
	return nil
}

// checkComposablePayloads drives composablePayload over every shape of
// declaration that decides a rule.
func checkComposablePayloads() error {
	cases := []struct {
		payload appPayload
		// refusal is a substring the refusal must carry, and "" means the
		// payload has to be accepted.
		refusal string
		why     string
	}{
		{
			payload: appPayload{Entry: "/app/gen", Files: []string{"/app/gen"}},
			why:     "the ordinary case: one executable, which is what every chain produces today",
		},
		{
			payload: appPayload{},
			refusal: "declares no payload",
			why:     "an App no constructor in this module produced",
		},
		{
			payload: appPayload{Entry: "/app/gen", Files: []string{"/app/gen", "/app/lib/runtime.so"}},
			refusal: "composing more than one file is not supported yet",
			why:     "a tree-shaped payload, which is refused rather than published broken",
		},
		{
			payload: appPayload{Entry: "/app/gen", Files: []string{"/app/other"}},
			refusal: "the file that would be exec'd is not one it declared",
			why:     "a declaration that does not cover its own entry",
		},
	}
	for _, c := range cases {
		err := composablePayload(c.payload)
		if c.refusal == "" {
			if err != nil {
				return fmt.Errorf("expected the payload %v to be composable (%s), got: %v", c.payload.Files, c.why, err)
			}
			continue
		}
		if err == nil {
			return fmt.Errorf("expected the payload %v to be refused (%s), got nil", c.payload.Files, c.why)
		}
		if !strings.Contains(err.Error(), c.refusal) {
			return fmt.Errorf("expected the refusal of %v to mention %q (%s), got: %v", c.payload.Files, c.refusal, c.why, err)
		}
	}
	// The multi-file refusal has to name what is wrong rather than merely
	// report that something is: a caller told only "unsupported" has no way
	// to find out which file made it so.
	err := composablePayload(appPayload{Entry: "/app/gen", Files: []string{"/app/gen", "/app/lib/runtime.so"}})
	if err == nil {
		return fmt.Errorf("expected a multi-file payload to be refused, got nil")
	}
	for _, want := range []string{"/app/gen", "/app/lib/runtime.so"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the multi-file refusal to name %s, got: %v", want, err)
		}
	}
	return nil
}

// checkComposedEnvConflicts drives composedEnvConflict over the three ways two
// environments can fail to be one environment, and the one way they cannot.
func checkComposedEnvConflicts() error {
	standard := map[string]string{"PATH": appPath}
	cases := []struct {
		base, from map[string]string
		refusal    string
		why        string
	}{
		{
			base: standard,
			from: map[string]string{"PATH": appPath},
			why:  "the ordinary case: both sides carry the standardized environment and nothing else",
		},
		{
			base: map[string]string{},
			from: map[string]string{},
			why:  "two images that declare nothing agree about nothing to disagree over",
		},
		{
			base:    map[string]string{"PATH": appPath},
			from:    map[string]string{"PATH": "/opt/bin"},
			refusal: "one image has one environment",
			why:     "the same variable with two values, which last-writer-wins would resolve silently",
		},
		{
			base:    map[string]string{"PATH": appPath},
			from:    map[string]string{"PATH": appPath, "PYTHONPATH": "/app/lib"},
			refusal: "the base declares no PYTHONPATH",
			why:     "a variable only the composed payload declares, which the derived image would have to restate the base's environment to carry",
		},
		{
			base:    map[string]string{"PATH": appPath, "JAVA_HOME": "/opt/java"},
			from:    map[string]string{"PATH": appPath},
			refusal: "it would run under an environment it was not built for",
			why:     "a variable only the base declares, which the payload never saw",
		},
	}
	for _, c := range cases {
		err := composedEnvConflict(c.base, c.from)
		if c.refusal == "" {
			if err != nil {
				return fmt.Errorf("expected %v and %v to compose (%s), got: %v", c.base, c.from, c.why, err)
			}
			continue
		}
		if err == nil {
			return fmt.Errorf("expected %v and %v to be refused (%s), got nil", c.base, c.from, c.why)
		}
		if !strings.Contains(err.Error(), c.refusal) {
			return fmt.Errorf("expected the refusal of %v and %v to mention %q (%s), got: %v", c.base, c.from, c.refusal, c.why, err)
		}
	}
	// The value disagreement has to quote both values. A caller told only
	// which variable disagrees still has to go and read two images to find
	// out how.
	err := composedEnvConflict(map[string]string{"PATH": "/a"}, map[string]string{"PATH": "/b"})
	if err == nil {
		return fmt.Errorf("expected two values of PATH to be refused, got nil")
	}
	for _, want := range []string{`"/a"`, `"/b"`} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the refusal to quote %s, got: %v", want, err)
		}
	}
	return nil
}

// checkPlatformMatching drives matchPlatforms over the sets that pair and the
// sets that cannot.
//
// The containers are nil throughout: matchPlatforms decides on platforms
// alone, which is what lets the rule be checked without building an image per
// row.
func checkPlatformMatching() error {
	variants := func(platforms ...dagger.Platform) []*variant {
		out := make([]*variant, 0, len(platforms))
		for _, p := range platforms {
			out = append(out, &variant{Platform: p})
		}
		return out
	}
	cases := []struct {
		base, from []*variant
		refusal    string
		why        string
	}{
		{
			base: variants("linux/amd64", "linux/arm64"),
			from: variants("linux/amd64", "linux/arm64"),
			why:  "the ordinary case",
		},
		{
			base: variants("linux/amd64", "linux/arm64"),
			from: variants("linux/arm64", "linux/amd64"),
			why:  "the same set in another order, which is the same set",
		},
		{
			base: variants("linux/amd64"),
			from: variants("linux/amd64"),
			why:  "a single-platform application, which is not a degenerate case",
		},
		{
			base:    variants("linux/amd64", "linux/arm64"),
			from:    variants("linux/amd64"),
			refusal: "nothing would be composed into linux/arm64",
			why:     "a platform the payload was not built for, which would ship an index holding an executable for another architecture",
		},
		{
			base:    variants("linux/amd64"),
			from:    variants("linux/amd64", "linux/arm64"),
			refusal: "the payload built for linux/arm64 would be discarded",
			why:     "a platform only the payload carries, which would be dropped without a word",
		},
		{
			base:    nil,
			from:    variants("linux/amd64"),
			refusal: "carries no images to compose into",
			why:     "an application with no variants at all",
		},
		{
			base:    variants("linux/amd64"),
			from:    nil,
			refusal: "the application being composed carries no images",
			why:     "a payload with no variants at all",
		},
	}
	for _, c := range cases {
		pairs, err := matchPlatforms(c.base, c.from)
		if c.refusal == "" {
			if err != nil {
				return fmt.Errorf("expected %v and %v to pair (%s), got: %v", platformsOf(c.base), platformsOf(c.from), c.why, err)
			}
			if len(pairs) != len(c.base) {
				return fmt.Errorf("expected %d pairs (%s), got %d", len(c.base), c.why, len(pairs))
			}
			// Every pair has to be one platform on both sides. Pairing a
			// base variant with the wrong architecture's payload is the exact
			// failure this function exists to make impossible, and a length
			// check alone would not see it.
			for i, pair := range pairs {
				if pair.Base.Platform != c.base[i].Platform {
					return fmt.Errorf("expected pair %d to be built from %s, got %s", i, c.base[i].Platform, pair.Base.Platform)
				}
				if pair.From.Platform != pair.Base.Platform {
					return fmt.Errorf("paired the %s image with a %s payload", pair.Base.Platform, pair.From.Platform)
				}
			}
			continue
		}
		if err == nil {
			return fmt.Errorf("expected %v and %v to be refused (%s), got nil", platformsOf(c.base), platformsOf(c.from), c.why)
		}
		if !strings.Contains(err.Error(), c.refusal) {
			return fmt.Errorf("expected the refusal to mention %q (%s), got: %v", c.refusal, c.why, err)
		}
	}
	return nil
}

// checkExecRefusals drives execRefusal over what the runtime writes when a
// payload cannot be started, and over what a payload that did start writes.
//
// The two refused rows are the messages measured on Dagger v0.21.8 for the
// two failures this check exists to catch: an executable built for another
// architecture, and a dynamically linked one whose loader is not in the image.
// Both come back through ReturnTypeAny as exit code 1, which is why the exit
// code is not what decides.
func checkExecRefusals() error {
	const entry = "/usr/local/bin/gen"
	cases := []struct {
		stderr string
		reason string
		why    string
	}{
		{
			stderr: "fork/exec " + entry + ": exec format error\n",
			reason: "exec format error",
			why:    "an executable built for another architecture",
		},
		{
			stderr: "fork/exec " + entry + ": no such file or directory\n",
			reason: "no such file or directory",
			why:    "a dynamically linked executable whose loader is not in the image",
		},
		{
			stderr: "",
			why:    "a payload that started and said nothing",
		},
		{
			stderr: "usage: gen [flags]\n",
			why:    "a CLI that started, printed its usage and exited non-zero, which is not a failure to start",
		},
		{
			stderr: "fork/exec /usr/local/bin/other: exec format error\n",
			why:    "the runtime's message about some other path, which says nothing about this entry",
		},
		{
			stderr: "warning: fork/exec " + entry + ": exec format error\n",
			why:    "a payload that ran and mentioned the message rather than being the subject of it",
		},
	}
	for _, c := range cases {
		reason, refused := execRefusal(entry, c.stderr)
		if c.reason == "" {
			if refused {
				return fmt.Errorf("expected %q to be read as a payload that started (%s), got refused with %q", c.stderr, c.why, reason)
			}
			continue
		}
		if !refused {
			return fmt.Errorf("expected %q to be read as a failure to start (%s), got accepted", c.stderr, c.why)
		}
		if reason != c.reason {
			return fmt.Errorf("expected the reason for %q to be %q (%s), got %q", c.stderr, c.reason, c.why, reason)
		}
	}
	return nil
}

// checkComposedCrossings drives composedCrossings over an application carrying
// an entry and contributions of its own.
//
// The two things asserted are the two halves of the placement rule, and they
// are not symmetric: the entry *moves*, from the executable directory in its
// own image to the plugin directory here, because the base's CLI has to be
// able to find it; everything else keeps its own path, because a complete
// application is what may be composed.
func checkComposedCrossings() error {
	from := &App{
		Payload: appPayload{Entry: "/app/gen", Files: []string{"/app/gen"}},
		ContributedPaths: []occupied{
			{Path: "/etc/thing.conf", Holder: contributedHolder},
			{Path: "/srv/templates", Holder: contributedHolder},
		},
	}
	got := composedCrossings(from)
	want := []crossing{
		{From: "/app/gen", To: appPluginDir + "/gen"},
		{From: "/etc/thing.conf", To: "/etc/thing.conf"},
		{From: "/srv/templates", To: "/srv/templates"},
	}
	if len(got) != len(want) {
		return fmt.Errorf("expected %d things to cross, got %d", len(want), len(got))
	}
	for i, w := range want {
		if got[i].From != w.From || got[i].To != w.To {
			return fmt.Errorf("expected %s to land at %s, got %s landing at %s", w.From, w.To, got[i].From, got[i].To)
		}
		// The holder is what a collision refusal reads out, so every crossing
		// has to name the application it came from rather than leave the
		// reader to guess which composition put it there.
		if !strings.Contains(got[i].Holder, appPluginDir+"/gen") {
			return fmt.Errorf("expected the holder of %s to name the composed application, got %q", w.To, got[i].Holder)
		}
	}
	// The entry must not stay in the executable directory: that is the
	// application's own binary's home in every image, and an entry landing
	// there would collide with the base's binary on the very first
	// composition rather than sitting on the PATH where the base can find it.
	if strings.HasPrefix(got[0].To, appDir+"/") {
		return fmt.Errorf("expected the composed entry to leave %s, got %s", appDir, got[0].To)
	}
	return nil
}
