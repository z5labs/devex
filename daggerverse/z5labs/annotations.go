package main

import (
	"context"
	"fmt"
	"net/url"
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
	// The failures below name the field and not the output. `git config
	// --get remote.origin.url` routinely returns a URL with credentials
	// in it — `https://x-access-token:<token>@host/org/repo` is what a
	// GitHub Actions checkout leaves behind — and an error message is
	// the least controlled output this module has.
	if fields["sha"] == "" {
		return "", "", "", fmt.Errorf("read git state for image annotations: HEAD names no commit")
	}
	if fields["created"] == "" {
		return "", "", "", fmt.Errorf("read git state for image annotations: HEAD carries no commit time")
	}
	return fields["sha"], fields["created"], redactURLCredentials(fields["source"]), nil
}

// redactURLCredentials strips any userinfo from a URL.
//
// The origin remote is published as org.opencontainers.image.source, on
// a manifest anyone who can pull the image can read — and a CI checkout
// leaves credentials in that remote as a matter of course, so the
// annotation is a credential-exfiltration path unless the userinfo comes
// off. A URL that carried credentials keeps its host and path and loses
// only the part that was never meant to travel.
//
// Anything that does not parse as a URL is returned unchanged: an SSH
// remote is spelled `git@host:org/repo`, which is not a URL and carries
// a username that is part of the address rather than a secret.
func redactURLCredentials(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return raw
	}
	parsed.User = nil
	return parsed.String()
}
