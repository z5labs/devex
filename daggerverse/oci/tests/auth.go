package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"dagger/tests/internal/dagger"
)

// AuthenticatesFromDockerConfig asserts a caller can hand over a
// ~/.docker/config.json and have this module find the host's credentials in
// it — no username, no password, just the file a `docker login` already
// wrote.
//
// Both client libraries are exercised: PushImage goes through
// go-containerregistry and Resolve through oras, and the credential
// resolution feeding them is shared. A test using only one would leave the
// other silently anonymous.
func (t *Tests) AuthenticatesFromDockerConfig(ctx context.Context) error {
	reg, err := newRegistry(ctx)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "docker-config")
	if err != nil {
		return err
	}
	endpoint, err := reg.endpoint(ctx)
	if err != nil {
		return err
	}
	config, err := dockerConfigSecret(ctx, dockerConfig{
		auths: map[string]dockerAuth{endpoint: {username: registryUser, password: reg.Password}},
	})
	if err != nil {
		return err
	}

	client := dag.Oci().Registry("test-registry.invalid", dagger.OciRegistryOpts{
		DockerConfig: config,
		Service:      reg.Service,
		Insecure:     true,
	})

	pushed, err := client.PushImage(ctx, repo, "v1", []*dagger.Container{baseImage("linux/amd64")})
	if err != nil {
		return fmt.Errorf("PushImage authenticated from a docker config: %v", err)
	}
	got, err := client.Resolve(ctx, repo, "v1")
	if err != nil {
		return fmt.Errorf("Resolve authenticated from a docker config: %v", err)
	}
	if got != pushed {
		return fmt.Errorf("Resolve returned %s, want the pushed digest %s", got, pushed)
	}
	return nil
}

// DockerConfigCredentialsDoNotLeak asserts that a docker config carrying the
// wrong password fails with a 401 whose text holds neither that password nor
// the base64 blob it was packed into.
//
// The blob matters as much as the password. It is base64, not encryption, so
// a `auth` value in a CI log is a password in a CI log — and it is the form
// the credential actually travels in, which makes it the one a client library
// echoing its own request would print.
func (t *Tests) DockerConfigCredentialsDoNotLeak(ctx context.Context) error {
	reg, err := newRegistry(ctx)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "docker-config-unauthorized")
	if err != nil {
		return err
	}
	wrong, err := uniqueName(ctx, "wrong")
	if err != nil {
		return err
	}
	endpoint, err := reg.endpoint(ctx)
	if err != nil {
		return err
	}

	auth := dockerAuth{username: registryUser, password: wrong}
	config, err := dockerConfigSecret(ctx, dockerConfig{
		auths: map[string]dockerAuth{endpoint: auth},
	})
	if err != nil {
		return err
	}

	client := dag.Oci().Registry("test-registry.invalid", dagger.OciRegistryOpts{
		DockerConfig: config,
		Service:      reg.Service,
		Insecure:     true,
	})

	_, err = client.PushImage(ctx, repo, "v1", []*dagger.Container{baseImage("linux/amd64")})
	if err == nil {
		return errors.New("PushImage with a docker config holding the wrong password succeeded")
	}
	text := err.Error()
	if strings.Contains(text, wrong) {
		return fmt.Errorf("the error text leaks the password from the docker config: %s", text)
	}
	if strings.Contains(text, auth.blob()) {
		return fmt.Errorf("the error text leaks the docker config's base64 auth value: %s", text)
	}
	if strings.Contains(text, reg.Password) {
		return fmt.Errorf("the error text leaks the registry's real password: %s", text)
	}
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "401") && !strings.Contains(lower, "unauthorized") {
		return fmt.Errorf("the error does not look like a 401: %s", text)
	}
	return nil
}

