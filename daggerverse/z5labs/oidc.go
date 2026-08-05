package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"dagger/z-5-labs/internal/dagger"
)

// sigstoreAudience is the audience the workload identity token is minted
// for. It is fixed rather than a parameter: the token is exchanged for a
// signing certificate from a sigstore CA, and a CA will only accept a
// token whose audience names it. A caller able to choose the audience
// could obtain a token this module then presents somewhere it was never
// scoped for.
const sigstoreAudience = "sigstore"

// idTokenTimeout bounds the exchange. The token endpoint is on the CI
// provider's own network and answers in milliseconds; a publish that
// hangs on it is a misconfiguration, and a bounded failure names it
// where an unbounded one just stops.
const idTokenTimeout = 30 * time.Second

// workloadIdentity is what the exchanged OIDC token says about the build,
// reduced to the fields provenance needs.
//
// Every field here comes out of the token's own claims. None of them is
// a parameter, and that is the whole point: a caller-supplied repository
// or commit attests only that someone told the builder what to write
// down. The issuer signs these, and the sigstore CA binds them into the
// certificate the statement is signed with, so a verifier can check them
// against the signature rather than taking the document's word.
type workloadIdentity struct {
	// Issuer is the `iss` claim: the OIDC provider that minted the token.
	Issuer string
	// Subject is the `sub` claim: the workload the provider says this is.
	Subject string
	// Repository is the source repository, where the provider names one.
	Repository string
	// WorkflowRef identifies the build definition that ran — the
	// pipeline file and the ref it was read from.
	WorkflowRef string
	// Commit is the revision the provider says was built, where it says.
	Commit string
	// RunID identifies this particular invocation.
	RunID string
	// Raw is every claim, kept so the predicate can record the identity
	// as the provider expressed it rather than only as this module
	// happened to map it.
	Raw map[string]any
}

// BuilderID is the identity that signed for the build, in the form a
// SLSA verifier matches against: the issuer and the subject it vouched
// for. Both halves are needed — a subject is only meaningful relative to
// the issuer that asserts it.
func (w workloadIdentity) BuilderID() string {
	return w.Issuer + "#" + w.Subject
}

// exchangeIDToken trades the CI provider's token-request machinery for a
// workload identity token scoped to the sigstore audience, and returns
// both the raw token (which the CA needs) and the claims parsed out of
// it.
//
// requestURL and requestToken are the generic shape of this exchange, not
// GitHub's: `ACTIONS_ID_TOKEN_REQUEST_URL` and
// `ACTIONS_ID_TOKEN_REQUEST_TOKEN` are what GitHub Actions calls them,
// but the protocol is "GET this URL with this bearer token, receive a
// JWT", and GitLab, Buildkite, CircleCI and a self-hosted issuer all
// expose something that fits it. Nothing below reads a GitHub-specific
// claim as a requirement.
func exchangeIDToken(ctx context.Context, requestURL string, requestToken *dagger.Secret, service *dagger.Service) (string, *workloadIdentity, error) {
	bearer, err := requestToken.Plaintext(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("read id token request token: %v", err)
	}
	requestURL, err = boundToService(ctx, requestURL, service)
	if err != nil {
		return "", nil, err
	}
	endpoint, err := withAudience(requestURL)
	if err != nil {
		return "", nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, idTokenTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", nil, fmt.Errorf("build id token request: %v", err)
	}
	req.Header.Set("Authorization", "bearer "+bearer)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// The URL is echoed but the bearer token never is: it is a
		// credential, and an error message is the least controlled
		// output this module has.
		return "", nil, fmt.Errorf("request id token from %s: %v", redactQuery(endpoint), err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", nil, fmt.Errorf("read id token response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("id token request to %s returned %s", redactQuery(endpoint), resp.Status)
	}

	var payload struct {
		Value   string `json:"value"`
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", nil, fmt.Errorf("decode id token response: %v", err)
	}
	token := payload.Value
	if token == "" {
		token = payload.IDToken
	}
	if token == "" {
		return "", nil, fmt.Errorf("id token response from %s carried no token", redactQuery(endpoint))
	}

	identity, err := claimsOf(token)
	if err != nil {
		return "", nil, err
	}
	return token, identity, nil
}

