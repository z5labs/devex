package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// credential is one registry credential, whichever source it came from.
//
// The four fields are the union of what the two client libraries accept:
// go-containerregistry's authn.AuthConfig and oras' auth.Credential both
// model basic auth, a bearer access token and a refresh (identity) token,
// under different names. Holding the union here means precedence is decided
// once, in one place, rather than once per library.
type credential struct {
	username     string
	password     string
	accessToken  string
	refreshToken string
}

func (cred credential) empty() bool {
	return cred.username == "" && cred.password == "" &&
		cred.accessToken == "" && cred.refreshToken == ""
}

// secrets lists the plaintext values this credential carries, so scrub can
// keep every one of them out of an error crossing the module boundary. The
// username is not one: it is not secret, and redacting it would make an
// authentication failure unreadable.
func (cred credential) secrets() []string {
	return []string{cred.password, cred.accessToken, cred.refreshToken}
}

// credential resolves the credential this connection will authenticate with,
// along with every plaintext value read on the way — including values that
// lost, so an error can be scrubbed of them too.
//
// Precedence, highest first:
//
//  1. username / password. The most specific thing a caller can say, and the
//     only source that names a user.
//  2. bearerToken. Explicit, but it says nothing about which registry it is
//     for, so a caller who supplied a pair as well meant the pair.
//  3. dockerConfig. A file describing many registries at once, so it is the
//     least specific and loses to anything aimed at this one.
//  4. anonymous.
//
// A lower source is not consulted once a higher one has been supplied, and in
// particular a 401 does not fall through to the next. Retrying with a second
// credential would authenticate as somebody the caller did not choose, and
// would turn one wrong password into two failed attempts against a registry
// that may be counting them.
//
// addr rather than Host is what the docker config is searched for: it is the
// address actually dialled, so a Dagger-hosted registry is looked up under
// the endpoint it was reached at. For every registry on the public network
// the two are the same string.
func (reg *Registry) credential(ctx context.Context, addr string) (credential, []string, error) {
	switch {
	case reg.Username != "" || reg.Password != nil:
		cred := credential{username: reg.Username}
		if reg.Password != nil {
			pwd, err := reg.Password.Plaintext(ctx)
			if err != nil {
				return credential{}, nil, fmt.Errorf("read registry password: %v", err)
			}
			cred.password = pwd
		}
		return cred, cred.secrets(), nil

	case reg.BearerToken != nil:
		tok, err := reg.BearerToken.Plaintext(ctx)
		if err != nil {
			return credential{}, nil, fmt.Errorf("read registry bearer token: %v", err)
		}
		cred := credential{accessToken: tok}
		return cred, cred.secrets(), nil

	case reg.DockerConfig != nil:
		raw, err := reg.DockerConfig.Plaintext(ctx)
		if err != nil {
			return credential{}, nil, fmt.Errorf("read docker config: %v", err)
		}
		return credentialFromDockerConfig(raw, addr)

	default:
		return credential{}, nil, nil
	}
}

// dockerConfigFile is the slice of ~/.docker/config.json this module reads.
// Everything else in that file — proxies, plugin settings, aliases — belongs
// to the Docker CLI and means nothing to a registry client.
type dockerConfigFile struct {
	Auths       map[string]dockerAuthEntry `json:"auths"`
	CredsStore  string                     `json:"credsStore"`
	CredHelpers map[string]string          `json:"credHelpers"`
}

// dockerAuthEntry is one registry's entry under "auths".
type dockerAuthEntry struct {
	// Auth is base64("username:password"), which is what `docker login`
	// writes when no credential store is configured.
	Auth string `json:"auth"`
	// Username and Password are the unencoded form some tools write instead.
	Username string `json:"username"`
	Password string `json:"password"`
	// IdentityToken is an OAuth2 refresh token; RegistryToken is a bearer
	// token to send as-is.
	IdentityToken string `json:"identitytoken"`
	RegistryToken string `json:"registrytoken"`
}

// credentialFromDockerConfig finds the credential a Docker config holds for
// addr.
//
// A config that says nothing about addr is not an error: it is a config that
// describes other registries, and the caller gets anonymous access — the same
// answer `docker pull` would give.
func credentialFromDockerConfig(raw, addr string) (credential, []string, error) {
	var cfg dockerConfigFile
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		// encoding/json quotes the offending input in its message, and the
		// offending input here is a file full of passwords. The error says
		// what is wrong and nothing about what was in it.
		return credential{}, nil, errors.New("parse docker config: the supplied secret is not valid JSON")
	}

	// Collected before anything can fail, and returned on every path: a
	// credential that lost the lookup is still a credential that must not
	// reach a log.
	redact := dockerConfigSecrets(cfg)
	want := normalizeRegistryHost(addr)

	var (
		entry   dockerAuthEntry
		matched bool
	)
	for key, candidate := range cfg.Auths {
		if normalizeRegistryHost(key) == want {
			entry, matched = candidate, true
			break
		}
	}

	cred, err := entry.credential()
	if err != nil {
		return credential{}, redact, err
	}
	if !cred.empty() {
		return cred, redact, nil
	}

	// Nothing usable in the entry. If a helper owns this host, saying so is
	// far better than silently trying the registry anonymously and reporting
	// whatever 401 comes back.
	for key, helper := range cfg.CredHelpers {
		if normalizeRegistryHost(key) == want {
			return credential{}, redact, unsupportedHelper(want, helper)
		}
	}
	if matched && cfg.CredsStore != "" {
		return credential{}, redact, unsupportedHelper(want, cfg.CredsStore)
	}

	// A credsStore with no entry for this host is the ordinary shape of a
	// developer's config file. Erroring on it would make every config written
	// by Docker Desktop unusable for a public registry, so it falls through
	// to anonymous instead.
	return credential{}, redact, nil
}

