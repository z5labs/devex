package main

import (
	"errors"
	"fmt"
	"strings"
)

// credentialSelfCheck exercises docker-config credential resolution in
// process.
//
// It sits here rather than in tests/ because what it covers cannot be reached
// from there at a sensible price. Every case below is a shape of
// ~/.docker/config.json — a key written with a scheme, Docker Hub under its
// legacy name, an unpadded auth blob, a credsStore beside an empty entry —
// and reaching each one through the test module would mean standing up a
// registry per shape to assert on a string comparison that never touches the
// network. The live tests in tests/ prove the resolved credential actually
// authenticates; this proves the right one is resolved.
//
// Every case is run and every failure is reported: a run that stopped at the
// first would hide the rest behind whichever happened to be listed first.
func credentialSelfCheck() error {
	var failures []error
	for _, check := range []func() error{
		checkHostNormalization,
		checkAuthEntryShapes,
		checkHostMatching,
		checkCredentialHelpers,
		checkMalformedConfigsSayNothing,
		checkEveryCredentialIsRedacted,
	} {
		if err := check(); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// checkHostNormalization covers the ways a config key and a dialled address
// can spell the same registry.
func checkHostNormalization() error {
	cases := []struct{ in, want string }{
		{"ghcr.io", "ghcr.io"},
		{"https://ghcr.io", "ghcr.io"},
		{"http://localhost:5000", "localhost:5000"},
		{"  GHCR.IO  ", "ghcr.io"},
		{"registry.example.com:5000/v2/", "registry.example.com:5000"},
		// The four names Docker Hub answers to. `docker login` has written
		// each of them at some point in its history, and a user looking at
		// their own config file cannot tell which they got.
		{"docker.io", "docker.io"},
		{"index.docker.io", "docker.io"},
		{"https://index.docker.io/v1/", "docker.io"},
		{"registry-1.docker.io", "docker.io"},
	}
	var failures []error
	for _, tc := range cases {
		if got := normalizeRegistryHost(tc.in); got != tc.want {
			failures = append(failures, fmt.Errorf("normalizeRegistryHost(%q) = %q, want %q", tc.in, got, tc.want))
		}
	}
	return errors.Join(failures...)
}

// checkAuthEntryShapes covers the forms one auths entry is written in.
func checkAuthEntryShapes() error {
	cases := []struct {
		name string
		json string
		want credential
	}{
		{
			name: "packed auth blob",
			json: `{"auths":{"ghcr.io":{"auth":"dXNlcjpwYXNz"}}}`,
			want: credential{username: "user", password: "pass"},
		},
		{
			// Some tools omit the padding. Rejecting it would fail for a
			// reason invisible to whoever wrote the file: the padded and
			// unpadded forms of the same credential look alike.
			name: "unpadded auth blob",
			json: `{"auths":{"ghcr.io":{"auth":"dXNlcjpwYXM"}}}`,
			want: credential{username: "user", password: "pas"},
		},
		{
			name: "explicit username and password",
			json: `{"auths":{"ghcr.io":{"username":"user","password":"pass"}}}`,
			want: credential{username: "user", password: "pass"},
		},
		{
			// A password containing a colon still round-trips: only the
			// first colon separates the two halves.
			name: "password containing a colon",
			json: `{"auths":{"ghcr.io":{"auth":"dXNlcjpwYTpzcw=="}}}`,
			want: credential{username: "user", password: "pa:ss"},
		},
		{
			// What `docker login` writes for a registry that issued an
			// OAuth2 refresh token: an auth blob with an empty password
			// beside the token.
			name: "identity token",
			json: `{"auths":{"ghcr.io":{"auth":"dXNlcjo=","identitytoken":"refresh-me"}}}`,
			want: credential{username: "user", refreshToken: "refresh-me"},
		},
		{
			name: "registry token",
			json: `{"auths":{"ghcr.io":{"registrytoken":"bearer-me"}}}`,
			want: credential{accessToken: "bearer-me"},
		},
	}

	var failures []error
	for _, tc := range cases {
		got, _, err := credentialFromDockerConfig(tc.json, "ghcr.io")
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: unexpected error: %v", tc.name, err))
			continue
		}
		if got != tc.want {
			failures = append(failures, fmt.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want))
		}
	}
	return errors.Join(failures...)
}

