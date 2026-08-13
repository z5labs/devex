package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// selfTestCredential stands in for the secret half of a remote's userinfo
// everywhere in this file.
//
// It names itself for a reason. The whole point of the rows below is that at
// least one of them describes a value which, before devex#425, was published
// verbatim onto a manifest — so a row that failed would print the thing the
// check exists to keep out of a log. The literal is chosen so that a reader
// who finds it in CI output learns nothing and reaches for nothing, and the
// assertions below still never quote it: naming it is the second line of
// defence, not the first.
const selfTestCredential = "not-a-real-credential"

// SourceRedactionSelfTest checks redactURLCredentials against the rule rather
// than against a publish: no remote reaches
// org.opencontainers.image.source carrying userinfo, and an SSH remote —
// whose username is part of the address — survives untouched.
//
// It is a check of its own because the failure it guards cannot be seen from
// a publish. A remote whose credential leaks produces an image that builds,
// pushes and runs exactly like one whose credential was stripped; the
// difference is a field on a manifest that anyone who can pull the image can
// read, discovered by whoever reads it rather than by this pipeline. And the
// inputs that leak are the ones a working tree cannot easily be made to have:
// driving them through GoChain.gitFacts would mean a container, a repository
// and a remote URL that git itself would have to accept, per row.
//
// It sits on the module rather than in tests/ for the same reason
// VersionTagsSelfTest does — the function is unexported — and because a table
// of a dozen remotes costs one in-process call here.
//
// The rows are the shapes that distinguish the rule, including the accepting
// ones. A redactor that returned the empty string for everything would pass a
// table of leaking inputs alone while deleting the annotation from every
// image this module publishes.
//
// Nothing here quotes an input. url.Parse's own error is the reason the habit
// is worth keeping: it renders as `parse "<the whole URL>": invalid URL
// escape "%zz"`, credential included, so a redactor that reported why it
// could not parse would leak by exactly the path this check closes. The
// assertions below print a row's name and its expectation, and print what
// came back only after establishing that it does not carry the credential.
//
// +check
// +cache="session"
func (m *Z5labs) SourceRedactionSelfTest(ctx context.Context) error {
	// secret is the credential a row carries, spelled three ways. The plain
	// one is a URL that parses; the other two are the shapes that made
	// url.Parse fail and, before this check existed, made the redactor return
	// its input untouched. A percent that is not followed by two hex digits is
	// what someone who did not encode a literal `%` in their password
	// produces, so the correctly encoded credential was redacted and the naive
	// one was published.
	const (
		secret        = selfTestCredential
		secretPercent = selfTestCredential + "%zz"
		secretControl = selfTestCredential + "\x7f"
	)

	cases := []struct {
		// name identifies the row in a failure. It is what a failure prints in
		// place of the input, which is never printed.
		name string
		raw  string
		want string
		why  string
	}{
		{
			name: "the GitHub Actions checkout credential",
			raw:  "https://x-access-token:" + secret + "@github.com/z5labs/devex.git",
			want: "https://github.com/z5labs/devex.git",
			why:  "the case the function was written against: it parses, so the userinfo comes off and the rest is kept",
		},
		{
			name: "a password carrying an unencoded percent",
			raw:  "https://user:" + secretPercent + "@github.com/z5labs/devex.git",
			want: "https://github.com/z5labs/devex.git",
			why:  "url.Parse refuses the escape, and the old fallback published the whole string",
		},
		{
			name: "a password carrying a control character",
			raw:  "https://user:" + secretControl + "@github.com/org/repo.git",
			want: "https://github.com/org/repo.git",
			why:  "the other way userinfo defeats url.Parse, and it published the whole string too",
		},
		{
			name: "an unencoded percent beside a port",
			raw:  "https://user:" + secretPercent + "@github.com:8443/org/repo.git",
			want: "https://github.com:8443/org/repo.git",
			why:  "the port is part of the authority and belongs to the host half of it, so it is kept",
		},
		{
			name: "an unencoded percent with no scheme",
			raw:  "//user:" + secretPercent + "@github.com/org/repo.git",
			want: "//github.com/org/repo.git",
			why:  "a scheme-relative reference has an authority, so it is redacted like any other",
		},
		{
			name: "an at sign inside the password",
			raw:  "https://user:" + secret + "@extra@github.com/org/repo.git",
			want: "https://github.com/org/repo.git",
			why:  "url.Parse escapes the inner at sign and splits at the last one, so this row pins the parsing branch rather than the fallback",
		},
		{
			name: "an at sign inside a password that also defeats the parser",
			raw:  "https://user:" + secretPercent + "@extra@github.com/org/repo.git",
			want: "https://github.com/org/repo.git",
			why:  "the same shape through the fallback, which is where the split at the last at sign is this module's own to get right: splitting at the first would leave the tail of the credential in the value",
		},
		{
			name: "a password carrying an unencoded slash",
			raw:  "https://user:" + secret + "/CD@github.com/org/repo.git",
			want: "",
			why:  "a slash ends the authority, so the at sign falls outside it and the string reads as having no userinfo at all — and a slash is in the base64 alphabet tokens are drawn from, which makes this the likeliest way a pasted remote fails to parse",
		},
		{
			name: "a password carrying an unencoded question mark",
			raw:  "https://user:" + secret + "?CD@github.com/org/repo.git",
			want: "",
			why:  "the query delimiter ends the authority the same way, and the same reading applies",
		},
		{
			name: "a password carrying an unencoded hash",
			raw:  "https://user:" + secret + "#CD@github.com/org/repo.git",
			want: "",
			why:  "the fragment delimiter, the third of them, and the last shape that could put the at sign out of reach",
		},
		{
			name: "userinfo beside an unparseable path",
			raw:  "https://user:" + secret + "@github.com/or%zg/repo.git",
			want: "",
			why:  "the redaction cannot be confirmed by re-parsing, so the annotation is omitted rather than published on trust",
		},
		{
			name: "userinfo behind an empty scheme",
			raw:  "://user:" + secret + "@github.com/org/repo.git",
			want: "",
			why:  "no tool writes this, but it is a credential in an authority, and the fallback runs on strings nothing accepted: the userinfo comes off and the remainder still will not parse, so nothing is published",
		},
		{
			name: "an at sign in the path of a remote that will not parse",
			raw:  "https://github.com/or%zg/re@po.git",
			want: "",
			why:  "the price of the rule above: nothing here is secret, but in a string no parser accepted an at sign after the authority cannot be shown to be part of the path, and an absent annotation beats one that is present and wrong",
		},

		{
			name: "an SSH remote in scp syntax",
			raw:  "git@github.com:z5labs/devex.git",
			want: "git@github.com:z5labs/devex.git",
			why:  "not a URL and not a credential: the username is part of the address, and this is the case the fallback exists for",
		},
		{
			name: "an SSH remote in scp syntax with a doubled slash in its path",
			raw:  "git@github.com:z5labs//devex.git",
			want: "git@github.com:z5labs//devex.git",
			why:  "the doubled slash is in the path, so it must not be read as the start of an authority and turn the address into a redaction",
		},
		{
			name: "an SSH remote spelled as a URL",
			raw:  "ssh://git@github.com/z5labs/devex.git",
			want: "ssh://github.com/z5labs/devex.git",
			why:  "spelled as a URL it has userinfo, and a redactor cannot tell a username that is an address from one that is a login",
		},
		{
			name: "an ordinary public remote",
			raw:  "https://github.com/z5labs/devex.git",
			want: "https://github.com/z5labs/devex.git",
			why:  "nothing to strip, and the annotation is the reason the whole function returns a value at all",
		},
		{
			name: "a public remote whose path will not parse",
			raw:  "https://github.com/or%zg/repo.git",
			want: "https://github.com/or%zg/repo.git",
			why:  "unparseable but with no userinfo anywhere: there is no credential to omit an annotation over",
		},
		{
			name: "no origin at all",
			raw:  "",
			want: "",
			why:  "a tree with no origin, which ociAnnotations turns into an absent key rather than a blank one",
		},
	}

	for _, c := range cases {
		got := redactURLCredentials(c.raw)

		// First, and before anything is quoted: the credential is not in the
		// result. This is the property the whole check exists for, and it is
		// asserted separately from the expectation so that it holds even for a
		// row whose want was written wrongly.
		if strings.Contains(got, selfTestCredential) {
			return fmt.Errorf("%s (%s): the credential survived redaction; neither the remote nor the value is quoted here, because printing it is the failure this check exists to prevent", c.name, c.why)
		}
		if got != c.want {
			return fmt.Errorf("%s (%s): want %q, got %q", c.name, c.why, c.want, got)
		}

		// An invariant that holds whatever the table says, and the one a
		// future refactor is most likely to break quietly: a value that parses
		// carries no userinfo. A row could be added with a want that leaks
		// something other than the sentinel; this catches that, and it catches
		// a redactor that started returning its input for a shape nobody
		// tabulated.
		if parsed, err := url.Parse(got); err == nil && parsed.User != nil {
			return fmt.Errorf("%s (%s): the redacted value still carries userinfo", c.name, c.why)
		}
	}
	return nil
}
