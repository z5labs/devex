package main

import (
	"context"
	"fmt"
	imagepath "path"
	"sort"
	"strings"

	"dagger/z-5-labs/internal/dagger"
)

// Composing one application's payload into another's image.
//
// A base image carrying a CLI, plus a derived image per plugin that adds one
// executable and changes nothing else, is a shape two adopters hand-rolled an
// entire image build for because the archetype had nowhere to put a second
// executable. WithApp is that seam. The derived image is the same program as
// the base wearing one more executable, so it inherits the base's entrypoint,
// its user, its environment and its executable directory rather than restating
// any of them — a derived image that quietly became a *different* program
// wearing the base's filesystem is the failure this whole file is written to
// prevent.
//
// # The producing chain declares its payload; composition never infers it
//
// The unit that crosses is a payload: the paths that make an application
// runnable, plus which one is the entry. Only whatever built the application
// can know that, so it is declared — AppBuilder records the entry it packaged
// — and WithApp reads the declaration.
//
// It deliberately does not read the entrypoint, which is the rejected design
// and is wrong in four ways that all fail silently. `ep[0]` is a PATH lookup
// rather than a path for ["python", "/app/main.py"]; later elements can be
// paths that would be dropped (["/app/server", "--config", "/etc/app.toml"]);
// an application driven by Cmd has no ep[0] at all; and a CGO_ENABLED=1 Go
// binary or a `java -jar` needs a loader that lives in the source image. In
// every one of those the image publishes, the attestation is well-formed, and
// it dies on first exec.
//
// occupiedPaths still reads the entrypoint, and that is not the same thing. It
// answers "what is already in this image" for the collision rules; nothing
// there decides what crosses.
//
// # Where each thing lands
//
// The composed application's **entry** lands in the standardized plugin
// directory, under its own file name. That directory is the one part of the
// image an extension is meant to fill — build.go's appPluginDir comment has
// been promising exactly that since before there was a way to fill it — and it
// is what makes `ghcr.io/z5labs/avroc-gen-go` the base image plus
// /usr/local/bin/avroc-gen-go and nothing else. It is emphatically not appDir:
// that holds the application's own binary, the one the inherited entrypoint
// names.
//
// Everything else the composed application carries lands **at its own path**.
// A complete application is what may be composed, not a slim carrier that only
// makes sense once merged, so a `from` whose image holds /etc/thing.conf puts
// it at /etc/thing.conf here too. The hazard is not that a path is unobvious;
// it is that two things want one path, and that is detectable — see the
// collision rules below.
//
// # What is refused, and why each refusal is a real failure
//
//   - A **platform mismatch**. Every platform of the derived image is built
//     from the matching platform of the base, and a set that does not match
//     exactly is refused in both directions: a missing one would ship an index
//     whose arm64 manifest holds an amd64 executable, which fails at exec time
//     with the kernel's message and for nobody here, and an extra one would
//     silently discard a platform the caller built.
//   - A **path collision**, with the base or with an application composed
//     earlier. The image would hold one thing while its documents describe
//     two, which is the undetectable incompleteness the contribution mechanism
//     exists to prevent, arriving through composition instead.
//   - A **conflicting environment variable**. Environment is image-level and
//     there is no namespacing, so two payloads wanting different values of one
//     variable cannot coexist. Last-writer-wins would give one of them an
//     environment it was never built for, silently.
//   - A **declared payload of more than one file**, for now. The restriction
//     is a check on the declaration rather than a limit of the seam: a
//     single-executable payload is what every chain produces today, and a
//     tree-shaped payload gets a refusal naming what is wrong instead of a
//     silently broken published image.
//
// # Completeness is proven by running it
//
// No API shape establishes that a payload is complete. A script's imports, a
// template opened on the first request, a dlopen, a CA bundle read at connect
// time — enumerating a payload is always a guess. So Publish executes the
// entry of every composed payload in the derived image before the first byte
// moves, and the contract is that a payload runs under the base's environment
// with nothing added. assertComposedPayloadsRun carries the mechanism and what
// it can and cannot tell apart.

