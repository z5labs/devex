package main

import (
	"strings"
)

// latestTag is the widest moving tag: whatever the most recent release
// published. It is a fixed name rather than a configurable one because a
// consumer who writes `FROM app` and gets `latest` is not reading this
// module's configuration.
const latestTag = "latest"

// versionTags is the family of tags one release is published under, derived
// from the version and from nothing else.
//
// # The family is the archetype's, not the caller's
//
// A consumer pins at the level of risk they are willing to take, and the
// four levels are the same in every project:
//
//	v1.2.3   never moves
//	v1.2     picks up patches
//	v1       picks up minor releases
//	latest   picks up everything
//
// Which of those exist is decided here rather than supplied by the caller,
// because a family stated per project is one every project can get wrong —
// and a consumer cannot tell a project that publishes no `v1` from one whose
// `v1` has stopped moving. The cost is a project whose scheme is not SemVer,
// and that case is served by the last rule below rather than by an option.
//
// # What the version says
//
//   - A SemVer release publishes the whole family, every tag naming one
//     digest.
//   - A SemVer *prerelease* publishes its own full version tag and nothing
//     else. `v1.3.0-rc.1` is precisely the release that must not be handed to
//     everybody pinning `v1`, which is what moving a moving tag would do.
//   - A version that is not SemVer publishes as a single tag and moves
//     nothing. That is the high-frequency-install case — no semantic
//     versioning, many builds, a version like `abc1234-2026-01-01T00-00-00Z`
//     — and it is a first-class use rather than a degraded one: it is also
//     exactly the behaviour every caller had before this family existed.
//
// A `v` prefix is preserved rather than normalized away: `v1.2.3` gives
// `v1.2` and `v1`, `1.2.3` gives `1.2` and `1`. Rewriting it would publish a
// project's moving tags under names nobody in that project uses.
//
// # What a pure function of one version cannot see
//
// This derivation reads the version and never the registry, so it cannot
// know that a *newer* release already moved the tags it is about to write.
// Publishing v1.2.3 after v1.3.0 has shipped walks `v1` and `latest`
// backwards, and every consumer pinning them is downgraded. Nothing here
// prevents that, deliberately: the alternative is a publish that resolves
// each moving tag first and silently declines to move some of them, which
// makes "the release published" mean something different depending on what
// was already there.
//
// For a release published out of *sequence* — a re-run, a version tagged
// late — the answer is to release in order, or to publish it as a
// prerelease, which moves nothing. **A maintenance release is neither**, and
// it is the case this rule genuinely does not serve: v1.9.1 published after
// v2.0.0 is a backport, `v1.9` and `v1` moving forward is exactly right, and
// `latest` regressing to a 1.x is exactly wrong. Publishing it as a
// prerelease is not a workaround either, because that changes the immutable
// tag consumers were told to pin. This module publishes `latest` anyway
// today, because the family is the one the archetype states and a `latest`
// that sometimes exists is worse than one that always does; devex#417 is
// where the maintenance case gets decided, and until it does, a project
// backporting across majors should know that its `latest` follows the last
// publish rather than the highest version.
//
// The error is validateVersion's, unchanged. It is re-run here because
// refusing SemVer build metadata is a property of publishing rather than of
// one constructor: `+` is not in the OCI tag charset, so a version carrying
// it has no tag family and no single tag either, and admitting it here would
// undo the refusal App already made.
func versionTags(version string) ([]string, error) {
	if err := validateVersion(version); err != nil {
		return nil, err
	}
	sv, ok := parseSemver(version)
	if !ok || sv.Prerelease != "" {
		return []string{version}, nil
	}
	return []string{
		version,
		sv.Prefix + sv.Major + "." + sv.Minor,
		sv.Prefix + sv.Major,
		latestTag,
	}, nil
}

