package main

import (
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
// A string that does not parse as a URL is not therefore safe, and treating
// it as safe is what devex#425 was: the fallback returned the input
// untouched, and the inputs that reach it include the ones url.Parse refuses
// *because of the credential inside them*. Any percent not followed by two
// hex digits, and any control character, in the userinfo is enough — so the
// correctly encoded credential was redacted and the naive one was published,
// which inverts the purpose of the function in the one case it cannot parse
// its way out of.
//
// So an unparseable string is read structurally instead. RFC 3986 puts the
// userinfo in the authority, and the authority is present only where the
// hierarchical part begins with `//`:
//
//   - No authority: returned unchanged. This is the case the old fallback was
//     written for and it is still the right answer — an SSH remote is spelled
//     `git@host:org/repo`, which is not a URL, has no authority, and carries a
//     username that is part of the address rather than a secret.
//   - An authority with no `@`: returned unchanged. It failed to parse for a
//     reason somewhere else in the string, and there is no credential in it to
//     omit an annotation over.
//   - An authority with an `@`: the userinfo is cut out textually, and the
//     result is then re-parsed. It is published only if it parses and carries
//     no userinfo; otherwise the empty string is returned and the annotation is
//     omitted altogether.
//
// That last clause is the decision devex#425 asked for, and it is the
// conservative half of the rule rather than the useful one. A textual cut over
// a string no parser would accept is a claim about a shape nothing validated;
// re-parsing is what turns it into something checked, and where it cannot be
// checked, an omitted annotation is the safe outcome. It is the rule
// ociAnnotations already states for a tree with no origin — a key present and
// wrong is worse than an absent one — applied to a value that might be a
// credential rather than to one that is merely blank.
//
// Nothing here reports why a string would not parse. url.Parse's error quotes
// the whole input, credential included, so a redactor that explained itself
// would leak by exactly the path this function closes.
//
// SourceRedactionSelfTest is the table this behaviour is stated in.
func redactURLCredentials(raw string) string {
	if raw == "" {
		return ""
	}
	if parsed, err := url.Parse(raw); err == nil {
		if parsed.User == nil {
			return raw
		}
		parsed.User = nil
		return parsed.String()
	}

	stripped, hadUserinfo := stripAuthorityUserinfo(raw)
	if !hadUserinfo {
		return raw
	}
	if parsed, err := url.Parse(stripped); err != nil || parsed.User != nil {
		return ""
	}
	return stripped
}

// stripAuthorityUserinfo removes the userinfo from raw's authority
// component, textually, and reports whether there was one to remove.
//
// It exists for strings url.Parse has already refused, so it works from the
// grammar rather than from a parse. Per RFC 3986 the authority is present
// only when the hierarchical part opens with `//`; it runs to the first `/`,
// `?` or `#` after that, and within it the userinfo ends at an `@`.
//
// Two details are load bearing. The `//` has to open the hierarchical part —
// at the start of the string, or immediately after a scheme — because a
// doubled slash inside a path is not an authority, and reading one as an
// authority would rewrite an SSH remote like `git@host:org//repo` into
// something that is not the address it names. And the split is at the **last**
// `@` in the authority, which is where url.Parse itself splits: `@` is not
// legal unescaped in userinfo, so a string carrying two of them is one of the
// unparseable inputs this path exists for, and splitting at the first would
// leave the tail of the credential in the value.
func stripAuthorityUserinfo(raw string) (string, bool) {
	rest := raw
	offset := 0
	if colon := strings.IndexByte(raw, ':'); colon > 0 && isURLScheme(raw[:colon]) {
		offset = colon + 1
		rest = raw[offset:]
	}
	if !strings.HasPrefix(rest, "//") {
		return raw, false
	}

	start := offset + 2
	end := len(raw)
	if i := strings.IndexAny(raw[start:], "/?#"); i >= 0 {
		end = start + i
	}
	at := strings.LastIndexByte(raw[start:end], '@')
	if at < 0 {
		return raw, false
	}
	return raw[:start] + raw[start+at+1:], true
}

// isURLScheme reports whether s is a URL scheme: an ASCII letter followed by
// letters, digits, `+`, `-` and `.`, per RFC 3986.
//
// It is what keeps the colon in `git@host:org/repo` from being read as a
// scheme separator — `git@host` is not a scheme, so that remote has no
// authority and is left alone.
func isURLScheme(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z':
		case i > 0 && ('0' <= c && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return true
}