// appPayload is what an application's constructor declared makes it runnable
// inside its image.
//
// It is declared rather than derived, and that is the whole point: the only
// thing that can know which paths make an application runnable is whatever put
// them there. AppBuilder records the executable it packaged; a chain producing
// an interpreted application would record its tree, and would owe a document
// covering that tree the same way every contribution owes one.
//
// The type is unexported and the field carrying it is +private, so nothing
// here reaches the schema. Its own fields are exported because the round trip
// across a call boundary is encoding/json.
type appPayload struct {
	// Entry is the absolute path in the application's own image that is
	// exec'd. It is empty for an App no constructor declared one for, which
	// is a state no constructor in this module produces.
	Entry string
	// Files is every path the constructor declared, Entry included. It is a
	// separate field rather than Entry alone because the payload is the unit
	// that crosses and a payload is not always one file — the first-version
	// restriction is a check on this slice, and a check on a field that could
	// only ever hold one thing would be a tautology.
	Files []string
}

// composedApp is one application composed into another: where its entry landed
// in the derived image and which release it came from.
//
// The version is carried because it is the one fact about the composed
// application that nothing else records. The SBOM says which module versions
// went into that executable and the derived image is published under the
// base's version, so without this nothing anywhere says which release of the
// plugin shipped.
type composedApp struct {
	// Entry is the absolute path in the *derived* image, which is what
	// Publish execs.
	Entry string
	// Version is the version the composed application was built under.
	Version string
}

