package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"sort"
	"strings"

	"dagger/tests/internal/dagger"
)

// cosignImage is the upstream cosign release, pinned. It is the whole point
// of these tests: what makes a signature worth publishing is that a command
// a consumer already has can check it, so the checking is done by cosign
// itself and never by a verifier written here. A hand-rolled reader would
// pass over a layout cosign rejects, which is exactly the failure being
// guarded against.
const cosignImage = "ghcr.io/sigstore/cosign/cosign:v2.4.1"

// AppSignsEveryPublishedManifest asserts that stock `cosign verify` passes
// against the published tag and against every per-platform manifest
// beneath it.
//
// Both halves matter and the second is the one that is easy to lose. A
// consumer verifies the tag, which resolves to the manifest list; their
// runtime then pulls the per-platform manifest for its architecture, a
// different digest the index signature says nothing about. Signing the
// index alone would leave this test's first assertion passing and its
// second failing — a verification reporting success over bytes it never
// checked, which is worse than no signature at all.
//
// It runs in the supplied-key mode, because a session with no sigstore
// beside it cannot get a Fulcio certificate. What that costs is stated
// rather than hidden: the certificate, the identity and the transparency
// log entry are untested here, exactly as fulcioCertificate and rekorBundle
// say. What it covers is everything else — the payload, the signature over
// it, the annotations, the manifest shape and the tag cosign computes to
// find them, all of it read by cosign rather than asserted against this
// module's own idea of the layout.
func (t *Tests) AppSignsEveryPublishedManifest(ctx context.Context) error {
	const (
		version    = "v9.0.0"
		repository = "z5labs/signed"
	)
	src, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	headSha, err := headFullSha(ctx, src)
	if err != nil {
		return err
	}
	svc, password, secret, err := localRegistry(ctx)
	if err != nil {
		return err
	}
	prov, err := newProvenanceHarness(ctx, headSha)
	if err != nil {
		return err
	}

	// Two platforms, so the publish produces a real manifest list with real
	// children. One platform may be stored as a bare manifest, which would
	// make the recursive half of this test assert nothing.
	app := dag.Z5Labs().Go(src).App(version, dagger.Z5LabsGoChainAppOpts{
		Platforms: []dagger.Platform{"linux/amd64", "linux/arm64"},
	})
	refs, err := publishable(app, svc, secret, prov).Publish(ctx, []string{repository})
	if err != nil {
		return fmt.Errorf("Publish: %v", err)
	}
	digest, err := digestOf(refs[0])
	if err != nil {
		return err
	}

	registry := testRegistry(svc, secret)
	index, err := fetchManifest(ctx, registry, repository, digest)
	if err != nil {
		return err
	}
	if len(index.Manifests) < 2 {
		return fmt.Errorf("published manifest %s lists %d children, want the two platforms that were built",
			digest, len(index.Manifests))
	}

	verifier, err := cosignVerifier(ctx, svc, password, prov.Public)
	if err != nil {
		return err
	}
	// The tag first, which is what a consumer types, then every child, which
	// is what their runtime actually pulls.
	references := []string{fmt.Sprintf("%s:5000/%s:%s", registryAlias, repository, version)}
	for _, child := range index.Manifests {
		references = append(references, fmt.Sprintf("%s:5000/%s@%s", registryAlias, repository, child.Digest))
	}
	for _, reference := range references {
		if err := verifier.verify(ctx, reference); err != nil {
			return err
		}
	}
	return nil
}

// AppSignatureDoesNotVerifyForAnotherKey asserts `cosign verify` fails when
// the key it is given is not the one that signed.
//
// Without it every assertion in AppSignsEveryPublishedManifest could be
// satisfied by a cosign invocation that checks nothing — a wrong flag, a
// missing digest, a verifier that resolves nothing and reports success. A
// green verification is only evidence if the same command goes red when the
// signature does not belong to the key.
func (t *Tests) AppSignatureDoesNotVerifyForAnotherKey(ctx context.Context) error {
	const (
		version    = "v9.1.0"
		repository = "z5labs/signed-elsewhere"
	)
	src, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	headSha, err := headFullSha(ctx, src)
	if err != nil {
		return err
	}
	svc, password, secret, err := localRegistry(ctx)
	if err != nil {
		return err
	}
	prov, err := newProvenanceHarness(ctx, headSha)
	if err != nil {
		return err
	}
	// A second harness only for its key: an independently generated one,
	// never a mutation of the first, so nothing about the two can coincide.
	other, err := newProvenanceHarness(ctx, headSha)
	if err != nil {
		return err
	}

	app := dag.Z5Labs().Go(src).App(version, dagger.Z5LabsGoChainAppOpts{Platforms: []dagger.Platform{hostPlatform()}})
	if _, err := publishable(app, svc, secret, prov).Publish(ctx, []string{repository}); err != nil {
		return fmt.Errorf("Publish: %v", err)
	}

	verifier, err := cosignVerifier(ctx, svc, password, other.Public)
	if err != nil {
		return err
	}
	reference := fmt.Sprintf("%s:5000/%s:%s", registryAlias, repository, version)
	if err := verifier.verify(ctx, reference); err == nil {
		return fmt.Errorf("cosign verified %s against a key that did not sign it", reference)
	}
	return nil
}

