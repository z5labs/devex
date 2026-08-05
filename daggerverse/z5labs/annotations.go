package main

import (
	"context"
	"fmt"
	"strings"
)

// The standard OCI annotation keys this module populates. They are the
// pre-defined keys from the image spec rather than a z5labs namespace,
// because the value of an annotation is that a tool nobody here wrote
// already knows how to read it.
//
// https://github.com/opencontainers/image-spec/blob/main/annotations.md
const (
	annotationRevision = "org.opencontainers.image.revision"
	annotationSource   = "org.opencontainers.image.source"
	annotationCreated  = "org.opencontainers.image.created"
	annotationVersion  = "org.opencontainers.image.version"
)

// ociAnnotations returns the source annotations stamped onto every image
// variant this GoApp builds.
//
// Every value is a function of HEAD and of nothing else, which is what
// keeps two builds of one commit byte-identical:
//
//   - revision is the full HEAD SHA. The image tag and the binary stamp
//     both use the short SHA because a docker tag is read by people; the
//     annotation is read by tooling resolving a commit, so it carries the
//     unabbreviated identifier.
//   - created is the commit's committer time, not the time of the build.
//     Wall-clock time here would make every rebuild of one commit a
//     different manifest, which is the property GoApp exists to preserve.
//   - source is the origin remote's URL. A source tree with no origin
//     omits the key rather than carrying an empty one — an annotation
//     present and blank is worse than absent, because a consumer cannot
//     tell it apart from a repository whose URL really is "".
//   - version is set only when a tag points at HEAD, and is the same
//     string the binary reports as main.version and the image carries as
//     its tag. A branch build has no version to name.
func (a *GoApp) ociAnnotations(ctx context.Context) (map[string]string, error) {
	sha, created, source, err := a.headAnnotationFacts(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{
		annotationRevision: sha,
		annotationCreated:  created,
	}
	if source != "" {
		out[annotationSource] = source
	}
	version, _, fromTag, err := a.buildIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if fromTag {
		out[annotationVersion] = version
	}
	return out, nil
}

// headAnnotationFacts reads the full HEAD SHA, the committer time in
// RFC 3339 and the origin remote's URL out of the source tree.
//
// One exec rather than three: each is a separate container round trip
// otherwise, and these three values are always wanted together. The
// origin lookup is allowed to fail — `git config --get` exits 1 for a
// key that is not set, which is a repository without an origin and not
// an error — so its status is discarded and an empty line is what a
// missing origin looks like to the parser.
func (a *GoApp) headAnnotationFacts(ctx context.Context) (sha, created, source string, err error) {
	out, execErr := dag.Go().Container(a.Source).
		WithExec([]string{"sh", "-c", `
set -e
printf 'sha=%s\n' "$(git rev-parse HEAD)"
printf 'created=%s\n' "$(git show -s --format=%cI HEAD)"
printf 'source=%s\n' "$(git config --get remote.origin.url || true)"
`}).
		Stdout(ctx)
	if execErr != nil {
		return "", "", "", fmt.Errorf("read git state for image annotations: %v", execErr)
	}
	fields := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		fields[key] = value
	}
	if fields["sha"] == "" {
		return "", "", "", fmt.Errorf("read git state for image annotations: no HEAD commit in %q", out)
	}
	if fields["created"] == "" {
		return "", "", "", fmt.Errorf("read git state for image annotations: no commit time in %q", out)
	}
	return fields["sha"], fields["created"], fields["source"], nil
}
