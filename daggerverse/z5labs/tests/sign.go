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
// It runs in the supplied-key mode, which is a division of labour rather
// than a limitation: the certificate, the identity and the transparency log
// entry are AppKeylessSignatureVerifiesAgainstALocalSigstore's subject, and
// the recursion is this one's. What each covers is the half the other cannot
// see cheaply — signImage walks the same digests whichever signer it holds,
// so the recursive property is mode-independent and is asserted once, over
// the mode that needs no CA standing behind it.
//
// What it covers is the payload, the signature over it, the annotations, the
// manifest shape and the tag cosign computes to find them, all of it read by
// cosign rather than asserted against this module's own idea of the layout.
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
		if err := verifier.mustVerify(ctx, reference); err != nil {
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
	code, stderr, err := verifier.verify(ctx, reference)
	if err != nil {
		return err
	}
	if code == 0 {
		return fmt.Errorf("cosign verified %s against a key that did not sign it", reference)
	}
	// The message, not merely the exit code. Cosign exits non-zero for a
	// pull failure, an unknown flag and an unreachable registry too, and
	// every one of those would also break the positive test — but silently,
	// leaving this test green over a command that verified nothing. The
	// signature-mismatch wording is the only outcome that says the
	// verification actually ran and actually rejected.
	const want = "no matching signatures"
	if !strings.Contains(stderr, want) {
		return fmt.Errorf("cosign rejected %s but not for a signature mismatch: wanted %q in its output, got exit %d and: %s",
			reference, want, code, stderr)
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

	ctr, err := cosignContainer(ctx, svc, password)
	if err != nil {
		return nil, err
	}
	return &cosign{ctr: ctr.WithNewFile("/keys/cosign.pub", string(publicPEM))}, nil
}

// cosignKeylessVerifier builds a cosign container that trusts this session's
// sigstore: its root, and the intermediate the leaves are issued from.
//
// The two files are the whole difference from cosignVerifier. There is no
// key, because the point of the keyless mode is that there is no key for a
// consumer to have been given — what a verifier is told is which authority
// to trust, and it recovers the identity from the certificate.
func cosignKeylessVerifier(ctx context.Context, svc *dagger.Service, password string, sig *sigstoreHarness) (*cosign, error) {
	ctr, err := cosignContainer(ctx, svc, password)
	if err != nil {
		return nil, err
	}
	return &cosign{ctr: ctr.
		WithNewFile("/keys/root.pem", string(sig.RootPEM)).
		WithNewFile("/keys/intermediate.pem", string(sig.IntermediatePEM))}, nil
}

// cosignContainer is the cosign CLI with a registry credential, which is
// everything both verifiers share.
func cosignContainer(ctx context.Context, svc *dagger.Service, password string) (*dagger.Container, error) {
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

	return dag.Container().From(cosignImage).
		WithServiceBinding(registryAlias, svc).
		// 0444 rather than the 0400 default: the cosign image runs as a
		// non-root user, and a secret readable only by root is one cosign
		// reports as a missing credential.
		WithMountedSecret("/docker/config.json",
			dag.SetSecret("z5labs-cosign-docker-config-"+nameHex[:16], string(config)),
			dagger.ContainerWithMountedSecretOpts{Mode: 0o444}).
		WithEnvVariable("DOCKER_CONFIG", "/docker").
		WithEnvVariable("HOME", "/tmp"), nil
}

// verify runs the command a consumer of a *supplied-key* publish is told to
// run in WithSigningKey's doc comment.
//
// --insecure-ignore-tlog is what that mode genuinely requires: nothing
// certified the key, so there is nothing to have logged. verifyKeyless is
// the other mode's command, and the two are separate methods precisely so
// that a flag one of them needs cannot arrive in the other by sharing.
//
// The exec is allowed to fail rather than raising, because a caller
// asserting that a verification *fails* has to be able to say why it
// failed. Treating any error as the expected one would let an image that
// could not be pulled, or a flag this cosign does not know, stand in for a
// signature that did not match — and then the negative test passes while
// proving nothing about the positive one.
func (c *cosign) verify(ctx context.Context, reference string) (int, string, error) {
	return c.run(ctx, reference,
		"--key", "/keys/cosign.pub",
		"--insecure-ignore-tlog=true",
	)
}

// verifyKeyless runs the command Publish's doc comment gives a consumer of a
// keyless publish: an identity and an issuer, and no key anywhere.
//
// Three flags beyond those exist only because the sigstore is this session's,
// and AppKeylessSignatureVerifiesAgainstALocalSigstore's doc comment says
// what each of them costs. They are passed here rather than folded into the
// container so that a reader of a failing run sees the whole command.
func (c *cosign) verifyKeyless(ctx context.Context, reference, identityPattern, issuer string) (int, string, error) {
	return c.run(ctx, reference,
		"--certificate-identity-regexp", identityPattern,
		"--certificate-oidc-issuer", issuer,
		"--ca-roots", "/keys/root.pem",
		"--ca-intermediates", "/keys/intermediate.pem",
		"--insecure-ignore-sct=true",
		"--insecure-ignore-tlog=true",
	)
}

// run executes `cosign verify` with mode-specific flags and returns cosign's
// own exit code and stderr.
//
// --allow-http-registry and --allow-insecure-registry are added here because
// every mode needs them and for the same reason: the registry under test
// speaks plain HTTP. Nothing else is added, so a mode cannot acquire a
// weakening flag by being routed through a shared helper.
func (c *cosign) run(ctx context.Context, reference string, flags ...string) (int, string, error) {
	args := append([]string{"verify"}, flags...)
	args = append(args, "--allow-http-registry", "--allow-insecure-registry", reference)
	ctr := c.ctr.WithExec(args, dagger.ContainerWithExecOpts{
		UseEntrypoint: true,
		Expect:        dagger.ReturnTypeAny,
	})
	code, err := ctr.ExitCode(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("run cosign verify %s: %v", reference, err)
	}
	stderr, err := ctr.Stderr(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("read cosign stderr for %s: %v", reference, err)
	}
	return code, stderr, nil
}

// mustVerify fails unless cosign reports the reference verified.
func (c *cosign) mustVerify(ctx context.Context, reference string) error {
	code, stderr, err := c.verify(ctx, reference)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("cosign verify %s exited %d: %s", reference, code, stderr)
	}
	return nil
}

// mustVerifyKeyless fails unless cosign reports the reference verified by
// identity.
func (c *cosign) mustVerifyKeyless(ctx context.Context, reference, identityPattern, issuer string) error {
	code, stderr, err := c.verifyKeyless(ctx, reference, identityPattern, issuer)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("cosign verify %s by identity %s from %s exited %d: %s",
			reference, identityPattern, issuer, code, stderr)
	}
	return nil
}