// WithApp composes another application's payload into every platform of this
// one's image.
//
// The result is an ordinary App: published, annotated, signed and attested
// exactly like any other, to a repository of its own that has nothing to do
// with any binary's name. What comes out is the base program wearing one more
// executable — it keeps the base's entrypoint, user, environment and
// executable directory, and it is published under the base's version, because
// the release it belongs to is the base's release.
//
// from's entry lands in the standardized plugin directory under its own file
// name, so a base image whose CLI discovers plugins on the PATH finds it
// without either side agreeing on anything but that directory. Everything else
// from carries — its contributed files and directories — lands at its own
// path. The file comment above records why the entry is read from the
// declaration rather than from the entrypoint, and it is the single most
// important thing about this method.
//
// There is no path argument and nothing is inferred. A composed application is
// composed whole or refused: see the file comment for the four refusals and
// why each is a failure that would otherwise publish cleanly and die on first
// exec.
//
// Composition is not restricted to applications this module compiled, and a
// derived image may be composed into in turn — the payload of the result is
// the union of both sides', the collision surface grows and the semantics do
// not. What gets nothing from this seam is an out-of-pipeline image built
// `FROM` a published base: a stranger's Dockerfile adds bytes with no document
// and has no mechanism to produce one, so that image's attestation describes
// the base and nothing else.
//
// +cache="session"
func (a *App) WithApp(
	ctx context.Context,
	// The application whose payload is composed into this one's image.
	from *App,
) (*App, error) {
	if from == nil {
		return nil, fmt.Errorf("withApp requires an application to compose")
	}
	if err := composablePayload(from.Payload); err != nil {
		return nil, fmt.Errorf("withApp cannot compose this application: %v", err)
	}
	pairs, err := matchPlatforms(a.Variants, from.Variants)
	if err != nil {
		return nil, fmt.Errorf("withApp cannot compose this application: %v", err)
	}
	crossings := composedCrossings(from)
	taken, err := a.occupiedPaths(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range crossings {
		if why := overlappingPath(c.To, taken); why != "" {
			return nil, fmt.Errorf(
				"withApp cannot put %s at %s: %s, and content that landed on top of it would leave the image's documents "+
					"describing bytes that are not in it", c.Holder, c.To, why)
		}
		taken = append(taken, occupied{Path: c.To, Holder: c.Holder})
	}
	for _, pair := range pairs {
		baseEnv, err := containerEnv(ctx, pair.Base.Container)
		if err != nil {
			return nil, fmt.Errorf("read the %s image's environment: %v", pair.Base.Platform, err)
		}
		fromEnv, err := containerEnv(ctx, pair.From.Container)
		if err != nil {
			return nil, fmt.Errorf("read the composed %s image's environment: %v", pair.From.Platform, err)
		}
		if err := composedEnvConflict(baseEnv, fromEnv); err != nil {
			return nil, fmt.Errorf("withApp cannot compose this application: the %s images %v", pair.Base.Platform, err)
		}
	}
	// Nothing above this line has changed anything. Every refusal is made
	// before the first variant is touched, so a WithApp that fails leaves the
	// App exactly as it found it rather than half composed.
	destinations := make(map[string]string, len(crossings))
	for _, c := range crossings {
		destinations[c.From] = c.To
	}
	for _, pair := range pairs {
		for _, c := range pair.From.Contributions {
			to, ok := destinations[c.Path]
			if !ok {
				// Unreachable while every contribution is made through the
				// paths composedCrossings reads. It is an error rather than a
				// silent skip because the thing it would skip is bytes: an
				// image missing a file its documents describe is exactly what
				// this seam refuses everywhere else.
				return nil, fmt.Errorf(
					"withApp: the composed application carries %s in its %s image, which its declaration does not account for",
					c.Path, pair.From.Platform)
			}
			pair.Base.Container = placeContribution(pair.Base.Container, to, to == destinations[from.Payload.Entry], c)
			pair.Base.Contributions = append(pair.Base.Contributions, contribution{
				Name:    c.Name,
				Path:    to,
				File:    c.File,
				Content: c.Content,
				Tree:    c.Tree,
			})
		}
	}
	for _, c := range crossings {
		a.ContributedPaths = append(a.ContributedPaths, occupied{Path: c.To, Holder: c.Holder})
	}
	a.Composed = append(a.Composed, composedApp{
		Entry:   destinations[from.Payload.Entry],
		Version: from.Version,
	})
	a.Composed = append(a.Composed, from.Composed...)
	return a, nil
}

// crossing is one thing a composed application brings into the derived image:
// where it lives in its own image, where it lands here, and what to call it in
// a refusal.
type crossing struct {
	From   string
	To     string
	Holder string
}

// composedCrossings is everything from brings, and where each of it lands.
//
// The entry moves — from appDir in its own image to the plugin directory here
// — and everything else keeps its path. That asymmetry is the design: the
// entry is the one thing whose location is a property of the *relationship*
// between the two images, because the base's CLI has to be able to find it,
// while a contributed file's path is a property of the application that
// contributed it.
func composedCrossings(from *App) []crossing {
	entry := composedEntryPath(from.Payload.Entry)
	out := make([]crossing, 0, 1+len(from.ContributedPaths))
	out = append(out, crossing{
		From:   from.Payload.Entry,
		To:     entry,
		Holder: "the entry of the application composed at " + entry,
	})
	for _, p := range from.ContributedPaths {
		out = append(out, crossing{
			From:   p.Path,
			To:     p.Path,
			Holder: p.Holder + ", brought in by the application composed at " + entry,
		})
	}
	return out
}

// composedEntryPath is where a composed application's entry lands: the
// standardized plugin directory, under the entry's own file name.
func composedEntryPath(entry string) string {
	return appPluginDir + "/" + imagepath.Base(entry)
}

// placeContribution copies one of the composed application's contributions
// into the derived image at to.
//
// The modes and the owner are re-applied here rather than inherited from the
// source image, and both are the module's rather than either application's —
// the same rule contribute.go states, reached through a different door. The
// entry gets the executable mode because it is the one file in the payload
// that is exec'd; everything else gets the read-only mode its own contribution
// got.
func placeContribution(ctr *dagger.Container, to string, isEntry bool, c contribution) *dagger.Container {
	if c.Tree != nil {
		return ctr.WithDirectory(to, normalizedTree(c.Tree), dagger.ContainerWithDirectoryOpts{
			Owner: appOwner,
		})
	}
	mode := contributedFileMode
	if isEntry {
		mode = entryMode
	}
	return ctr.WithFile(to, c.Content, dagger.ContainerWithFileOpts{
		Owner:       appOwner,
		Permissions: mode,
	})
}

// variantPair is one platform's base variant beside the composed
// application's variant for the same platform.
type variantPair struct {
	Base *variant
	From *variant
}

// matchPlatforms pairs every base variant with the composed application's
// variant for the same platform, and refuses a set that does not match
// exactly.
//
// Both directions are refused and neither is pedantry. A platform the composed
// application does not carry would publish an index whose manifest for that
// architecture holds an executable built for another one — the failure arrives
// at exec time, with the kernel's message, in front of a consumer. A platform
// the *base* does not carry is a variant the caller built and paid for that
// would be dropped without a word.
//
// The message names both sides in the order they were built, because a caller
// looking at "linux/amd64" alone cannot tell which side is missing it.
func matchPlatforms(base, from []*variant) ([]variantPair, error) {
	if len(base) == 0 {
		return nil, fmt.Errorf("this application carries no images to compose into")
	}
	if len(from) == 0 {
		return nil, fmt.Errorf("the application being composed carries no images")
	}
	byPlatform := make(map[dagger.Platform]*variant, len(from))
	for _, v := range from {
		byPlatform[v.Platform] = v
	}
	pairs := make([]variantPair, 0, len(base))
	var missing []string
	for _, v := range base {
		other, ok := byPlatform[v.Platform]
		if !ok {
			missing = append(missing, string(v.Platform))
			continue
		}
		delete(byPlatform, v.Platform)
		pairs = append(pairs, variantPair{Base: v, From: other})
	}
	extra := make([]string, 0, len(byPlatform))
	for p := range byPlatform {
		extra = append(extra, string(p))
	}
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		return nil, fmt.Errorf(
			"every platform of the derived image has to be built from the matching platform of the application composed into it, "+
				"and this image carries %s while that one carries %s%s",
			strings.Join(platformsOf(base), ", "), strings.Join(platformsOf(from), ", "), mismatchDetail(missing, extra))
	}
	return pairs, nil
}