// checkHostMatching covers which entry a lookup lands on, and what happens
// when it lands on none.
func checkHostMatching() error {
	const config = `{
	  "auths": {
	    "https://index.docker.io/v1/": {"auth":"aHViOmh1Yg=="},
	    "https://ghcr.io": {"auth":"Z2g6Z2hw"},
	    "registry.example.com:5000": {"auth":"cmVnOnJlZw=="}
	  }
	}`

	cases := []struct {
		addr string
		want credential
	}{
		{"docker.io", credential{username: "hub", password: "hub"}},
		{"index.docker.io", credential{username: "hub", password: "hub"}},
		{"ghcr.io", credential{username: "gh", password: "ghp"}},
		{"registry.example.com:5000", credential{username: "reg", password: "reg"}},
		// A port is part of the identity: a config for the registry on 5000
		// says nothing about the one on 5001.
		{"registry.example.com:5001", credential{}},
		// A config that names other registries is a request for anonymous
		// access here, not an error.
		{"quay.io", credential{}},
	}

	var failures []error
	for _, tc := range cases {
		got, _, err := credentialFromDockerConfig(config, tc.addr)
		if err != nil {
			failures = append(failures, fmt.Errorf("lookup %s: unexpected error: %v", tc.addr, err))
			continue
		}
		if got != tc.want {
			failures = append(failures, fmt.Errorf("lookup %s: got %+v, want %+v", tc.addr, got, tc.want))
		}
	}
	return errors.Join(failures...)
}

// checkCredentialHelpers covers the boundary between "this host's credential
// is somewhere I cannot reach" and "this file says nothing about this host".
//
// Getting that boundary wrong in the strict direction is the expensive
// mistake: a credsStore is present in essentially every config Docker Desktop
// writes, and treating it as governing every host would make a public,
// anonymous pull fail for anyone who has ever run `docker login`.
func checkCredentialHelpers() error {
	var failures []error

	wantsHelper := []struct {
		name   string
		json   string
		addr   string
		binary string
	}{
		{
			name:   "per-host helper",
			json:   `{"credHelpers":{"1234.dkr.ecr.us-east-1.amazonaws.com":"ecr-login"}}`,
			addr:   "1234.dkr.ecr.us-east-1.amazonaws.com",
			binary: "docker-credential-ecr-login",
		},
		{
			name:   "credsStore with an entry for this host",
			json:   `{"credsStore":"desktop","auths":{"ghcr.io":{}}}`,
			addr:   "ghcr.io",
			binary: "docker-credential-desktop",
		},
	}
	for _, tc := range wantsHelper {
		_, _, err := credentialFromDockerConfig(tc.json, tc.addr)
		if err == nil {
			failures = append(failures, fmt.Errorf("%s: want an error naming %s, got none", tc.name, tc.binary))
			continue
		}
		if !strings.Contains(err.Error(), tc.binary) {
			failures = append(failures, fmt.Errorf("%s: error does not name %s: %v", tc.name, tc.binary, err))
		}
	}

	wantsCredential := []struct {
		name string
		json string
		addr string
		want credential
	}{
		{
			// A credential written in the file wins over the store: it is
			// there, and the store is not reachable.
			name: "credsStore beside a usable entry",
			json: `{"credsStore":"desktop","auths":{"ghcr.io":{"auth":"Z2g6Z2hw"}}}`,
			addr: "ghcr.io",
			want: credential{username: "gh", password: "ghp"},
		},
		{
			name: "per-host helper beside a usable entry",
			json: `{"credHelpers":{"ghcr.io":"gcloud"},"auths":{"ghcr.io":{"auth":"Z2g6Z2hw"}}}`,
			addr: "ghcr.io",
			want: credential{username: "gh", password: "ghp"},
		},
		{
			// The ordinary developer config, seen from a public registry.
			name: "credsStore with no entry for this host",
			json: `{"credsStore":"desktop","auths":{"ghcr.io":{}}}`,
			addr: "quay.io",
			want: credential{},
		},
		{
			name: "helpers for other hosts only",
			json: `{"credHelpers":{"gcr.io":"gcloud"}}`,
			addr: "quay.io",
			want: credential{},
		},
	}
	for _, tc := range wantsCredential {
		got, _, err := credentialFromDockerConfig(tc.json, tc.addr)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: unexpected error: %v", tc.name, err))
			continue
		}
		if got != tc.want {
			failures = append(failures, fmt.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want))
		}
	}
	return errors.Join(failures...)
}

