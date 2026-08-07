package main

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"dagger/z-5-labs/internal/dagger"
)

// GoApp is the application archetype. Construct via Z5labs.GoApp.
type GoApp struct {
	// +private
	Source *dagger.Directory
	// +private
	Pkg string
	// +private
	BinaryName string
	// +private
	PublishOn string
	// +private
	Registry string
	// +private
	AuthUsername string
	// +private
	Auth *dagger.Secret
	// +private
	LintConfig *dagger.File
	// +private
	LintVersion string
	// +private
	Platforms []string
	// +private
	RegistryService *dagger.Service
	// +private
	Insecure bool
	// +private
	IDTokenRequestURL string
	// +private
	IDTokenRequestToken *dagger.Secret
	// +private
	SigningKey *dagger.Secret
	// +private
	IDTokenService *dagger.Service
}

// Ci runs the standardized GoApp pipeline: verify .git exists, run the
// shared check stages (fmt+vet+lint+test -race) once, build a scratch
// image per platform, then conditionally publish per the publishOn
// filter.
//
// It returns the digest of what was published — the manifest list naming
// every platform variant, or the single image manifest when only one
// platform was built. Every matching ref publishes the same bytes under
// its own tag, so one digest describes them all. A run that publishes
// nothing — no ref matched, or no registry was configured — returns the
// empty string rather than an error.
//
// Returning the digest rather than only an error is what lets a caller
// reference what was published: an attestation, a deployment manifest or
// a release note has to name an immutable artifact, and a tag is not one.
//
// Every published image carries the standard OCI source annotations —
// revision, source, created, and version on a tag build — on each
// platform variant, and every published digest carries three
// attestations: an SPDX and a CycloneDX SBOM per platform, produced by
// the `go` module from the binaries this pipeline compiled, and a signed
// SLSA provenance statement whose build identity comes from an exchanged
// workload identity token. A publish that cannot produce provenance
// fails rather than publishing without it.
//
// Publish is a side-effecting operation against an external registry, so
// the whole pipeline is uncached — re-runs (e.g. after a retry, or after
// a new ref appears within the same engine session) must actually push.
//
// +check
// +cache="never"
func (a *GoApp) Ci(ctx context.Context) (string, error) {
	if err := requireGitWorkingTree(ctx, a.Source); err != nil {
		return "", err
	}
	binaryName, err := a.resolvedBinaryName(ctx)
	if err != nil {
		return "", err
	}
	if err := sharedCheck(ctx, a.Source, a.LintConfig, a.LintVersion); err != nil {
		return "", err
	}
	annotations, err := a.ociAnnotations(ctx)
	if err != nil {
		return "", err
	}
	// Build a scratch image per platform. Force evaluation via Sync so
	// build failures surface here, not during a later publish step.
	// The binaries are kept, not only the images: an SBOM describes the
	// compiled artifact, and recovering it from a published image would
	// mean pulling back bytes this pipeline already has in hand.
	variants := make([]*dagger.Container, 0, len(a.Platforms))
	binaries := make(map[string]*dagger.File, len(a.Platforms))
	for _, p := range a.Platforms {
		bin, err := a.buildBinaryForPlatform(ctx, p, binaryName)
		if err != nil {
			return "", err
		}
		binaries[p] = bin
		img := a.imageForPlatform(p, binaryName, bin, annotations)
		if _, err := img.Sync(ctx); err != nil {
			return "", fmt.Errorf("build %s: %v", p, err)
		}
		variants = append(variants, img)
	}
	matches, err := a.matchingRefs(ctx)
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", nil
	}
	if a.Registry != "" && a.Auth == nil {
		return "", fmt.Errorf("auth is required when registry is set")
	}
	if a.Registry == "" {
		return "", nil
	}
	// Provenance is resolved before the first byte is pushed, so a run
	// that cannot produce it fails without leaving a half-attested image
	// behind. It is also why this is not an "if configured" branch: an
	// attestation step that can be omitted is one that will be, and the
	// image published without it looks exactly like one published with.
	sgn, err := a.newSigner(ctx)
	if err != nil {
		return "", err
	}
	shortSha, commitISO, err := a.shortShaAndCommitTime(ctx)
	if err != nil {
		return "", err
	}
	username := a.AuthUsername
	if username == "" {
		username = "ci"
	}
	// The registry is the oci module's business, not this archetype's.
	// It knows that Container.Publish cannot see session service
	// bindings and works around it in pure Go; this pipeline only knows
	// which bytes to push and what to call them.
	registry := dag.Oci().Registry(a.Registry, dagger.OciRegistryOpts{
		Username: username,
		Password: a.Auth,
		Service:  a.RegistryService,
		Insecure: a.Insecure,
	})
	digest := ""
	tags := make([]string, 0, len(matches))
	for _, ref := range matches {
		tag, ok := imageTagFor(ref, shortSha, commitISO)
		if !ok {
			continue
		}
		// Every variant goes in one call, so a multi-platform build
		// publishes one manifest list naming them all rather than a
		// tag per architecture.
		pushed, err := registry.PushImage(ctx, binaryName, tag, variants)
		if err != nil {
			return "", fmt.Errorf("publish %s:%s: %v", binaryName, tag, err)
		}
		digest = pushed
		tags = append(tags, tag)
	}
	if digest == "" {
		return "", nil
	}
	version, _, _, err := a.buildIdentity(ctx)
	if err != nil {
		return "", err
	}
	// Every tag named the same bytes, so one set of attestations covers
	// them all: they anchor to the digest, which is what a tag resolves
	// to and what a consumer should be pinning anyway.
	facts := buildFacts{
		Repository: binaryName,
		Tags:       tags,
		Digest:     digest,
		Platforms:  a.Platforms,
		Pkg:        a.resolvedPkg(),
		BinaryName: binaryName,
		SourceURI:  annotations[annotationSource],
		Commit:     annotations[annotationRevision],
		Version:    version,
	}
	if err := a.attachAttestations(ctx, registry, sgn, facts, binaries); err != nil {
		return "", err
	}
	return digest, nil
}

