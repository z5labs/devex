package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime"

	"dagger/tests/internal/dagger"
)

// zotVersion pins the registry these tests run against. ":latest" is a
// moving target and would make a red suite ambiguous between "this module
// broke" and "the registry changed".
const zotVersion = "v2.1.8"

// registryUser is the htpasswd account every test registry is created with.
// It is not a secret and nothing asserts on it.
const registryUser = "ci"

// registryImage is zot, not the registry:2 the other test modules in this
// repo use and not distribution 3 either.
//
// The referrers tests need a registry that serves the native OCI 1.1
// referrers API, because oras silently falls back to the OCI 1.1 tag schema
// against one that does not — and a suite green over the fallback is
// evidence about the fallback, not about GHCR. registry:2.8 has no such
// endpoint. registry:3.0.0 was measured here too and does not register the
// route either: GET /v2/<name>/referrers/<digest> comes back as a bare
// "404 page not found" from the router, not a registry error. zot serves it.
// requireNativeReferrersAPI keeps that a checked fact rather than a claim in
// this comment.
//
// zot publishes one image per architecture rather than a manifest list, so
// the tag has to name the arch. Test module code runs in a linux container
// on the engine, which makes runtime.GOARCH here the engine's architecture.
func registryImage() string {
	return fmt.Sprintf("ghcr.io/project-zot/zot-linux-%s:%s", runtime.GOARCH, zotVersion)
}

// zotConfig is the registry's configuration.
//
// Authentication is htpasswd with no accessControl block, which in zot means
// any authenticated user may do anything — including DELETE, which
// PushImageIsNotCached needs. Deduplication is off because it rewrites blobs
// as hard links in the background, and a test that deletes a manifest and
// pushes it again should not be racing a background rewriter.
const zotConfig = `{
  "storage": { "rootDirectory": "/var/lib/registry", "dedupe": false },
  "http": {
    "address": "0.0.0.0",
    "port": "5000",
    "auth": { "htpasswd": { "path": "/etc/zot/htpasswd" } }
  },
  "log": { "level": "warn" }
}`

// testRegistry is one running registry plus the credentials for it.
type testRegistry struct {
	Service  *dagger.Service
	Secret   *dagger.Secret
	Password string
}

// newRegistry stands up a zot registry with htpasswd auth, over plain HTTP.
func newRegistry(ctx context.Context) (*testRegistry, error) {
	pwd, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, fmt.Errorf("random sha256 (password): %v", err)
	}
	// Secret names surface in trace UIs and logs, so the name is derived
	// from an independent random rather than from the password.
	nameHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, fmt.Errorf("random sha256 (secret name): %v", err)
	}
	secret := dag.SetSecret("oci-registry-pwd-"+nameHex[:16], pwd)

	// The password reaches htpasswd through a secret env var so it never
	// appears in container args. NONCE is a separate non-secret random:
	// Dagger deliberately excludes secret values from cache keys, so without
	// it this exec would return an earlier session's htpasswd file, built
	// for an earlier session's password.
	htpasswd := dag.Container().From("httpd:2.4-alpine").
		WithEnvVariable("NONCE", nameHex).
		WithSecretVariable("REGISTRY_PASSWORD", secret).
		WithExec([]string{"sh", "-c", `htpasswd -Bbn ` + registryUser + ` "$REGISTRY_PASSWORD" > /tmp/htpasswd`}).
		File("/tmp/htpasswd")

	svc := dag.Container().From(registryImage()).
		WithMountedFile("/etc/zot/htpasswd", htpasswd).
		WithNewFile("/etc/zot/config.json", zotConfig).
		WithExposedPort(5000).
		AsService(dagger.ContainerAsServiceOpts{
			UseEntrypoint: true,
			Args:          []string{"serve", "/etc/zot/config.json"},
		})

	return &testRegistry{Service: svc, Secret: secret, Password: pwd}, nil
}

// client is an authenticated, plain-HTTP handle on the test registry.
//
// host is a placeholder: Registry() ignores it whenever a service is set,
// because a session service's hostname is assigned by the engine. Passing a
// recognisable string keeps that visible in a trace.
func (tr *testRegistry) client() *dagger.OciRegistry {
	return dag.Oci().Registry("test-registry.invalid", dagger.OciRegistryOpts{
		Username: registryUser,
		Password: tr.Secret,
		Service:  tr.Service,
		Insecure: true,
	})
}

