package main

import (
	"context"
	"fmt"
	"regexp"
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
	Platforms []string
	// +private
	RegistryService *dagger.Service
}

// Ci runs the standardized GoApp pipeline: verify .git exists, run the
// shared check stages (fmt+vet+lint+test -race) once, build a scratch
// image per platform, then conditionally publish per the publishOn
// filter.
//
// Publish is a side-effecting operation against an external registry, so
// the whole pipeline is uncached — re-runs (e.g. after a retry, or after
// a new ref appears within the same engine session) must actually push.
//
// +check
// +cache="never"
func (a *GoApp) Ci(ctx context.Context) error {
	if err := requireGitWorkingTree(ctx, a.Source); err != nil {
		return err
	}
	binaryName, err := a.resolvedBinaryName(ctx)
	if err != nil {
		return err
	}
	if err := sharedCheck(ctx, a.Source, a.LintConfig); err != nil {
		return err
	}
	// Build a scratch image per platform. Force evaluation via Sync so
	// build failures surface here, not during a later publish step.
	variants := make([]*dagger.Container, 0, len(a.Platforms))
	for _, p := range a.Platforms {
		bin, err := a.buildBinaryForPlatform(ctx, p, binaryName)
		if err != nil {
			return err
		}
		img := a.imageForPlatform(p, binaryName, bin)
		if _, err := img.Sync(ctx); err != nil {
			return fmt.Errorf("build %s: %v", p, err)
		}
		variants = append(variants, img)
	}
	matches, err := a.matchingRefs(ctx)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		return nil
	}
	if a.Registry != "" && a.Auth == nil {
		return fmt.Errorf("auth is required when registry is set")
	}
	if a.Registry == "" {
		return nil
	}
	shortSha, commitISO, err := a.shortShaAndCommitTime(ctx)
	if err != nil {
		return err
	}
	primary, others := variants[0], variants[1:]
	username := a.AuthUsername
	if username == "" {
		username = "ci"
	}
	// Materialize the multi-platform image as an OCI tarball, then push
	// it via skopeo inside a sidecar container. Container.Publish runs
	// in the engine's BuildKit context, which does not see session
	// service bindings — so we cannot use it to reach a Dagger-hosted
	// registry. Skopeo in a service-bound container CAN reach it.
	tarball := primary.AsTarball(dagger.ContainerAsTarballOpts{
		PlatformVariants: others,
	})
	pusher := dag.Container().From(skopeoImage).
		WithFile("/img.tar", tarball).
		WithEnvVariable("REGISTRY_USERNAME", username).
		WithSecretVariable("REGISTRY_PASSWORD", a.Auth)
	// TLS verification stays on for real registries; disable it only
	// when the caller wired in a registryService (a Dagger-hosted
	// registry:2 over plain HTTP for tests).
	tlsFlag := "--dest-tls-verify=true"
	if a.RegistryService != nil {
		pusher = pusher.WithServiceBinding(registryServiceAlias, a.RegistryService)
		tlsFlag = "--dest-tls-verify=false"
	}
	for _, ref := range matches {
		tag, ok := imageTagFor(ref, shortSha, commitISO)
		if !ok {
			continue
		}
		image := fmt.Sprintf("%s/%s:%s", a.Registry, binaryName, tag)
		// --dest-creds reads from env via shell expansion; multi-arch
		// images carry all variants in the OCI archive (--all copies
		// every manifest in the source).
		cmd := fmt.Sprintf(
			`skopeo copy --all %s --dest-creds="$REGISTRY_USERNAME:$REGISTRY_PASSWORD" oci-archive:/img.tar docker://%s`,
			tlsFlag, image,
		)
		if _, err := pusher.WithExec([]string{"sh", "-c", cmd}).Sync(ctx); err != nil {
			return fmt.Errorf("publish %s: %v", image, err)
		}
	}
	return nil
}

// registryServiceAlias is the WithServiceBinding name used when the
// caller supplies a registryService. Tests bind their local registry:2
// under this same alias and use it as the registry hostname.
const registryServiceAlias = "registry"

// skopeoImage is pinned to a specific stable tag; ":latest" produces
// non-reproducible builds and can break unexpectedly on upstream
// rebuilds.
const skopeoImage = "quay.io/skopeo/stable:v1.22.2"

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

// sanitizeDockerTag replaces characters disallowed in docker tags
// (":" and "+") with "-".
func sanitizeDockerTag(s string) string {
	r := strings.NewReplacer(":", "-", "+", "-")
	return r.Replace(s)
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
	modulePath := parseModuleDirective(contents)
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
// and -s -w for reproducibility and size.
func (a *GoApp) buildBinaryForPlatform(_ context.Context, platform, binaryName string) (*dagger.File, error) {
	goos, goarch, err := parsePlatform(platform)
	if err != nil {
		return nil, err
	}
	out := "/out/" + binaryName
	ctr := dag.Go().Container(a.Source).
		WithEnvVariable("GOOS", goos).
		WithEnvVariable("GOARCH", goarch).
		WithEnvVariable("CGO_ENABLED", "0").
		WithExec([]string{"go", "build", "-trimpath", "-ldflags", "-s -w", "-o", out, a.resolvedPkg()})
	return ctr.File(out), nil
}

// imageForPlatform packages binary as a scratch image pinned to
// platform, with /app/<binaryName> as the entrypoint. The platform
// option creates an empty container; we do not call From("scratch")
// because Docker's "scratch" is a base name, not a pullable image.
func (a *GoApp) imageForPlatform(platform, binaryName string, binary *dagger.File) *dagger.Container {
	return dag.Container(dagger.ContainerOpts{Platform: dagger.Platform(platform)}).
		WithFile("/app/"+binaryName, binary).
		WithEntrypoint([]string{"/app/" + binaryName})
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