// newSigner resolves the identity this publish signs its provenance
// with, and refuses the publish when the machinery to do so was not
// supplied.
//
// Refusing rather than skipping is the decision this function exists to
// make. An unattested image is indistinguishable from an attested one
// until someone goes looking, so "provenance when configured" is
// provenance nobody can rely on — and the reason GoApp exists at all is
// that a build step living outside the standard pipeline drifts out of
// it. The error names the missing inputs and how to obtain them, because
// the failure a caller hits is almost always a missing permission rather
// than a missing argument.
func (a *GoApp) newSigner(ctx context.Context) (*signer, error) {
	var missing []string
	if strings.TrimSpace(a.IDTokenRequestURL) == "" {
		missing = append(missing, "idTokenRequestUrl")
	}
	if a.IDTokenRequestToken == nil {
		missing = append(missing, "idTokenRequestToken")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf(
			"publishing requires provenance and provenance requires a workload identity token, but %s %s not set; "+
				"on GitHub Actions grant `permissions: id-token: write` and pass ACTIONS_ID_TOKEN_REQUEST_URL and "+
				"ACTIONS_ID_TOKEN_REQUEST_TOKEN, or on any other CI the equivalent OIDC token request endpoint and its bearer token",
			strings.Join(missing, " and "), pluralIsAre(len(missing)))
	}
	return newSigner(ctx, a.IDTokenRequestURL, a.IDTokenRequestToken, a.SigningKey, a.IDTokenService)
}

// pluralIsAre keeps the refusal message grammatical whether one input is
// missing or both. A message that reads like a template is one people
// stop reading.
func pluralIsAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// requireGitWorkingTree confirms source is a git working tree by
// accepting either a `.git` directory (normal clone) or a `.git` file
// (worktrees / submodules — where `.git` is a "gitdir: ..." pointer).
// Detection errors are wrapped so unrelated I/O failures surface.
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

// matchingRefs collects refs at HEAD, normalizes them, and filters by
// the publishOn regex.
func (a *GoApp) matchingRefs(ctx context.Context) ([]string, error) {
	pattern := a.PublishOn
	if pattern == "" {
		pattern = "^refs/heads/main$"
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("compile publishOn regex %q: %v", pattern, err)
	}
	refs, err := a.collectRefs(ctx)
	if err != nil {
		return nil, err
	}
	var matches []string
	for _, r := range refs {
		if re.MatchString(r) {
			matches = append(matches, r)
		}
	}
	return matches, nil
}

// collectRefs runs `git for-each-ref --points-at HEAD ...` inside a
// go-toolchain container (the golang image carries git) and returns the
// normalized list of refs at HEAD.
func (a *GoApp) collectRefs(ctx context.Context) ([]string, error) {
	out, err := dag.Go().Container(a.Source).
		WithExec([]string{
			"git", "for-each-ref",
			"--points-at", "HEAD",
			"--sort=-creatordate",
			"--format=%(refname)",
		}).
		Stdout(ctx)
	if err != nil {
		return nil, fmt.Errorf("git for-each-ref: %v", err)
	}
	return normalizeRefs(strings.Split(strings.TrimSpace(out), "\n")), nil
}

