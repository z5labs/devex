package main

import (
	"net/url"
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
// variant an App carries.
//
// Every value is a function of HEAD and of the version the caller stated,
// and of nothing else, which is what keeps two builds of one (commit,
// version) pair byte-identical:
//
//   - revision is the full HEAD SHA. The binary's commit stamp uses the
//     short SHA because it is read by people; the annotation is read by
//     tooling resolving a commit, so it carries the unabbreviated
//     identifier.
//   - created is the commit's committer time, not the time of the build.
//     Wall-clock time here would make every rebuild of one commit a
//     different manifest, which is the property this pipeline exists to
//     preserve.
//   - source is the origin remote's URL, with any credentials already
//     stripped. A source tree with no origin omits the key rather than
//     carrying an empty one — an annotation present and blank is worse than
//     absent, because a consumer cannot tell it apart from a repository
//     whose URL really is "".
//   - version is the caller's version. It is always set now: it used to be
//     present only when a tag pointed at HEAD, because that was the only
//     case in which the pipeline had a version worth the name.
//
// Every key but the version is omitted when the fact behind it was never
// observed, and the source key has always been. An App assembled from
// prebuilt executables read no working tree, so it has no revision and no
// commit time; a key present and blank would be worse than an absent one for
// exactly the reason the source key already gives, because a consumer cannot
// tell it apart from a revision that really is "". A language chain always
// has all three — gitFacts refuses a tree that cannot supply them — so
// nothing about the Go path changes here.
func ociAnnotations(facts gitState, version string) map[string]string {
	out := map[string]string{annotationVersion: version}
	if facts.SHA != "" {
		out[annotationRevision] = facts.SHA
	}
	if facts.Created != "" {
		out[annotationCreated] = facts.Created
	}
	if facts.SourceURI != "" {
		out[annotationSource] = facts.SourceURI
	}
	return out
}

// redactURLCredentials strips any userinfo from a URL.
//
// The origin remote is published as org.opencontainers.image.source, on a
// manifest anyone who can pull the image can read — and a CI checkout
// leaves credentials in that remote as a matter of course, so the
// annotation is a credential-exfiltration path unless the userinfo comes
// off. A URL that carried credentials keeps its host and path and loses
// only the part that was never meant to travel.
//
// Anything that does not parse as a URL is returned unchanged: an SSH
// remote is spelled `git@host:org/repo`, which is not a URL and carries a
// username that is part of the address rather than a secret.
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
