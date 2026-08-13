package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/z-5-labs/internal/dagger"
)

// The image environment every application this module builds carries, in
// full. Every value is fixed here and there is deliberately no
// caller-facing way to move any of them: a published image is something other
// people write `FROM` and `COPY` lines against, and a PATH that varied per
// app would make "put your plugin on the PATH" a question you have to ask
// per image rather than a contract the module promises.
//
// appPath is the conventional default a container runtime injects when an
// image sets none. Taking that value rather than inventing one means an
// image that later gains a real base layer keeps the PATH its base expects,
// and an out-of-pipeline consumer writing `COPY --from=` gets the layout
// every other image already has.
//
// appPluginDir is the one directory on it that an extension's executables
// land in. It is the conventional home for locally installed executables,
// and it has to be the same directory in every image for the contract to be
// worth anything — a plugin directory that moved per app could not be
// written against at all.
//
// One seam puts an executable there and it is App.WithApp: an extension
// arrives as a whole application, so every byte it brings names the platform
// it was built for, the variant sets are paired platform by platform, and the
// entry is exec'd in the derived image before anything is published.
// Contributing there is refused outright, in all three directions —
// App.WithFile and App.WithDirectory take a file and a directory, neither of
// which carries an architecture, and both land the same bytes in every
// variant. The refusal covers every directory appPath names and not this one
// alone, because the hazard is discovery by bare name and this is one of six
// directories the image searches. See compose.go for the seam and contribute.go
// for the refusal.
//
// Those two halves used to contradict each other, which is what devex#427
// settled: a contributed file lands 0444 and so was unusable here, while a
// contributed tree lands 0555 and was not — so the rule one helper enforced
// by omission was bypassed by wrapping the same binary in a directory. The
// sentence above means composition, and now says so.
//
// The application's own binary does not live here and does not rely on the
// PATH: appDir holds it and the entrypoint names it absolutely. PATH exists
// for what an extension adds, not for finding the app itself.
//
// appHomeDir and appTmpDir are the other two variables' values, and they are
// pinned here for the reason PATH is: what a process reads out of HOME and
// TMPDIR is otherwise the *runtime's* choice rather than the image's. Measured
// under podman 5.8.4 on an image with this layout, an unset HOME gives ""
// under uid 0 and "/" under a uid override, so os.UserHomeDir returns an error
// in one deployment and a root-owned directory in another, from one digest.
// Pinning them makes the answer a property of the image (devex#424).
//
// They differ in what the image carries behind them, and the difference is
// deliberate — see imageHome and the package doc's "The image contract".
const (
	appPluginDir = "/usr/local/bin"
	// appPath is composed from appPluginDir rather than spelled out, so that
	// "the plugin directory is on the PATH" is a fact the compiler keeps
	// rather than one two string literals happen to agree on. Editing the
	// PATH to drop the directory is then not something you can do by
	// accident.
	appPath    = "/usr/local/sbin:" + appPluginDir + ":/usr/sbin:/usr/bin:/sbin:/bin"
	appDir     = "/app"
	appHomeDir = "/home/nonroot"
	appTmpDir  = "/tmp"
)

// appHomeMode is the mode HOME's directory lands with: traversable and
// readable, writable by nobody.
//
// It is the same mode a contributed directory gets, and for a stronger version
// of the same reason — an image whose files the application can rewrite is one
// whose published digest stops describing what is running, and a home
// directory is the first place a program tries to write. It stays owned by
// root rather than by appOwner, so the refusal survives a caller overriding
// the runtime user to anything but root.
const appHomeMode = 0o555

// imageHome is the directory HOME names, as content to copy into an image.
//
// The mode is set on a Directory and the Directory is copied in, because
// Container.withDirectory silently ignores its permissions argument while
// Directory.withNewDirectory honours it (measured, Dagger v0.21.8 — see
// daggerverse/CLAUDE.md). Setting it on the container copy would leave this
// comment describing a mode the image does not have.
//
// It is created under a name and taken back out of it for the reason
// normalizedTree is: the root of a Directory is not something a copy sets a
// mode on, so building it at the root would leave the home directory itself at
// the default while nothing inside it changed.
func imageHome() *dagger.Directory {
	const at = "home"
	return dag.Directory().
		WithNewDirectory(at, dagger.DirectoryWithNewDirectoryOpts{Permissions: appHomeMode}).
		Directory(at)
}

// stampVersionVar and stampCommitVar are the linker symbols every binary
// this module builds is stamped with. They are fixed by the module rather
// than chosen per application, so every z5labs Go application answers
// "which build am I running" the same way.
const (
	stampVersionVar = "main.version"
	stampCommitVar  = "main.commit"
)