// strict is the same registry with TLS verification left on, which is what
// PushFailsAgainstPlaintextRegistryByDefault needs: a plain-HTTP registry
// addressed by a client that has not been told to accept one.
func (tr *testRegistry) strict() *dagger.OciRegistry {
	return dag.Oci().Registry("test-registry.invalid", dagger.OciRegistryOpts{
		Username: registryUser,
		Password: tr.Secret,
		Service:  tr.Service,
	})
}

// endpoint resolves the running registry's host:port, for the few
// assertions that have to talk to it directly rather than through the
// module under test.
func (tr *testRegistry) endpoint(ctx context.Context) (string, error) {
	return endpointOf(ctx, tr.Service)
}

// endpointOf starts a service and returns the host:port it answers on.
//
// The credential tests need this for a second reason beyond talking to the
// registry directly: a docker config is keyed by host, and the host a
// Dagger-hosted registry is reached at is assigned by the engine, so the
// config can only be written once the service has an endpoint.
func endpointOf(ctx context.Context, service *dagger.Service) (string, error) {
	svc, err := service.Start(ctx)
	if err != nil {
		return "", fmt.Errorf("start service: %v", err)
	}
	ep, err := svc.Endpoint(ctx)
	if err != nil {
		return "", fmt.Errorf("service endpoint: %v", err)
	}
	return ep, nil
}

// anonymousZotConfig is the same registry with the auth block removed
// entirely: zot then allows every operation without credentials.
const anonymousZotConfig = `{
  "storage": { "rootDirectory": "/var/lib/registry", "dedupe": false },
  "http": { "address": "0.0.0.0", "port": "5000" },
  "log": { "level": "warn" }
}`

// newAnonymousRegistry stands up a registry that asks for no credentials.
//
// It stands in for a public registry. The alternative — pointing an anonymous
// test at docker.io — would make a green suite depend on an unauthenticated
// pull rate limit, and would say nothing this does not: what is under test is
// that the module sends no Authorization header and does not fail for the
// want of one.
//
// NONCE is why each caller gets its own instance. Dagger content-addresses
// services, so two identical container definitions are one running service
// shared across tests and across sessions; a random makes the definitions
// differ, which keeps one test's pushes out of another's registry.
func newAnonymousRegistry(ctx context.Context) (*dagger.Service, error) {
	nonce, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, fmt.Errorf("random sha256 (registry nonce): %v", err)
	}
	return dag.Container().From(registryImage()).
		WithEnvVariable("NONCE", nonce).
		WithNewFile("/etc/zot/config.json", anonymousZotConfig).
		WithExposedPort(5000).
		AsService(dagger.ContainerAsServiceOpts{
			UseEntrypoint: true,
			Args:          []string{"serve", "/etc/zot/config.json"},
		}), nil
}

// proxyImage is the reverse proxy the bearer tests put in front of a
// registry. Pinned for the same reason zot is.
const proxyImage = "nginx:1.29-alpine"

// bearerProxyTemplate gates every request on one exact bearer token and
// forwards what survives to the registry bound as "registry".
//
// The 401 carries a Bearer challenge because both client libraries decide how
// to authenticate from the WWW-Authenticate header on the first refusal. A
// bare 401 makes them give up, and a Basic challenge makes them send basic
// auth — so a gate without this header would test nothing about bearer
// tokens. Neither library then calls the realm: an access token supplied by
// the caller is sent as-is, which is exactly the flow under test.
//
// ${BEARER_TOKEN} is substituted by the nginx image's own entrypoint at
// container start, from a secret environment variable. The token therefore
// never appears in a container argument or in an image layer.
const bearerProxyTemplate = `server {
    listen 80;
    server_name _;
    client_max_body_size 0;

    location / {
        add_header WWW-Authenticate 'Bearer realm="http://bearer-proxy.invalid/token",service="registry"' always;
        if ($http_authorization != "Bearer ${BEARER_TOKEN}") {
            return 401;
        }
        proxy_pass http://registry:5000;
        proxy_set_header Host $http_host;
        proxy_request_buffering off;
        proxy_read_timeout 300s;
    }
}
`

// bearerProxy is a registry reachable only with one bearer token.
type bearerProxy struct {
	Service *dagger.Service
	Secret  *dagger.Secret
	Token   string
}