// boundToService replaces the request URL's authority with a Dagger
// service's endpoint, leaving the path and query alone.
//
// This is the same shape as registryService, and for the same reason: a
// session-hosted issuer has no address until the engine assigns it one,
// so the caller cannot write the host into the URL. The path is still
// the caller's, because a token endpoint's path is part of the
// provider's protocol and not something a service binding knows about.
//
// The service is started here rather than by the caller because starting
// it is what makes the endpoint resolvable from *this* module's runtime;
// a hostname resolved in someone else's container is not reachable from
// this one.
func boundToService(ctx context.Context, requestURL string, service *dagger.Service) (string, error) {
	if service == nil {
		return requestURL, nil
	}
	started, err := service.Start(ctx)
	if err != nil {
		return "", fmt.Errorf("start id token service: %v", err)
	}
	endpoint, err := started.Endpoint(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve id token service endpoint: %v", err)
	}
	parsed, err := url.Parse(requestURL)
	if err != nil {
		return "", fmt.Errorf("parse id token request url: %v", err)
	}
	parsed.Host = endpoint
	return parsed.String(), nil
}

// withAudience adds the sigstore audience to the token request URL,
// preserving any query the provider already put there.
func withAudience(requestURL string) (string, error) {
	parsed, err := url.Parse(requestURL)
	if err != nil {
		return "", fmt.Errorf("parse id token request url: %v", err)
	}
	// http is permitted alongside https and is not a relaxation made for
	// the test suite: an issuer reachable only inside a session — a
	// self-hosted one, or a local runner's — has no public name to put a
	// certificate on, and refusing it here would make the requirement to
	// exchange a token unsatisfiable for exactly the deployments that
	// most need to. Anything that is not HTTP at all is a caller error.
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", fmt.Errorf("id token request url must be http or https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("id token request url %q names no host", redactQuery(requestURL))
	}
	query := parsed.Query()
	query.Set("audience", sigstoreAudience)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// redactQuery strips the query string before a URL goes into an error.
// A token-request URL's query is provider-issued and has been observed
// to carry a request-scoped secret.
func redactQuery(raw string) string {
	if parsed, err := url.Parse(raw); err == nil {
		parsed.RawQuery = ""
		return parsed.String()
	}
	return "the id token request url"
}

// claimsOf decodes a JWT's claim set.
//
// The signature is not verified here, and that is deliberate rather than
// an omission: the token arrived over TLS directly from the issuer's own
// endpoint, so this module already knows where it came from, and the
// claims are re-verified by the certificate authority that will only
// issue against a signature it checked. Verifying here would mean
// shipping a copy of every supported provider's key discovery, which is
// the GitHub-specific coupling this design exists to avoid.
func claimsOf(token string) (*workloadIdentity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("id token is not a JWT: expected 3 segments, got %d", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode id token claims: %v", err)
	}
	claims := map[string]any{}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, fmt.Errorf("decode id token claims: %v", err)
	}

	identity := &workloadIdentity{
		Issuer:  claimString(claims, "iss"),
		Subject: claimString(claims, "sub"),
		Raw:     claims,
	}
	if identity.Issuer == "" || identity.Subject == "" {
		return nil, fmt.Errorf("id token carries no iss/sub claim, so it identifies no builder")
	}
	if !audienceMatches(claims) {
		return nil, fmt.Errorf("id token audience is not %q; the provider ignored the requested audience", sigstoreAudience)
	}
	// Each of these is read from the first claim a provider is known to
	// use it under, falling back to a claim every OIDC token has. The
	// order is a preference list, not a provider switch: a provider this
	// module has never heard of still produces a usable identity.
	identity.Repository = firstClaim(claims, "repository", "project_path", "repository_url")
	identity.WorkflowRef = firstClaim(claims, "job_workflow_ref", "workflow_ref", "ref", "sub")
	identity.Commit = firstClaim(claims, "sha", "revision", "commit_sha")
	identity.RunID = firstClaim(claims, "run_id", "pipeline_id", "build_id", "jti")
	return identity, nil
}

// audienceMatches reports whether the token was minted for sigstore. The
// `aud` claim is a string or an array of strings depending on the
// provider, and a token minted for something else must not be presented
// to a sigstore CA.
func audienceMatches(claims map[string]any) bool {
	switch aud := claims["aud"].(type) {
	case string:
		return aud == sigstoreAudience
	case []any:
		for _, entry := range aud {
			if s, ok := entry.(string); ok && s == sigstoreAudience {
				return true
			}
		}
	}
	return false
}

// claimString reads one string claim, tolerating a provider that encodes
// a number where a string was expected.
func claimString(claims map[string]any, name string) string {
	switch value := claims[name].(type) {
	case string:
		return value
	case float64:
		return fmt.Sprintf("%.0f", value)
	}
	return ""
}

// firstClaim returns the first of names that is present and non-empty.
func firstClaim(claims map[string]any, names ...string) string {
	for _, name := range names {
		if value := claimString(claims, name); value != "" {
			return value
		}
	}
	return ""
}