// gitState is everything the build reads out of HEAD. It is read in one
// exec because these values are always wanted together and each would
// otherwise be its own container round trip.
type gitState struct {
	// SHA is the unabbreviated HEAD SHA. It is what the revision
	// annotation and the provenance carry, because those are read by
	// tooling resolving a commit.
	SHA string
	// ShortSHA is the abbreviated HEAD SHA, which is what a binary reports
	// as main.commit — that one is read by people.
	ShortSHA string
	// Created is HEAD's committer time in RFC 3339. The commit's time and
	// not the build's: a wall-clock value would make every rebuild of one
	// commit a different manifest.
	Created string
	// SourceURI is the origin remote with any userinfo stripped, empty
	// when the tree has no origin.
	SourceURI string
}

// gitFacts reads HEAD's identity out of the source tree.
//
// The origin lookup is allowed to fail — `git config --get` exits 1 for a
// key that is not set, which is a repository without an origin and not an
// error — so its status is discarded and an empty line is what a missing
// origin looks like to the parser.
func (g *GoChain) gitFacts(ctx context.Context) (gitState, error) {
	out, execErr := dag.Go().Container(g.Source).
		WithExec([]string{"sh", "-c", `
set -e
printf 'sha=%s\n' "$(git rev-parse HEAD)"
printf 'shortSha=%s\n' "$(git rev-parse --short HEAD)"
printf 'created=%s\n' "$(git show -s --format=%cI HEAD)"
printf 'source=%s\n' "$(git config --get remote.origin.url || true)"
`}).
		Stdout(ctx)
	if execErr != nil {
		return gitState{}, fmt.Errorf("could not derive the build identity from source: %v", execErr)
	}
	fields := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		fields[key] = value
	}
	// The failures below name the field and not the output. `git config
	// --get remote.origin.url` routinely returns a URL with credentials in
	// it — `https://x-access-token:<token>@host/org/repo` is what a GitHub
	// Actions checkout leaves behind — and an error message is the least
	// controlled output this module has.
	if fields["sha"] == "" || fields["shortSha"] == "" {
		return gitState{}, fmt.Errorf("could not derive the build identity from source: HEAD names no commit")
	}
	if fields["created"] == "" {
		return gitState{}, fmt.Errorf("could not derive the build identity from source: HEAD carries no commit time")
	}
	return gitState{
		SHA:       fields["sha"],
		ShortSHA:  fields["shortSha"],
		Created:   fields["created"],
		SourceURI: redactURLCredentials(fields["source"]),
	}, nil
}

// requireGitWorkingTree confirms source is a git working tree by accepting
// either a `.git` directory (normal clone) or a `.git` file (worktrees /
// submodules — where `.git` is a "gitdir: ..." pointer). Detection errors
// are wrapped so unrelated I/O failures surface.
func requireGitWorkingTree(ctx context.Context, source *dagger.Directory) error {
	entries, err := source.Entries(ctx)
	if err != nil {
		return fmt.Errorf("source must be a git working tree: list entries: %w", err)
	}
	for _, e := range entries {
		if e == ".git" || e == ".git/" {
			return nil
		}
	}
	return fmt.Errorf("source must be a git working tree: no .git directory or file found")
}

// resolvedBinaryName returns the basename of the `module` directive in
// source/go.mod.
//
// It names the file inside the image and the SBOM documents beside it, and
// nothing else: the repository an app is published to is stated to Publish
// and is independent of it. That separation is the point — the two were one
// string before, so a project could not publish `hello` to
// `ghcr.io/z5labs/hello-service` without renaming its binary.
func (g *GoChain) resolvedBinaryName(ctx context.Context) (string, error) {
	contents, err := g.Source.File("go.mod").Contents(ctx)
	if err != nil {
		return "", fmt.Errorf("read go.mod to derive the binary name: %w", err)
	}
	modulePath, err := parseModuleDirective(contents)
	if err != nil {
		return "", fmt.Errorf("scan go.mod for module directive: %w", err)
	}
	if modulePath == "" {
		return "", fmt.Errorf("could not derive the binary name: missing module directive in go.mod")
	}
	name := basenameAfterSlash(modulePath)
	if name == "" {
		return "", fmt.Errorf("could not derive the binary name from module path %q", modulePath)
	}
	return name, nil
}