// unsupportedHelper is the error for a config that resolves a host through a
// credential helper.
//
// Helpers are not honoured and cannot be: a helper is an external binary the
// Docker CLI executes, and the module runtime is a Go process in a container
// holding neither gcloud, nor ecr-login, nor a macOS keychain. Reaching them
// would mean shelling out to a helper container, which is the one thing this
// module does not do anywhere.
//
// So the failure is explicit, names the binary the config asked for, and says
// what to do instead. The alternative — falling through to anonymous — turns a
// missing credential into a 401 from somewhere else entirely.
func unsupportedHelper(host, helper string) error {
	return fmt.Errorf(
		"docker config resolves %s through the credential helper docker-credential-%s, "+
			"which this module cannot run: no helper binaries exist in the module runtime. "+
			"Resolve the credential in the caller and pass it as username/password or bearerToken",
		host, helper)
}

// credential reads one auths entry.
//
// The explicit username and password fields win over the packed auth blob
// when both are present: a tool that wrote both wrote the unencoded pair
// last, and they are the ones a human editing the file would have changed.
func (entry dockerAuthEntry) credential() (credential, error) {
	cred := credential{
		username:     entry.Username,
		password:     entry.Password,
		accessToken:  entry.RegistryToken,
		refreshToken: entry.IdentityToken,
	}
	if entry.Auth == "" {
		return cred, nil
	}

	decoded, err := decodeDockerAuth(entry.Auth)
	if err != nil {
		return credential{}, err
	}
	user, pass, ok := strings.Cut(decoded, ":")
	if !ok {
		return credential{}, errors.New(`docker config: an "auth" value does not decode to "username:password"`)
	}
	if cred.username == "" {
		cred.username = user
	}
	if cred.password == "" {
		cred.password = pass
	}
	return cred, nil
}

// decodeDockerAuth decodes an "auth" value. Padded standard base64 is what
// `docker login` writes; the unpadded form is accepted because other tools
// write it and rejecting it would fail for a reason the user cannot see.
//
// The error carries no part of the value: a partially decoded credential is
// still a credential.
func decodeDockerAuth(auth string) (string, error) {
	if decoded, err := base64.StdEncoding.DecodeString(auth); err == nil {
		return string(decoded), nil
	}
	decoded, err := base64.RawStdEncoding.DecodeString(auth)
	if err != nil {
		return "", errors.New(`docker config: an "auth" value is not valid base64`)
	}
	return string(decoded), nil
}

// dockerConfigSecrets lists every plaintext credential anywhere in the
// config, not only the one that matched.
//
// The whole file was handed over as one secret, so every value in it is the
// caller's secret. Scrubbing only the entry that won would let a stray error
// mentioning some other host's password through.
func dockerConfigSecrets(cfg dockerConfigFile) []string {
	secrets := make([]string, 0, len(cfg.Auths)*4)
	for _, entry := range cfg.Auths {
		secrets = append(secrets, entry.Password, entry.IdentityToken, entry.RegistryToken)
		if entry.Auth == "" {
			continue
		}
		// The blob itself is a credential in transport encoding.
		secrets = append(secrets, entry.Auth)
		decoded, err := decodeDockerAuth(entry.Auth)
		if err != nil {
			continue
		}
		if _, pass, ok := strings.Cut(decoded, ":"); ok {
			secrets = append(secrets, pass)
		}
	}
	return secrets
}

// normalizeRegistryHost reduces a Docker config key or a registry address to
// the bare host[:port] both sides can be compared on.
//
// Config keys are written every way a decade of Docker CLI versions allowed:
// "ghcr.io", "https://ghcr.io", and for Docker Hub the legacy
// "https://index.docker.io/v1/". Comparing them literally would miss the
// entry a user can plainly see in their file.
func normalizeRegistryHost(host string) string {
	normalized := strings.TrimSpace(host)
	if scheme := strings.Index(normalized, "://"); scheme >= 0 {
		normalized = normalized[scheme+3:]
	}
	normalized, _, _ = strings.Cut(normalized, "/")
	normalized = strings.ToLower(normalized)

	// Docker Hub answers to four names and `docker login` may have written
	// any of them.
	switch normalized {
	case "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		return "docker.io"
	}
	return normalized
}
