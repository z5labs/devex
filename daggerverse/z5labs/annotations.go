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
//   - An authority carrying an `@`: the userinfo is cut out textually, and the
//     result is then re-parsed. It is published only if it parses and carries
//     no userinfo; otherwise the empty string is returned and the annotation is
//     omitted altogether.
//   - An authority with no `@` in it, but an `@` somewhere later in the string:
//     omitted. The delimiters that end an authority are `/`, `?` and `#`, and
//     an unencoded one of those inside a password puts the `@` beyond them —
//     `https://user:AB/CD@host/org/repo` has the authority `user:AB` by the
//     grammar and the whole credential by intent. In a string no parser
//     accepted, those two readings cannot be told apart, so neither is
//     published.
//   - Anything else: returned unchanged. There is no `@` after the authority
//     begins, so there is no userinfo to omit an annotation over.
//
// The two omitting clauses are the decision devex#425 asked for, and they are
// the conservative half of the rule rather than the useful one. A textual cut
// over a string no parser would accept is a claim about a shape nothing
// validated; re-parsing is what turns it into something checked, and where it
// cannot be checked, an omitted annotation is the safe outcome. It is the rule
// ociAnnotations already states for a tree with no origin — a key present and
// wrong is worse than an absent one — applied to a value that might be a
// credential rather than to one that is merely blank.
//
// The price is paid by a URL that fails to parse for some unrelated reason and
// happens to carry an `@` in its path: its annotation is dropped even though
// nothing in it was ever secret. That is the trade taken deliberately. The
// alternative — cutting at the last `@` anywhere and publishing whatever
// parses — turns `https://host/or%zg/re@po.git` into `https://po.git`, which is
// an annotation that is present and wrong.
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

	stripped, outcome := stripAuthorityUserinfo(raw)
	switch outcome {
	case redactionNone:
		return raw
	case redactionUnverifiable:
		return ""
	}
	if parsed, err := url.Parse(stripped); err != nil || parsed.User != nil {
		return ""
	}
	return stripped
}

// redaction is what stripAuthorityUserinfo made of a string url.Parse had
// already refused. There are three answers and not two, because "I found no
// userinfo" and "I cannot establish that there is none" are different findings
// and only the first one is safe to publish.
type redaction int

const (
	// redactionNone: nothing in the string is userinfo, so it stands as it is.
	redactionNone redaction = iota
	// redactionCut: userinfo was removed. The result is a candidate and not yet
	// an answer — the caller re-parses it before publishing it.
	redactionCut
	// redactionUnverifiable: there is an `@` no reading of this string accounts
	// for, so the caller publishes nothing.
	redactionUnverifiable
)

// stripAuthorityUserinfo removes the userinfo from raw's authority component,
// textually, and reports what it was able to establish.
//
// It exists for strings url.Parse has already refused, so it works from the
// grammar rather than from a parse. Per RFC 3986 the authority is present only
// when the hierarchical part opens with `//`; it runs to the first `/`, `?` or
// `#` after that, and within it the userinfo ends at an `@`.
//
// Three details are load bearing.
//
// The `//` has to open the hierarchical part — at the start of the string, or
// immediately after the colon that ends a scheme — because a doubled slash
// inside a path is not an authority, and reading one as an authority would
// rewrite an SSH remote like `git@host:org//repo` into something that is not
// the address it names.
//
// The split is at the **last** `@` in the authority, which is where url.Parse
// itself splits. A second `@` is not legal unescaped in userinfo, so a string
// carrying two of them is one of the unparseable inputs this path exists for,
// and splitting at the first would leave the tail of the credential in the
// value.
//
// And an `@` that falls *outside* the authority is reported as unverifiable
// rather than as an absence. The delimiter that ended the authority may itself
// have been part of a password nobody encoded — `/` is in the base64 alphabet
// that tokens are drawn from, so it is an ordinary thing for a password to
// contain and the single most likely reason a pasted https remote fails to
// parse. Treating that as "no userinfo here" is the devex#425 bug in a second
// costume: the string comes back untouched with the credential in it.
func stripAuthorityUserinfo(raw string) (string, redaction) {
	start, ok := authorityStart(raw)
	if !ok {
		return raw, redactionNone
	}

	end := len(raw)
	if i := strings.IndexAny(raw[start:], "/?#"); i >= 0 {
		end = start + i
	}
	if at := strings.LastIndexByte(raw[start:end], '@'); at >= 0 {
		return raw[:start] + raw[start+at+1:], redactionCut
	}
	if strings.IndexByte(raw[start:], '@') >= 0 {
		return "", redactionUnverifiable
	}
	return raw, redactionNone
}

// authorityStart returns the index at which raw's authority component begins,
// and whether raw has one at all.
//
// The authority opens the hierarchical part, so its `//` sits at the start of
// the string or directly after the colon that ends the scheme. Everything else
// spelled with a doubled slash is a path, and the case that matters is
// `git@host:org//repo`: reading that path's `//` as an authority would find an
// `@` before it and cut the address in half.
//
// An empty scheme is admitted — `://user:secret@host/path` is not a legal
// URI-reference and no tool produces it, but it is a shape carrying a
// credential, and the point of this path is that it runs on strings nothing
// accepted.
func authorityStart(raw string) (int, bool) {
	slashes := strings.Index(raw, "//")
	if slashes < 0 {
		return 0, false
	}
	prefix := raw[:slashes]
	if prefix == "" {
		return len("//"), true
	}
	if !strings.HasSuffix(prefix, ":") {
		return 0, false
	}
	if scheme := prefix[:len(prefix)-1]; scheme != "" && !isURLScheme(scheme) {
		return 0, false
	}
	return slashes + len("//"), true
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