// DockerConfigCredentialHelperIsNotSupported asserts that a config resolving
// this host through a credential helper fails, and that the failure names the
// helper binary it asked for.
//
// Helpers are not honoured and cannot be: one is an external
// docker-credential-* binary the Docker CLI executes, and the module runtime
// holds no gcloud, no ecr-login and no keychain. The alternative to failing
// is falling through to anonymous, which turns "your credential lives
// somewhere I cannot reach" into an unrelated 401 from the registry — a
// failure whose cause is invisible in the message.
func (t *Tests) DockerConfigCredentialHelperIsNotSupported(ctx context.Context) error {
	reg, err := newRegistry(ctx)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "cred-helper")
	if err != nil {
		return err
	}
	endpoint, err := reg.endpoint(ctx)
	if err != nil {
		return err
	}
	config, err := dockerConfigSecret(ctx, dockerConfig{
		credHelpers: map[string]string{endpoint: "gcloud"},
	})
	if err != nil {
		return err
	}

	client := dag.Oci().Registry("test-registry.invalid", dagger.OciRegistryOpts{
		DockerConfig: config,
		Service:      reg.Service,
		Insecure:     true,
	})

	_, err = client.PushImage(ctx, repo, "v1", []*dagger.Container{baseImage("linux/amd64")})
	if err == nil {
		return errors.New("PushImage with a credential-helper-only docker config succeeded")
	}
	if !strings.Contains(err.Error(), "docker-credential-gcloud") {
		return fmt.Errorf("the error does not name the credential helper it could not run: %v", err)
	}
	return nil
}

// AuthenticatesWithBearerToken asserts a token supplied on its own reaches
// the registry as an Authorization: Bearer header, and that a wrong one is
// refused without either token appearing in the error.
//
// The registry behind the gate is anonymous, so nothing here can pass by
// accident on some other credential: the only thing separating the two halves
// of this test is the token.
func (t *Tests) AuthenticatesWithBearerToken(ctx context.Context) error {
	upstream, err := newAnonymousRegistry(ctx)
	if err != nil {
		return err
	}
	proxy, err := newBearerProxy(ctx, upstream)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "bearer")
	if err != nil {
		return err
	}

	client := proxy.client(nil)
	pushed, err := client.PushImage(ctx, repo, "v1", []*dagger.Container{baseImage("linux/amd64")})
	if err != nil {
		return fmt.Errorf("PushImage through a bearer-gated proxy: %v", err)
	}
	got, err := client.Resolve(ctx, repo, "v1")
	if err != nil {
		return fmt.Errorf("Resolve through a bearer-gated proxy: %v", err)
	}
	if got != pushed {
		return fmt.Errorf("Resolve returned %s, want the pushed digest %s", got, pushed)
	}

	wrong, err := uniqueName(ctx, "wrong-token")
	if err != nil {
		return err
	}
	wrongName, err := uniqueName(ctx, "oci-bad-bearer")
	if err != nil {
		return err
	}
	refused := proxy.client(dag.SetSecret(wrongName, wrong))
	if _, err := refused.Resolve(ctx, repo, "v1"); err == nil {
		return errors.New("Resolve with the wrong bearer token succeeded")
	} else if strings.Contains(err.Error(), wrong) {
		return fmt.Errorf("the error text leaks the bearer token that was used: %v", err)
	} else if strings.Contains(err.Error(), proxy.Token) {
		return fmt.Errorf("the error text leaks the proxy's real bearer token: %v", err)
	}
	return nil
}