// signatureTagsFor is the tags cosign computes to find the signatures of a
// published digest and of every manifest beneath it, sorted.
//
// It is derived from what the registry holds rather than from the module's
// own idea of the layout, so a test comparing against it is asserting that
// a signature exists for each manifest that was really published.
func signatureTagsFor(ctx context.Context, registry *dagger.OciRegistry, repository, digest string) ([]string, error) {
	index, err := fetchManifest(ctx, registry, repository, digest)
	if err != nil {
		return nil, err
	}
	out := []string{strings.ReplaceAll(digest, ":", "-") + signatureTagSuffix}
	for _, child := range index.Manifests {
		out = append(out, strings.ReplaceAll(child.Digest, ":", "-")+signatureTagSuffix)
	}
	sort.Strings(out)
	return out, nil
}

// partitionTags splits a repository's tags into the release tags and the
// signature tags.
//
// Signature tags are told apart by the suffix, which is cosign's rule and
// not this suite's: a consumer's tooling makes the same split, and a test
// that used a different one would pass over a layout cosign could not read.
func partitionTags(tags []string) (release, signatures []string) {
	for _, tag := range tags {
		if strings.HasSuffix(tag, signatureTagSuffix) {
			signatures = append(signatures, tag)
			continue
		}
		release = append(release, tag)
	}
	sort.Strings(release)
	sort.Strings(signatures)
	return release, signatures
}

// signatureTagSuffix is what cosign appends to the digest-derived tag it
// stores a signature under. Spelled out here rather than imported from the
// module, so a rename there is a test failure rather than a silent
// agreement between two halves of one codebase.
const signatureTagSuffix = ".sig"

// cosign is a container holding the cosign CLI, a registry credential and
// the public key to verify against.
type cosign struct {
	ctr *dagger.Container
}

// cosignVerifier builds that container.
//
// The credential goes in as a docker config rather than on the command
// line, because that is the only way cosign takes one: it authenticates
// through go-containerregistry's keychain, which reads $DOCKER_CONFIG. The
// whole config is mounted as a secret, because the file holds
// base64("ci:<password>") — a `withNewFile` would put those bytes in the
// call arguments, which is where a trace reads them from.
func cosignVerifier(ctx context.Context, svc *dagger.Service, password string, key *ecdsa.PublicKey) (*cosign, error) {
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode verification key: %v", err)
	}
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})

	config, err := json.Marshal(map[string]any{
		"auths": map[string]any{
			registryAlias + ":5000": map[string]string{
				"auth": base64.StdEncoding.EncodeToString([]byte("ci:" + password)),
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode docker config: %v", err)
	}
	// The secret's name is an independent random, never derived from what it
	// holds: names show up in traces where values do not.
	nameHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, fmt.Errorf("random sha256 (secret name): %v", err)
	}

	ctr := dag.Container().From(cosignImage).
		WithServiceBinding(registryAlias, svc).
		WithNewFile("/keys/cosign.pub", string(publicPEM)).
		// 0444 rather than the 0400 default: the cosign image runs as a
		// non-root user, and a secret readable only by root is one cosign
		// reports as a missing credential.
		WithMountedSecret("/docker/config.json",
			dag.SetSecret("z5labs-cosign-docker-config-"+nameHex[:16], string(config)),
			dagger.ContainerWithMountedSecretOpts{Mode: 0o444}).
		WithEnvVariable("DOCKER_CONFIG", "/docker").
		WithEnvVariable("HOME", "/tmp")
	return &cosign{ctr: ctr}, nil
}

// verify runs `cosign verify` against one reference.
//
// The flags are the ones a consumer of a supplied-key publish is told to
// run in WithSigningKey's doc comment, plus the two that exist only because
// the registry under test speaks plain HTTP. Nothing here weakens the check
// itself: --insecure-ignore-tlog is what the supplied-key mode genuinely
// requires, and adding it silently to the keyless mode is the failure this
// naming is meant to make obvious.
func (c *cosign) verify(ctx context.Context, reference string) error {
	_, err := c.ctr.
		WithExec([]string{
			"verify",
			"--key", "/keys/cosign.pub",
			"--insecure-ignore-tlog=true",
			"--allow-http-registry",
			"--allow-insecure-registry",
			reference,
		}, dagger.ContainerWithExecOpts{UseEntrypoint: true}).
		Sync(ctx)
	if err != nil {
		return fmt.Errorf("cosign verify %s: %v", reference, err)
	}
	return nil
}