// platformsOf names a variant set's platforms in the order they were built.
func platformsOf(variants []*variant) []string {
	out := make([]string, 0, len(variants))
	for _, v := range variants {
		out = append(out, string(v.Platform))
	}
	return out
}

// mismatchDetail says which way round the mismatch is, so the reader does not
// have to diff two lists themselves.
func mismatchDetail(missing, extra []string) string {
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, "nothing would be composed into "+strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		parts = append(parts, "the payload built for "+strings.Join(extra, ", ")+" would be discarded")
	}
	if len(parts) == 0 {
		return ""
	}
	return " — " + strings.Join(parts, ", and ")
}

// composablePayload reports why a declared payload cannot be composed, or nil
// when it can.
//
// The restriction is the first version's, and it is deliberately a check on
// the *declaration* rather than a limit of the seam: everything below this
// function handles a payload of any shape, and every chain that exists today
// declares exactly one file. A chain that declared a tree would need a
// document covering that tree, and would need this refusal replaced by
// whatever proves the tree is complete — so until then a tree gets a message
// naming what is wrong instead of an image that publishes and does not run.
//
// It is a free function over a declaration rather than a method reading an
// App, and that is what makes the multi-file branch testable at all: nothing
// in this module's public API can declare a payload of more than one file
// today, so driving it end to end is impossible and the branch would be
// deletable with every test still green. ComposeSelfTest drives it in process.
// diffImageEnv is split the same way, for the same reason.
func composablePayload(p appPayload) error {
	if p.Entry == "" {
		return fmt.Errorf(
			"it declares no payload, so there is nothing to compose; an application built by this module always declares one, " +
				"and one that does not was assembled some other way")
	}
	if len(p.Files) > 1 {
		return fmt.Errorf(
			"its declared payload is %s, and composing more than one file is not supported yet; "+
				"a payload of one executable is what every chain produces today, and a tree-shaped one needs a document "+
				"covering the tree before it can be composed rather than published broken",
			strings.Join(p.Files, ", "))
	}
	if len(p.Files) == 1 && p.Files[0] != p.Entry {
		return fmt.Errorf(
			"its declared payload is %s while its entry is %s, so the file that would be exec'd is not one it declared",
			p.Files[0], p.Entry)
	}
	return nil
}

// containerEnv reads one image's environment as a map.
func containerEnv(ctx context.Context, ctr *dagger.Container) (map[string]string, error) {
	vars, err := ctr.EnvVariables(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(vars))
	for i := range vars {
		name, err := vars[i].Name(ctx)
		if err != nil {
			return nil, err
		}
		value, err := vars[i].Value(ctx)
		if err != nil {
			return nil, err
		}
		out[name] = value
	}
	return out, nil
}