// PasswordBeatsTokenAndDockerConfig pins the top of the precedence order: a
// username and password win over a bearer token and over a docker config,
// both of which are wrong here and would fail the push if either were used.
//
// Precedence has to be tested from the winning side. A test that supplied
// only correct credentials would pass whichever source the module happened to
// read, and the bug this guards against — a later source quietly overwriting
// an earlier one — would be invisible.
func (t *Tests) PasswordBeatsTokenAndDockerConfig(ctx context.Context) error {
	reg, err := newRegistry(ctx)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "password-wins")
	if err != nil {
		return err
	}
	endpoint, err := reg.endpoint(ctx)
	if err != nil {
		return err
	}
	decoys, err := decoyCredentials(ctx, endpoint)
	if err != nil {
		return err
	}

	client := dag.Oci().Registry("test-registry.invalid", dagger.OciRegistryOpts{
		Username:     registryUser,
		Password:     reg.Secret,
		BearerToken:  decoys.token,
		DockerConfig: decoys.config,
		Service:      reg.Service,
		Insecure:     true,
	})

	pushed, err := client.PushImage(ctx, repo, "v1", []*dagger.Container{baseImage("linux/amd64")})
	if err != nil {
		return fmt.Errorf("PushImage with a correct password beside a wrong token and config: %v", err)
	}
	got, err := client.Resolve(ctx, repo, "v1")
	if err != nil {
		return fmt.Errorf("Resolve with a correct password beside a wrong token and config: %v", err)
	}
	if got != pushed {
		return fmt.Errorf("Resolve returned %s, want the pushed digest %s", got, pushed)
	}
	return nil
}

// TokenBeatsDockerConfig pins the middle rung: with no username or password,
// a bearer token is used and the docker config beside it is not.
//
// The config names the same host and holds credentials the gate would refuse,
// so a module that preferred the file — or that fell back to it after the
// token — would fail here rather than pass quietly.
func (t *Tests) TokenBeatsDockerConfig(ctx context.Context) error {
	upstream, err := newAnonymousRegistry(ctx)
	if err != nil {
		return err
	}
	proxy, err := newBearerProxy(ctx, upstream)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "token-wins")
	if err != nil {
		return err
	}
	endpoint, err := endpointOf(ctx, proxy.Service)
	if err != nil {
		return err
	}
	decoys, err := decoyCredentials(ctx, endpoint)
	if err != nil {
		return err
	}

	client := dag.Oci().Registry("bearer-proxy.invalid", dagger.OciRegistryOpts{
		BearerToken:  proxy.Secret,
		DockerConfig: decoys.config,
		Service:      proxy.Service,
		Insecure:     true,
	})

	pushed, err := client.PushImage(ctx, repo, "v1", []*dagger.Container{baseImage("linux/amd64")})
	if err != nil {
		return fmt.Errorf("PushImage with a correct token beside a wrong docker config: %v", err)
	}
	got, err := client.Resolve(ctx, repo, "v1")
	if err != nil {
		return fmt.Errorf("Resolve with a correct token beside a wrong docker config: %v", err)
	}
	if got != pushed {
		return fmt.Errorf("Resolve returned %s, want the pushed digest %s", got, pushed)
	}
	return nil
}

// AnonymousAccessNeedsNoCredentials asserts the bottom of the order still
// works, in the two shapes it comes in: no credentials at all, and a docker
// config that simply says nothing about this host.
//
// The second shape is the one that breaks by accident. A developer's config
// carries a credsStore for Docker Desktop and entries for two or three
// registries; reading it as "this file governs every host" would make every
// public pull fail with an unrunnable-helper error. It has to mean "nothing
// here is about that registry", and fall through to anonymous.
func (t *Tests) AnonymousAccessNeedsNoCredentials(ctx context.Context) error {
	upstream, err := newAnonymousRegistry(ctx)
	if err != nil {
		return err
	}
	bare, err := uniqueName(ctx, "anonymous")
	if err != nil {
		return err
	}

	client := dag.Oci().Registry("anonymous-registry.invalid", dagger.OciRegistryOpts{
		Service:  upstream,
		Insecure: true,
	})
	pushed, err := client.PushImage(ctx, bare, "v1", []*dagger.Container{baseImage("linux/amd64")})
	if err != nil {
		return fmt.Errorf("PushImage with no credentials at all: %v", err)
	}
	got, err := client.Resolve(ctx, bare, "v1")
	if err != nil {
		return fmt.Errorf("Resolve with no credentials at all: %v", err)
	}
	if got != pushed {
		return fmt.Errorf("Resolve returned %s, want the pushed digest %s", got, pushed)
	}

	// A config about other registries entirely, including the credsStore a
	// Docker Desktop install writes.
	config, err := dockerConfigSecret(ctx, dockerConfig{
		credsStore: "desktop",
		auths: map[string]dockerAuth{
			"ghcr.io": {username: "someone", password: "not-for-this-host"},
		},
	})
	if err != nil {
		return err
	}
	elsewhere, err := uniqueName(ctx, "anonymous-with-config")
	if err != nil {
		return err
	}

	withConfig := dag.Oci().Registry("anonymous-registry.invalid", dagger.OciRegistryOpts{
		DockerConfig: config,
		Service:      upstream,
		Insecure:     true,
	})
	pushed, err = withConfig.PushImage(ctx, elsewhere, "v1", []*dagger.Container{baseImage("linux/amd64")})
	if err != nil {
		return fmt.Errorf("PushImage with a docker config naming other registries: %v", err)
	}
	got, err = withConfig.Resolve(ctx, elsewhere, "v1")
	if err != nil {
		return fmt.Errorf("Resolve with a docker config naming other registries: %v", err)
	}
	if got != pushed {
		return fmt.Errorf("Resolve returned %s, want the pushed digest %s", got, pushed)
	}
	return nil
}