// buildBinaryForPlatform cross-compiles source against platform and returns
// the resulting binary as a *dagger.File. CGO is disabled and the binary is
// built with -trimpath and -s -w for reproducibility and size, and stamped
// with the caller's version and the commit read from HEAD.
//
// Stamping happens here, in the per-variant compile, and nowhere else: a
// multi-platform publish collapses the variants into a single manifest list,
// so a stamp applied at the image or publish layer would be applied once to
// an artifact that had already merged them.
func (g *GoChain) buildBinaryForPlatform(platform, pkg, binaryName, version, commit string) *dagger.File {
	return dag.Go().Build(g.Source, dagger.GoBuildOpts{
		Pkg:          pkg,
		ArtifactName: binaryName,
		Trimpath:     true,
		Strip:        true,
		DisableCgo:   true,
		Platform:     platform,
		Tags:         g.BuildTags,
		Stamps: []string{
			stampVersionVar + "=" + version,
			stampCommitVar + "=" + commit,
		},
	}).File(binaryName)
}

// imageForEntry packages entry as a scratch image pinned to platform. The
// platform option creates an empty container; we do not call
// From("scratch") because Docker's "scratch" is a base name, not a pullable
// image.
//
// The entrypoint is the executable's absolute path, so the app runs whatever
// PATH says. The environment is the module's standardized set and is set here
// rather than anywhere a caller can reach — see the constants above.
//
// It is written by iterating expectedImageEnv rather than by naming the
// variables again, so the set an image carries and the set a publish asserts
// are one list rather than two that have to agree. Adding a variable to the
// standardized set is then a one-line change that cannot half-land. The keys
// are applied in sorted order because an image config's environment is a list,
// so two builds of one commit have to write it the same way round.
//
// HOME's directory is created here too, empty and read-only. TMPDIR's is
// deliberately not — the package doc's "The image contract" carries why the
// two variables are treated differently.
//
// The entry lands owned by the image's non-root user and read-only, which is
// the same treatment every contributed byte gets and for the same reason: an
// image whose files the application can rewrite is one whose published digest
// stops describing what is running. It is 0555 rather than 0444 because this
// is the one file in the image that is exec'd.
//
// The image runs as that same user, appOwner, and this is the only line that
// sets it — see the package doc's "The image runs as 65532:65532".
//
// What that placement buys is precise, and worth stating precisely: every image
// *this module publishes* carries 65532:65532, and there is no method on App
// that moves it. It is not a claim that the string is out of a caller's reach —
// App.Container hands back a *dagger.Container, and WithUser is a method on it,
// so a caller can take that container, change its user and publish it
// themselves. The narrow claim is the one a Kubernetes runAsNonRoot policy can
// be written against, and the broad one is not true of any Dagger module.
//
// The mode is what makes
// the two independent, and deliberately so. 0555 is exec'able by *every* uid,
// so a deployment that overrides the user — `--user $(id -u):$(id -g)`, or a
// securityContext pinning something the cluster allocated — is running an
// ordinary configuration rather than working around this line.
//
// This is the only place an image is built, and everything reaches it —
// GoChain.App by way of AppBuilder, and AppBuilder by way of a caller's
// prebuilt executable. A caller-supplied entrypoint is not an exemption from
// any of the above; it is the same code with a different file in it.
//
// annotations are applied per variant rather than to the manifest list a
// publish assembles from them: a caller pulls one platform, and an
// annotation that lived only on the index would be invisible to everything
// that resolved a platform first. Keys are applied in sorted order so two
// builds of one commit produce the same manifest bytes.
func imageForEntry(platform dagger.Platform, name string, entry *dagger.File, annotations map[string]string) *dagger.Container {
	entrypoint := appDir + "/" + name
	ctr := dag.Container(dagger.ContainerOpts{Platform: platform}).
		WithFile(entrypoint, entry, dagger.ContainerWithFileOpts{
			Owner:       appOwner,
			Permissions: entryMode,
		}).
		WithDirectory(appHomeDir, imageHome())
	env := expectedImageEnv()
	for _, k := range sortedKeys(env) {
		ctr = ctr.WithEnvVariable(k, env[k])
	}
	ctr = ctr.WithEntrypoint([]string{entrypoint}).WithUser(appOwner)
	for _, k := range sortedKeys(annotations) {
		ctr = ctr.WithAnnotation(k, annotations[k])
	}
	return ctr
}

// parsePlatform splits a Dagger platform string ("goos/goarch" or
// "goos/goarch/variant", e.g. "linux/arm/v7") into GOOS and GOARCH.
// Variant segments past the first two are accepted and ignored — they're
// carried into the image manifest by dagger.Platform, but the Go toolchain
// only takes GOOS/GOARCH (GOARM/GOAMD64 are unset here; callers needing
// those can extend the API later).
func parsePlatform(p string) (goos, goarch string, err error) {
	parts := strings.Split(p, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid platform %q (expected GOOS/GOARCH[/variant])", p)
	}
	return parts[0], parts[1], nil
}