// newBearerProxy puts a bearer-token gate in front of upstream.
//
// No registry in this repo's test estate issues bearer tokens — zot
// authenticates with htpasswd and nothing else — so the only way to exercise
// the flow a real token-issuing registry uses is to build the gate. What it
// proves is narrow and exactly the acceptance criterion: a token handed to
// Registry() arrives at the far end as `Authorization: Bearer <token>`.
//
// NGINX_ENVSUBST_FILTER restricts the entrypoint's substitution to
// BEARER_TOKEN. Without it envsubst also replaces nginx's own
// $http_authorization and $http_host with empty strings, and the gate then
// refuses everything.
func newBearerProxy(ctx context.Context, upstream *dagger.Service) (*bearerProxy, error) {
	token, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, fmt.Errorf("random sha256 (bearer token): %v", err)
	}
	nameHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, fmt.Errorf("random sha256 (bearer secret name): %v", err)
	}
	secret := dag.SetSecret("oci-bearer-token-"+nameHex[:16], token)

	svc := dag.Container().From(proxyImage).
		WithServiceBinding("registry", upstream).
		WithEnvVariable("NGINX_ENVSUBST_FILTER", "BEARER_TOKEN").
		WithSecretVariable("BEARER_TOKEN", secret).
		WithNewFile("/etc/nginx/templates/default.conf.template", bearerProxyTemplate).
		WithExposedPort(80).
		AsService(dagger.ContainerAsServiceOpts{
			UseEntrypoint: true,
			Args:          []string{"nginx", "-g", "daemon off;"},
		})

	return &bearerProxy{Service: svc, Secret: secret, Token: token}, nil
}

// client is a handle on the gated registry. A nil token means the proxy's
// own, which is the one that gets through.
func (proxy *bearerProxy) client(token *dagger.Secret) *dagger.OciRegistry {
	if token == nil {
		token = proxy.Secret
	}
	return dag.Oci().Registry("bearer-proxy.invalid", dagger.OciRegistryOpts{
		BearerToken: token,
		Service:     proxy.Service,
		Insecure:    true,
	})
}

// requireNativeReferrersAPI asserts the test registry answers
// GET /v2/<repo>/referrers/<digest> itself.
//
// It is the assertion that makes the referrer tests evidence about GHCR. A
// registry without the endpoint returns 404, oras silently falls back to the
// OCI 1.1 tag schema, and every referrer assertion still passes — against a
// code path GHCR does not use. Checking the status code directly is the only
// way to tell those two green suites apart.
func requireNativeReferrersAPI(ctx context.Context, tr *testRegistry, repo, subject string) error {
	endpoint, err := tr.endpoint(ctx)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("http://%s/v2/%s/referrers/%s", endpoint, repo, subject)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build referrers request: %v", err)
	}
	req.SetBasicAuth(registryUser, tr.Password)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s serves no native referrers API: GET /v2/<repo>/referrers/<digest> returned %d, want 200 (body %s)",
			registryImage(), resp.StatusCode, string(body))
	}
	return nil
}

// deleteManifest removes a manifest from the registry side, so a test can
// watch the next push put it back.
//
// It goes straight at the registry rather than through the module: the module
// deliberately has no delete, because nothing in the publish path this
// replaces ever deleted anything, and a registry client that can delete is a
// different blast radius from one that cannot.
func (tr *testRegistry) deleteManifest(ctx context.Context, repo, digest string) error {
	endpoint, err := tr.endpoint(ctx)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("http://%s/v2/%s/manifests/%s", endpoint, repo, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("build delete request: %v", err)
	}
	req.SetBasicAuth(registryUser, tr.Password)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("DELETE %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	// The dist-spec says 202 Accepted; 200 is tolerated because a registry
	// that deletes synchronously has still done what was asked.
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("DELETE %s returned %d, want 200 or 202 (body %s)", url, resp.StatusCode, string(body))
	}
	return nil
}

// pushDistinctImage pushes an image whose content nothing else shares, and
// returns its digest. Each call produces a different digest, which is what
// lets a test move a tag.
func (tr *testRegistry) pushDistinctImage(ctx context.Context, repo, tag string) (string, error) {
	marker, err := uniqueName(ctx, "marker")
	if err != nil {
		return "", err
	}
	img := baseImage("linux/amd64").WithNewFile("/marker", marker)
	digest, err := tr.client().PushImage(ctx, repo, tag, []*dagger.Container{img})
	if err != nil {
		return "", fmt.Errorf("PushImage %s:%s: %v", repo, tag, err)
	}
	return digest, nil
}

// uniqueName returns a fresh repository or tag component. No test bakes in a
// repository name: two tests sharing one would interfere whenever the suite
// runs in parallel, and a name that survives between runs would let a stale
// manifest satisfy an assertion about a fresh push.
func uniqueName(ctx context.Context, prefix string) (string, error) {
	hex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return "", fmt.Errorf("random sha256 (%s): %v", prefix, err)
	}
	return prefix + "-" + hex[:16], nil
}