// dockerAuth is one entry of a rendered docker config.
type dockerAuth struct {
	username string
	password string
}

// blob is the base64 "auth" value docker login would have written.
func (auth dockerAuth) blob() string {
	return base64.StdEncoding.EncodeToString([]byte(auth.username + ":" + auth.password))
}

// dockerConfig is the config file a test hands to the module.
type dockerConfig struct {
	auths       map[string]dockerAuth
	credsStore  string
	credHelpers map[string]string
}

// dockerConfigSecret renders a ~/.docker/config.json and wraps it in a Dagger
// secret.
//
// It is rendered through encoding/json rather than a format string: the
// values in it are random per run, and a hand-built JSON document is one
// unescaped character away from a parse failure that would look like a module
// bug.
func dockerConfigSecret(ctx context.Context, config dockerConfig) (*dagger.Secret, error) {
	file := map[string]any{}
	if len(config.auths) > 0 {
		auths := map[string]any{}
		for host, auth := range config.auths {
			auths[host] = map[string]any{"auth": auth.blob()}
		}
		file["auths"] = auths
	}
	if config.credsStore != "" {
		file["credsStore"] = config.credsStore
	}
	if len(config.credHelpers) > 0 {
		file["credHelpers"] = config.credHelpers
	}

	raw, err := json.Marshal(file)
	if err != nil {
		return nil, fmt.Errorf("render docker config: %v", err)
	}
	// The secret's name is derived from an independent random rather than
	// from anything in the file: names surface in trace UIs.
	nameHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, fmt.Errorf("random sha256 (docker config secret name): %v", err)
	}
	return dag.SetSecret("oci-docker-config-"+nameHex[:16], string(raw)), nil
}

// decoys is a bearer token and a docker config that are both wrong for the
// registry they name.
type decoys struct {
	token  *dagger.Secret
	config *dagger.Secret
}

// decoyCredentials builds credentials that would fail if they were used, so a
// precedence test can tell "the right source won" from "any source worked".
func decoyCredentials(ctx context.Context, host string) (*decoys, error) {
	wrongToken, err := uniqueName(ctx, "decoy-token")
	if err != nil {
		return nil, err
	}
	tokenName, err := uniqueName(ctx, "oci-decoy-token")
	if err != nil {
		return nil, err
	}
	wrongPassword, err := uniqueName(ctx, "decoy-password")
	if err != nil {
		return nil, err
	}
	config, err := dockerConfigSecret(ctx, dockerConfig{
		auths: map[string]dockerAuth{host: {username: "decoy", password: wrongPassword}},
	})
	if err != nil {
		return nil, err
	}
	return &decoys{token: dag.SetSecret(tokenName, wrongToken), config: config}, nil
}