// checkMalformedConfigsSayNothing asserts a config this module cannot read
// fails without quoting itself back.
//
// encoding/json puts the offending input in its error message, and the
// offending input here is a file full of passwords. A parse failure must
// therefore be replaced, not wrapped.
func checkMalformedConfigsSayNothing() error {
	cases := []struct {
		name    string
		json    string
		mustNot string
	}{
		{
			name:    "not json at all",
			json:    `not json, and here is a password: hunter2`,
			mustNot: "hunter2",
		},
		{
			name:    "auth is not base64",
			json:    `{"auths":{"ghcr.io":{"auth":"not base64 hunter2"}}}`,
			mustNot: "hunter2",
		},
		{
			name:    "auth has no separator",
			json:    `{"auths":{"ghcr.io":{"auth":"aHVudGVyMg=="}}}`,
			mustNot: "hunter2",
		},
	}

	var failures []error
	for _, tc := range cases {
		_, _, err := credentialFromDockerConfig(tc.json, "ghcr.io")
		if err == nil {
			failures = append(failures, fmt.Errorf("%s: want an error, got none", tc.name))
			continue
		}
		if strings.Contains(err.Error(), tc.mustNot) {
			failures = append(failures, fmt.Errorf("%s: the error quotes the config back: %v", tc.name, err))
		}
	}
	return errors.Join(failures...)
}

// checkEveryCredentialIsRedacted asserts the redaction list covers the whole
// file, not just the entry that matched.
//
// The file arrived as one secret, so every credential in it is the caller's
// secret — including the ones for hosts this connection never dialled.
func checkEveryCredentialIsRedacted() error {
	const config = `{
	  "auths": {
	    "ghcr.io": {"auth":"Z2g6Z2hw"},
	    "quay.io": {"username":"q","password":"quay-password"},
	    "gcr.io": {"identitytoken":"gcr-refresh","registrytoken":"gcr-bearer"}
	  }
	}`

	_, redact, err := credentialFromDockerConfig(config, "ghcr.io")
	if err != nil {
		return fmt.Errorf("unexpected error: %v", err)
	}
	indexed := make(map[string]bool, len(redact))
	for _, secret := range redact {
		indexed[secret] = true
	}

	var failures []error
	// "ghp" is the matched host's password, "Z2g6Z2hw" the blob it travelled
	// in, and the rest belong to hosts that lost the lookup.
	for _, want := range []string{"ghp", "Z2g6Z2hw", "quay-password", "gcr-refresh", "gcr-bearer"} {
		if !indexed[want] {
			failures = append(failures, fmt.Errorf("%q is not in the redaction list %q", want, redact))
		}
	}

	// And the list has to be applied, not merely collected.
	scrubbed := (&conn{redact: redact}).scrub(errors.New("401 from ghcr.io: tried ghp and quay-password"))
	if strings.Contains(scrubbed.Error(), "ghp") || strings.Contains(scrubbed.Error(), "quay-password") {
		failures = append(failures, fmt.Errorf("scrub left a credential behind: %v", scrubbed))
	}
	return errors.Join(failures...)
}