// composedEnvConflict reports why two images' environments cannot coexist in
// one image, or nil when they can.
//
// Equality rather than containment, in both directions, which is the same rule
// diffImageEnv holds a published image to and for a stronger reason here.
// Environment is image-level and has no namespacing, so there is no way to
// give two payloads different values of one variable — and a variable one side
// declares and the other does not is not a free addition either: the derived
// image inherits the base's environment, so carrying it would restate the
// base's environment and dropping it would run a payload without something it
// declared it needed. Both are silent. Refusing is not.
//
// It is a free function over two maps for the reason composablePayload is one:
// no caller-facing seam can put a variable on an image today — see the package
// doc's "Environment is a runtime concern" — so every branch here is driven in
// process by ComposeSelfTest rather than by building two applications that
// cannot disagree.
func composedEnvConflict(base, from map[string]string) error {
	for _, name := range sortedKeys(from) {
		value, ok := base[name]
		if !ok {
			return fmt.Errorf(
				"disagree about %s: the application being composed declares %s=%q and the base declares no %s, "+
					"and a derived image inherits the base's environment rather than restating it",
				name, name, from[name], name)
		}
		if value != from[name] {
			return fmt.Errorf(
				"disagree about %s: the base declares %s=%q and the application being composed declares %s=%q, "+
					"and one image has one environment",
				name, name, value, name, from[name])
		}
	}
	for _, name := range sortedKeys(base) {
		if _, ok := from[name]; !ok {
			return fmt.Errorf(
				"disagree about %s: the base declares %s=%q and the application being composed declares no %s, "+
					"so it would run under an environment it was not built for",
				name, name, base[name], name)
		}
	}
	return nil
}

// execRefusal reports why the runtime could not start entry, and whether that
// is what happened at all.
//
// This is the whole of the "run it to prove it is complete" check, and it is a
// string match because the exit code cannot answer the question. Measured on
// Dagger v0.21.8: an exec that never starts — a binary for another
// architecture, a dynamically linked executable whose loader is not in the
// image — comes back through ReturnTypeAny as **exit code 1** with the
// runtime's own message on stderr, which is indistinguishable by exit code
// alone from a CLI that exits 1 because it was run with no arguments. Refusing
// on a non-zero exit would therefore refuse most working CLIs, and accepting
// exit 1 would accept every payload this check exists to catch.
//
// What is matched is the prefix the Go runtime writes before the process
// exists at all: "fork/exec <the exact path we asked for>: ". The program
// contributed nothing to stderr in that case, because it never ran. A program
// that ran and chose to print that exact prefix, with its own absolute path in
// it, would fail the publish with a message quoting what it printed — an
// outcome worth having over the alternative, which is a silent pass for
// everything above.
//
// The limit worth stating: this proves the entry *starts*, not that it works.
// A payload missing a template it opens on the first request still publishes.
// That is the honest boundary of a smoke test, and it is a long way past what
// inspecting an API shape can establish.
func execRefusal(entry, stderr string) (string, bool) {
	prefix := "fork/exec " + entry + ": "
	if !strings.HasPrefix(stderr, prefix) {
		return "", false
	}
	reason, _, _ := strings.Cut(strings.TrimPrefix(stderr, prefix), "\n")
	return strings.TrimSpace(reason), true
}

// assertComposedPayloadsRun executes the entry of every composed payload in
// every platform's derived image, and refuses to publish one that cannot
// start.
//
// It runs at publish time, beside the other things that have to be true before
// the first byte moves, and for the same reason: a payload that cannot run is
// an image that publishes cleanly, attests cleanly and dies in front of a
// consumer. Doing it here rather than in WithApp also keeps composition lazy —
// a chained builder that forced every platform's build would turn a call that
// costs nothing into one that costs a full cross-compile, which is the reason
// withVariantNamed exists.
//
// The entry is run with no arguments and no stdin, because the contract being
// checked is that a payload runs under the base's environment with *nothing*
// added. Any exit code passes; only a failure to start does not. The cost that
// buys is real and is accepted knowingly: an entry that blocks forever when
// run with no arguments blocks the publish. A payload composed into a base is
// an extension the base's CLI invokes, and one that cannot be started and left
// to exit is one no consumer can drive either.
func (a *App) assertComposedPayloadsRun(ctx context.Context) error {
	for _, c := range a.Composed {
		for _, v := range a.Variants {
			stderr, err := v.Container.
				WithExec([]string{c.Entry}, dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny}).
				Stderr(ctx)
			if err != nil {
				return fmt.Errorf("refusing to publish: running %s in the %s image: %v", c.Entry, v.Platform, err)
			}
			if reason, refused := execRefusal(c.Entry, stderr); refused {
				return fmt.Errorf(
					"refusing to publish: %s could not be started in the %s image (%s); a composed payload has to run under "+
						"the base's environment with nothing added, and this one needs something the image does not have",
					c.Entry, v.Platform, reason)
			}
		}
	}
	return nil
}
