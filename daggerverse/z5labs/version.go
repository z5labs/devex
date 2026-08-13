package main

import (
	"fmt"
	"strings"
)

// maxTagLength is the OCI image tag length limit. A tag is
// `[A-Za-z0-9_][A-Za-z0-9._-]{0,127}`, so 128 characters in total.
const maxTagLength = 128

// validateVersion accepts any version that can be an image tag verbatim,
// and refuses everything else rather than rewriting it.
//
// The old pipeline derived the version from HEAD and sanitized whatever it
// found, mapping any character outside the tag charset to "-". That is the
// right trade for a value the pipeline invented — nobody typed
// "release/v1.2.3" expecting it back — and the wrong one for a value the
// caller states: two versions that differ only outside the charset would
// sanitize to one tag, and the second publish would silently replace the
// first.
//
// SemVer build metadata gets its own message because it is the case a
// releaser will actually hit. "1.0.0+build.7" is a legal SemVer and an
// illegal tag, and the two builds it distinguishes are exactly the two a
// silent rewrite would collapse.
func validateVersion(version string) error {
	if version == "" {
		return fmt.Errorf("version is required: it is what every binary is stamped with and what every image is published under")
	}
	if before, after, ok := strings.Cut(version, "+"); ok {
		return fmt.Errorf(
			"version %q carries SemVer build metadata, which cannot be an image tag: \"+\" is not in the OCI tag charset, "+
				"and dropping it would publish %q and any other %q+... build under the one tag %q, each silently replacing the last; "+
				"release %q, or fold %q into the version itself",
			version, version, before, before, before, after)
	}
	if len(version) > maxTagLength {
		return fmt.Errorf("version %q is %d characters, which cannot be an image tag: the OCI tag limit is %d", version, len(version), maxTagLength)
	}
	if first := version[0]; !isTagAlnum(first) && first != '_' {
		return fmt.Errorf(
			"version %q cannot be an image tag: an OCI tag starts with a letter, a digit or \"_\", and %q does not",
			version, string(first))
	}
	for i := 0; i < len(version); i++ {
		c := version[i]
		if isTagAlnum(c) || c == '.' || c == '-' || c == '_' {
			continue
		}
		return fmt.Errorf(
			"version %q cannot be an image tag: %q is not in the OCI tag charset, which is letters, digits, \".\", \"-\" and \"_\"",
			version, string(c))
	}
	return nil
}

// isTagAlnum reports whether c is an ASCII letter or digit. The tag charset
// is ASCII-only, so this deliberately does not consult unicode.
func isTagAlnum(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}

// validateRepository refuses a repository that is really a reference.
//
// Publish takes a repository *path* and appends it to the registry address
// WithRegistry was given, because a mirror or an internal registry serves
// the same release and the address is the part that moves. A caller who
// passes "ghcr.io/z5labs/app" would otherwise publish to
// "ghcr.io/ghcr.io/z5labs/app" — which succeeds, and is discovered by
// somebody failing to pull it.
func validateRepository(repository string) error {
	if strings.TrimSpace(repository) == "" {
		return fmt.Errorf("repository is required: it is the path appended to the registry address")
	}
	if strings.HasPrefix(repository, "/") || strings.HasSuffix(repository, "/") {
		return fmt.Errorf("repository %q must not start or end with %q: it is a path appended to the registry address", repository, "/")
	}
	if strings.ContainsAny(repository, "@") {
		return fmt.Errorf("repository %q must not carry a digest: Publish returns the digest it published, it does not take one", repository)
	}
	if strings.Contains(repository, ":") {
		return fmt.Errorf(
			"repository %q must not carry a tag: the tag is the version this app was built with, stated once to App",
			repository)
	}
	head, _, _ := strings.Cut(repository, "/")
	if strings.Contains(head, ".") {
		return fmt.Errorf(
			"repository %q looks like it starts with a registry address: repository is the path appended to the address given to withRegistry, "+
				"so pass the registry there and only %q here",
			repository, strings.TrimPrefix(repository, head+"/"))
	}
	return nil
}
