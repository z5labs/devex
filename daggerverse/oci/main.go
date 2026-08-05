// Package main implements the oci Dagger module: a registry client that
// knows how to talk to an OCI registry and nothing about why.
//
// It does not choose tags, decide when to publish, or know what the bytes it
// uploads mean. Callers that need a registry — z5labs' GoApp publish path,
// ssdd's baselines — get one here instead of each growing their own.
//
// The module is pure Go. Container.Publish cannot see session service
// bindings, which is why callers used to shell out to a container that
// could; a Go client running in the module's own runtime reaches a Dagger
// service directly, so this wraps go-containerregistry and oras-go rather
// than pinning tool images.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"dagger/oci/internal/dagger"

	orasauth "oras.land/oras-go/v2/registry/remote/auth"
	orasretry "oras.land/oras-go/v2/registry/remote/retry"
)

// Oci is the module's entrypoint. It holds no state; every operation is
// reached through Registry.
type Oci struct{}

// Registry binds one registry host and its credentials. service, when
// non-nil, is a Dagger-hosted registry reached by hostname rather than over
// the public network — its endpoint replaces host as the address dialled,
// because a session service's hostname is assigned by the engine and cannot
// be predicted by the caller.
//
// insecure is explicit and defaults to off: it means plain HTTP and no TLS
// verification. It is deliberately not inferred from service being set —
// that inference is a test affordance leaking into production behaviour. It
// is spelled insecure rather than tlsVerify because a bool defaulting to
// true is unsettable from the CLI.
//
// +cache="never"
func (m *Oci) Registry(
	// Registry host, as it appears in an image reference: "ghcr.io",
	// "registry.example.com:5000". Ignored when service is set.
	host string,
	// Username for basic authentication. Omit for an anonymous client.
	//
	// +optional
	username string,
	// Password or token for basic authentication.
	//
	// +optional
	password *dagger.Secret,
	// A Dagger-hosted registry to reach over the session network instead of
	// over the public network.
	//
	// +optional
	service *dagger.Service,
	// Talk plain HTTP and skip TLS verification. Off by default.
	//
	// +optional
	insecure bool,
) *Registry {
	return &Registry{
		Host:     host,
		Username: username,
		Password: password,
		Service:  service,
		Insecure: insecure,
	}
}

// Registry is an authenticated handle on one registry host.
//
// Every method carries a never-cache directive on its own doc-comment line:
// registry state is mutable and pushes are side-effecting, so the directive
// repeats on each chained method rather than living only on the factory.
type Registry struct {
	// Host is the registry host this handle was built for.
	Host string
	// Username is the basic-auth user, empty for an anonymous client.
	Username string
	// Insecure reports whether this handle talks plain HTTP.
	Insecure bool

	// Password is the basic-auth secret.
	//
	// +private
	Password *dagger.Secret
	// Service is the Dagger-hosted registry, when there is one.
	//
	// +private
	Service *dagger.Service
}

// conn is a resolved connection: the address actually dialled plus the
// plaintext credentials. Built once per method call, because resolving a
// service endpoint and a secret both need a context.
type conn struct {
	addr     string
	username string
	password string
	insecure bool
}

// connect resolves the registry address and credentials.
//
// A Dagger service is started here rather than in Registry() because
// Registry() takes no context: it is a pure constructor, and starting a
// service is not.
func (reg *Registry) connect(ctx context.Context) (*conn, error) {
	c := &conn{username: reg.Username, insecure: reg.Insecure}

	switch {
	case reg.Service != nil:
		svc, err := reg.Service.Start(ctx)
		if err != nil {
			return nil, fmt.Errorf("start registry service: %v", err)
		}
		ep, err := svc.Endpoint(ctx)
		if err != nil {
			return nil, fmt.Errorf("resolve registry service endpoint: %v", err)
		}
		c.addr = ep
	case reg.Host != "":
		c.addr = reg.Host
	default:
		return nil, errors.New("registry: host is required when no service is given")
	}

	if reg.Password != nil {
		pwd, err := reg.Password.Plaintext(ctx)
		if err != nil {
			return nil, fmt.Errorf("read registry password: %v", err)
		}
		c.password = pwd
	}
	return c, nil
}

// ref renders repository[:tag] against the resolved address.
func (c *conn) ref(repository, reference string) string {
	if reference == "" {
		return c.addr + "/" + repository
	}
	sep := ":"
	if strings.Contains(reference, ":") {
		// A digest, not a tag.
		sep = "@"
	}
	return c.addr + "/" + repository + sep + reference
}

// scrub removes the password from an error's text before it crosses the
// module boundary. Registries carry credentials in an Authorization header,
// so a leak is not expected — but a 401 body, a redirect URL, or a client
// library that echoes its request would each be a way for one to happen, and
// the cost of being wrong is a password in a CI log forever.
func (c *conn) scrub(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if c.password == "" || !strings.Contains(msg, c.password) {
		return err
	}
	return errors.New(strings.ReplaceAll(msg, c.password, "***"))
}

// httpClient builds the HTTP client used for every oras request on this
// connection: retrying, credential-carrying, and TLS-lenient when insecure.
func (c *conn) httpClient() *orasauth.Client {
	var base orasauth.Client
	base.Client = orasretry.DefaultClient
	if c.insecure {
		base.Client = &http.Client{
			Transport: orasretry.NewTransport(&http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // opt-in via insecure
			}),
		}
	}
	base.Cache = orasauth.NewCache()
	base.SetUserAgent("dagger-oci-module")
	if c.username != "" || c.password != "" {
		base.Credential = orasauth.StaticCredential(c.addr, orasauth.Credential{
			Username: c.username,
			Password: c.password,
		})
	}
	return &base
}

// validateRepository rejects an empty repository up front. Everything else
// about the name is the registry's business and the client libraries parse
// it — but an empty component silently produces a reference that means
// something else, which is the failure worth naming here.
func validateRepository(repository string) error {
	if strings.TrimSpace(repository) == "" {
		return errors.New("repository is required")
	}
	return nil
}

// validateTag rejects an empty tag for the same reason.
func validateTag(tag string) error {
	if strings.TrimSpace(tag) == "" {
		return errors.New("tag is required")
	}
	return nil
}