// semver is a version this module was able to read as SemVer. The numeric
// fields stay strings because nothing here does arithmetic on them and
// re-rendering an int is how "01" would quietly become "1".
type semver struct {
	// Prefix is "v" when the version carried one, and "" otherwise. It is
	// carried so the derived tags spell the version the way the project
	// does.
	Prefix string
	Major  string
	Minor  string
	// Patch is read by nothing: no tag in the family is derived from it,
	// because the tag that would be is the version itself. It is kept so this
	// type is the version rather than the part of it the family happens to
	// use — a rule that compares two releases needs all three.
	Patch string
	// Prerelease is what followed the first "-", empty for a release.
	Prerelease string
}

// parseSemver reads version as SemVer, reporting whether it could.
//
// It is deliberately strict — three numeric identifiers with no leading
// zeros, and a well-formed prerelease if there is one — because failing to
// parse is the *safe* outcome here: an unparsed version publishes one tag
// and moves nothing, while a loose parse invents moving tags for a version
// whose scheme this module guessed at. `2026.08.12` is the case that makes
// the point: SemVer says "08" is not a numeric identifier, so a date-shaped
// version publishes as itself rather than moving a `2026.08` nobody asked
// for.
//
// Build metadata is refused rather than parsed. It cannot reach here through
// versionTags, which validates first, but a parser that quietly accepted it
// would make the refusal depend on call order.
func parseSemver(version string) (semver, bool) {
	var sv semver
	rest := version
	if strings.HasPrefix(rest, "v") {
		sv.Prefix, rest = "v", rest[1:]
	}
	if strings.Contains(rest, "+") {
		return semver{}, false
	}
	core, pre, hasPre := strings.Cut(rest, "-")
	if hasPre {
		// The empty prerelease is checked here, beside the Cut, because it is
		// the one malformed prerelease that can change what gets published:
		// read as a release, "1.0.0-" would derive 1.0, 1 and latest. Every
		// other shape isPrereleaseIdentifiers rejects publishes one tag
		// whether it is rejected or accepted, so this is the check that has
		// to sit next to the field it protects.
		if pre == "" || !isPrereleaseIdentifiers(pre) {
			return semver{}, false
		}
		sv.Prerelease = pre
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	for _, part := range parts {
		if !isNumericIdentifier(part) {
			return semver{}, false
		}
	}
	sv.Major, sv.Minor, sv.Patch = parts[0], parts[1], parts[2]
	return sv, true
}

// isNumericIdentifier reports whether s is a SemVer numeric identifier:
// digits, and no leading zero unless the identifier is "0" itself.
func isNumericIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) == 1 || s[0] != '0'
}

// isPrereleaseIdentifiers reports whether s is a SemVer prerelease: one or
// more dot-separated identifiers, each non-empty, each alphanumeric or
// hyphen, and each numeric one without a leading zero.
//
// **Nothing it rejects changes what gets published today**, and that is worth
// stating plainly rather than leaving for someone to work out. A version
// reaching here carries a "-", so it publishes one tag and moves nothing
// whether this says "malformed prerelease, not SemVer" or "prerelease, so no
// moving tags". The one case that did decide something — the empty
// prerelease — is checked in parseSemver instead, next to the Cut that
// produces it.
//
// So this is here as the definition rather than as a guard: a rule that ever
// starts reading the prerelease — ordering two of them, deciding that
// `-rc.1` and `-rc.2` share a moving tag — needs to know it is a prerelease
// in the SemVer sense and not merely a string with a hyphen in it. Everything
// it can see is already inside the OCI tag charset, because validateVersion
// ran first.
func isPrereleaseIdentifiers(s string) bool {
	if s == "" {
		return false
	}
	for _, ident := range strings.Split(s, ".") {
		if ident == "" {
			return false
		}
		numeric := true
		for i := 0; i < len(ident); i++ {
			c := ident[i]
			switch {
			case c >= '0' && c <= '9':
			case isTagAlnum(c) || c == '-':
				numeric = false
			default:
				return false
			}
		}
		if numeric && !isNumericIdentifier(ident) {
			return false
		}
	}
	return true
}