// shortShaAndCommitTime returns the short HEAD SHA and the commit's
// committer timestamp formatted as a docker-tag-safe ISO string.
// Sanitization: ":" and "+" become "-".
func (a *GoApp) shortShaAndCommitTime(ctx context.Context) (string, string, error) {
	ctr := dag.Go().Container(a.Source)
	sha, err := ctr.WithExec([]string{"git", "rev-parse", "--short", "HEAD"}).Stdout(ctx)
	if err != nil {
		return "", "", fmt.Errorf("git rev-parse: %v", err)
	}
	iso, err := ctr.WithExec([]string{"git", "show", "-s", "--format=%cI", "HEAD"}).Stdout(ctx)
	if err != nil {
		return "", "", fmt.Errorf("git show commit time: %v", err)
	}
	return strings.TrimSpace(sha), sanitizeDockerTag(strings.TrimSpace(iso)), nil
}

// sanitizeDockerTag maps a git ref name to a docker-tag-safe string.
// Docker's tag charset is [A-Za-z0-9_.-]; the first character may not
// be '.' or '-', and total length is capped at 128. Any character
// outside the charset is replaced with '-', a leading '.' or '-' is
// replaced with '_', and the result is truncated to 128 characters.
// Common git tag schemes like "release/v1.2.3" therefore map to
// "release-v1.2.3" rather than failing the publish.
func sanitizeDockerTag(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z',
			c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '_', c == '.', c == '-':
			b = append(b, c)
		default:
			b = append(b, '-')
		}
	}
	if len(b) > 0 && (b[0] == '.' || b[0] == '-') {
		b[0] = '_'
	}
	if len(b) > 128 {
		b = b[:128]
	}
	return string(b)
}

// imageTagFor maps a single ref to its image tag. Tags map to the
// stripped tag name; branches map to "<shortSha>-<isoCommitTime>".
// Returns ok=false for unsupported ref shapes (e.g. refs/stash).
func imageTagFor(ref, shortSha, commitISO string) (string, bool) {
	if t, ok := strings.CutPrefix(ref, "refs/tags/"); ok {
		return sanitizeDockerTag(t), true
	}
	if _, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return shortSha + "-" + commitISO, true
	}
	return "", false
}

// normalizeRefs maps refs/remotes/origin/X → refs/heads/X and dedups
// while preserving the input order.
func normalizeRefs(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		r := strings.TrimSpace(raw)
		if r == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(r, "refs/remotes/origin/"); ok {
			r = "refs/heads/" + rest
		}
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

// Builder returns the local-dev sibling that produces the same image CI
// would publish, single-arch (host platform).
func (a *GoApp) Builder() *Builder {
	return &Builder{App: a}
}

// resolvedBinaryName returns a.BinaryName if set; otherwise the basename
// of the `module` directive in source/go.mod.
func (a *GoApp) resolvedBinaryName(ctx context.Context) (string, error) {
	if a.BinaryName != "" {
		return a.BinaryName, nil
	}
	contents, err := a.Source.File("go.mod").Contents(ctx)
	if err != nil {
		return "", fmt.Errorf("read go.mod to derive binary name: %w", err)
	}
	modulePath, err := parseModuleDirective(contents)
	if err != nil {
		return "", fmt.Errorf("scan go.mod for module directive: %w", err)
	}
	if modulePath == "" {
		return "", fmt.Errorf("could not derive binary name: missing module directive in go.mod")
	}
	name := basenameAfterSlash(modulePath)
	if name == "" {
		return "", fmt.Errorf("could not derive binary name from module path %q", modulePath)
	}
	return name, nil
}

// resolvedPkg returns a.Pkg if set; otherwise ".".
func (a *GoApp) resolvedPkg() string {
	if a.Pkg == "" {
		return "."
	}
	return a.Pkg
}

// buildBinaryForPlatform cross-compiles source against platform
// (formatted "<goos>/<goarch>") and returns the resulting binary as a
// *dagger.File. CGO is disabled and the binary is built with -trimpath
// and -s -w for reproducibility and size, and stamped with the version
// and commit derived from HEAD.
//
// Stamping happens here, in the per-variant compile, and nowhere else:
// Ci collapses its per-platform images into a single manifest list
// before publishing, so a stamp applied at the image or publish layer
// would be applied once to an artifact that already merged the variants.
func (a *GoApp) buildBinaryForPlatform(ctx context.Context, platform, binaryName string) (*dagger.File, error) {
	// Validate here rather than leaving it to Go.Build: Build's error
	// surfaces only when the returned directory is evaluated, which is
	// several steps further from the caller that supplied the platform.
	if _, _, err := parsePlatform(platform); err != nil {
		return nil, err
	}
	version, commit, err := a.stampValues(ctx)
	if err != nil {
		return nil, err
	}
	return dag.Go().Build(a.Source, dagger.GoBuildOpts{
		Pkg:          a.resolvedPkg(),
		ArtifactName: binaryName,
		Trimpath:     true,
		Strip:        true,
		DisableCgo:   true,
		Platform:     platform,
		Stamps: []string{
			stampVersionVar + "=" + version,
			stampCommitVar + "=" + commit,
		},
	}).File(binaryName), nil
}

// stampVersionVar and stampCommitVar are the linker symbols every binary
// GoApp builds is stamped with. They are fixed by the module rather than
// chosen per application, so every z5labs Go application answers "which
// build am I running" the same way.
const (
	stampVersionVar = "main.version"
	stampCommitVar  = "main.commit"
)

// stampValues returns the version and commit stamped into every binary
// this GoApp builds, both derived from HEAD and from nothing else.
//
// version follows the same rule as imageTagFor, so a binary's reported
// version and the tag of the image carrying it agree by construction: a
// tag pointing at HEAD gives the stripped tag name, anything else gives
// "<shortSha>-<isoCommitTime>". The refs are read unfiltered — publishOn
// decides what gets published, not what gets stamped, so a build that
// will never publish is stamped exactly like one that will.
//
// commit is the short HEAD SHA. Both values are functions of the commit
// alone, which is what makes two builds of one commit byte-identical.
func (a *GoApp) stampValues(ctx context.Context) (version, commit string, err error) {
	version, commit, _, err = a.buildIdentity(ctx)
	return version, commit, err
}

// buildIdentity is stampValues plus the one fact its callers cannot
// recover from its result: whether version came from a tag pointing at
// HEAD. "v1.2.3" and "abc1234-2026-01-01T00-00-00Z" are both just
// strings once returned, and the OCI version annotation is meaningful
// only for the first kind.
func (a *GoApp) buildIdentity(ctx context.Context) (version, commit string, fromTag bool, err error) {
	shortSha, commitISO, err := a.shortShaAndCommitTime(ctx)
	if err != nil {
		return "", "", false, fmt.Errorf("could not derive build stamp from source: %v", err)
	}
	refs, err := a.collectRefs(ctx)
	if err != nil {
		return "", "", false, fmt.Errorf("could not derive build stamp from source: %v", err)
	}
	for _, ref := range refs {
		if !strings.HasPrefix(ref, "refs/tags/") {
			continue
		}
		tag, ok := imageTagFor(ref, shortSha, commitISO)
		if !ok {
			continue
		}
		return tag, shortSha, true, nil
	}
	return shortSha + "-" + commitISO, shortSha, false, nil
}

// imageForPlatform packages binary as a scratch image pinned to
// platform, with /app/<binaryName> as the entrypoint. The platform
// option creates an empty container; we do not call From("scratch")
// because Docker's "scratch" is a base name, not a pullable image.
//
// annotations are applied per variant rather than to the manifest list
// Ci assembles from them: a caller pulls one platform, and an annotation
// that lived only on the index would be invisible to everything that
// resolved a platform first. Keys are applied in sorted order so two
// builds of one commit produce the same manifest bytes.
func (a *GoApp) imageForPlatform(platform, binaryName string, binary *dagger.File, annotations map[string]string) *dagger.Container {
	ctr := dag.Container(dagger.ContainerOpts{Platform: dagger.Platform(platform)}).
		WithFile("/app/"+binaryName, binary).
		WithEntrypoint([]string{"/app/" + binaryName})
	keys := make([]string, 0, len(annotations))
	for k := range annotations {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ctr = ctr.WithAnnotation(k, annotations[k])
	}
	return ctr
}

// parsePlatform splits a Dagger platform string ("goos/goarch" or
// "goos/goarch/variant", e.g. "linux/arm/v7") into GOOS and GOARCH.
// Variant segments past the first two are accepted and ignored —
// they're carried into the image manifest by dagger.Platform, but the
// Go toolchain only takes GOOS/GOARCH (GOARM/GOAMD64 are unset here;
// callers needing those can extend the API later).
func parsePlatform(p string) (goos, goarch string, err error) {
	parts := strings.Split(p, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid platform %q (expected GOOS/GOARCH[/variant])", p)
	}
	return parts[0], parts[1], nil
}
