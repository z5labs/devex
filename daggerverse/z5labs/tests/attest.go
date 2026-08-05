package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"dagger/tests/internal/dagger"
)

// The artifact types GoApp attaches under. Duplicated here on purpose:
// a test that imported the module's own constants would pass whatever
// the module renamed them to, and these strings are a contract with
// every consumer that lists referrers.
const (
	spdxArtifactType       = "application/spdx+json"
	cycloneDxArtifactType  = "application/vnd.cyclonedx+json"
	provenanceArtifactType = "application/vnd.in-toto+json"
)

// testRegistry builds an oci handle onto the local registry service, for
// reading back what GoApp published.
func testRegistry(svc *dagger.Service, secret *dagger.Secret) *dagger.OciRegistry {
	return dag.Oci().Registry(registryAlias+":5000", dagger.OciRegistryOpts{
		Username: "ci",
		Password: secret,
		Service:  svc,
		Insecure: true,
	})
}

// descriptor is the part of an OCI descriptor these tests read back.
type descriptor struct {
	MediaType    string            `json:"mediaType"`
	ArtifactType string            `json:"artifactType"`
	Digest       string            `json:"digest"`
	Annotations  map[string]string `json:"annotations"`
}

// manifest is the part of an image manifest or index these tests read.
type manifest struct {
	Annotations map[string]string `json:"annotations"`
	Layers      []descriptor      `json:"layers"`
	Manifests   []struct {
		Digest   string `json:"digest"`
		Platform struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
		} `json:"platform"`
		Annotations map[string]string `json:"annotations"`
	} `json:"manifests"`
}

// GoAppCiAnnotatesEveryPlatformVariant asserts that a published image
// carries the standard OCI source annotations, on every platform variant
// and not merely on the index.
//
// Per variant rather than per index because that is what survives a
// consumer: pulling linux/arm64 resolves the variant's own manifest, and
// an annotation that lived only on the manifest list would be invisible
// to everything downstream of that resolution. The values are checked
// against the git state the fixture was built with, so a plausible-but-
// wrong annotation fails.
func (t *Tests) GoAppCiAnnotatesEveryPlatformVariant(ctx context.Context) error {
	const tag = "v3.1.4"
	src, err := gitFixture(ctx, helloDir(), "main", []string{tag})
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	svc, _, secret, err := localRegistry(ctx)
	if err != nil {
		return err
	}
	prov, err := newProvenanceHarness(ctx, "")
	if err != nil {
		return err
	}
	platforms := []string{"linux/amd64", "linux/arm64"}
	digest, err := dag.Z5Labs().GoApp(src, prov.opts(dagger.Z5LabsGoAppOpts{
		PublishOn:       "^refs/tags/v.+",
		Registry:        registryAlias + ":5000",
		AuthUsername:    "ci",
		Auth:            secret,
		RegistryService: svc,
		Insecure:        true,
		Platforms:       platforms,
	})).Ci(ctx)
	if err != nil {
		return fmt.Errorf("Ci: %v", err)
	}

	headSha, err := headFullSha(ctx, src)
	if err != nil {
		return err
	}
	registry := testRegistry(svc, secret)

	index, err := fetchManifest(ctx, registry, "hello", digest)
	if err != nil {
		return err
	}
	if len(index.Manifests) != len(platforms) {
		return fmt.Errorf("expected %d platform variants under %s, got %d", len(platforms), digest, len(index.Manifests))
	}
	for _, entry := range index.Manifests {
		variant, err := fetchManifest(ctx, registry, "hello", entry.Digest)
		if err != nil {
			return err
		}
		platform := entry.Platform.OS + "/" + entry.Platform.Architecture
		want := map[string]string{
			"org.opencontainers.image.revision": headSha,
			"org.opencontainers.image.source":   fixtureOriginURL,
			"org.opencontainers.image.version":  tag,
		}
		for key, value := range want {
			if got := variant.Annotations[key]; got != value {
				return fmt.Errorf("%s variant %s: expected %q, got %q", platform, key, value, got)
			}
		}
		created := variant.Annotations["org.opencontainers.image.created"]
		if created == "" {
			return fmt.Errorf("%s variant carries no org.opencontainers.image.created", platform)
		}
		// The commit time, not the build time: a wall-clock value here
		// would make every rebuild of one commit a different manifest.
		commitTime, err := headCommitTime(ctx, src)
		if err != nil {
			return err
		}
		if created != commitTime {
			return fmt.Errorf("%s variant created=%q, expected the commit time %q", platform, created, commitTime)
		}
	}
	return nil
}

// GoAppCiRedactsCredentialsFromTheSourceAnnotation asserts a
// credential-bearing origin remote does not travel in the published
// image's source annotation.
//
// This is not hypothetical: `actions/checkout` leaves the origin URL as
// `https://x-access-token:<token>@github.com/org/repo`, and an
// annotation is readable by anyone who can pull the image. So the host
// and path have to survive — the annotation is useless without them —
// while the userinfo must not.
func (t *Tests) GoAppCiRedactsCredentialsFromTheSourceAnnotation(ctx context.Context) error {
	const tag = "v7.0.0"
	token, err := dag.Random().Sha256(ctx)
	if err != nil {
		return fmt.Errorf("random sha256 (fake checkout token): %v", err)
	}
	src, err := gitFixture(ctx, helloDir(), "main", []string{tag})
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	// Overwrite the fixture's origin with the shape a CI checkout leaves.
	credentialed := "https://x-access-token:" + token + "@example.com/z5labs/fixture.git"
	ctr := dag.Go().Container(src).
		WithEnvVariable("NONCE", token).
		WithExec([]string{"git", "remote", "set-url", "origin", credentialed})
	if _, err := ctr.Sync(ctx); err != nil {
		return fmt.Errorf("set credentialed origin: %v", err)
	}
	src = ctr.Directory("/src")

	svc, _, secret, err := localRegistry(ctx)
	if err != nil {
		return err
	}
	prov, err := newProvenanceHarness(ctx, "")
	if err != nil {
		return err
	}
	digest, err := dag.Z5Labs().GoApp(src, prov.opts(dagger.Z5LabsGoAppOpts{
		PublishOn:       "^refs/tags/v.+",
		Registry:        registryAlias + ":5000",
		AuthUsername:    "ci",
		Auth:            secret,
		RegistryService: svc,
		Insecure:        true,
		Platforms:       []string{hostPlatform()},
	})).Ci(ctx)
	if err != nil {
		return fmt.Errorf("Ci: %v", err)
	}

	image, err := fetchManifest(ctx, testRegistry(svc, secret), "hello", digest)
	if err != nil {
		return err
	}
	source := image.Annotations["org.opencontainers.image.source"]
	if strings.Contains(source, token) {
		return fmt.Errorf("the source annotation carries the checkout token")
	}
	if source != "https://example.com/z5labs/fixture.git" {
		return fmt.Errorf("expected the redacted origin, got %q", source)
	}
	return nil
}

// GoAppCiAttachesSbomsAndProvenance asserts that a publish leaves an
// SPDX document, a CycloneDX document and a signed provenance statement
// attached to the digest it returned, each retrievable from the registry
// and told apart by its artifact type.
//
// The provenance is not merely counted: the DSSE envelope is verified
// against the key the publish signed with, the statement's subject is
// checked to be the published digest, and the build identity is checked
// to be the one the token endpoint minted. An attestation nobody
// verifies is a file.
func (t *Tests) GoAppCiAttachesSbomsAndProvenance(ctx context.Context) error {
	const tag = "v5.0.0"
	src, err := gitFixture(ctx, helloDir(), "main", []string{tag})
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	headSha, err := headFullSha(ctx, src)
	if err != nil {
		return err
	}
	svc, _, secret, err := localRegistry(ctx)
	if err != nil {
		return err
	}
	prov, err := newProvenanceHarness(ctx, headSha)
	if err != nil {
		return err
	}
	digest, err := dag.Z5Labs().GoApp(src, prov.opts(dagger.Z5LabsGoAppOpts{
		PublishOn:       "^refs/tags/v.+",
		Registry:        registryAlias + ":5000",
		AuthUsername:    "ci",
		Auth:            secret,
		RegistryService: svc,
		Insecure:        true,
		Platforms:       []string{hostPlatform()},
	})).Ci(ctx)
	if err != nil {
		return fmt.Errorf("Ci: %v", err)
	}

	registry := testRegistry(svc, secret)
	for _, artifactType := range []string{spdxArtifactType, cycloneDxArtifactType, provenanceArtifactType} {
		found, err := referrersOf(ctx, registry, "hello", digest, artifactType)
		if err != nil {
			return err
		}
		if len(found) != 1 {
			return fmt.Errorf("expected exactly 1 referrer of type %s on %s, got %d", artifactType, digest, len(found))
		}
	}

	envelope, err := attachedDocument(ctx, registry, "hello", digest, provenanceArtifactType)
	if err != nil {
		return err
	}
	statement, err := verifyEnvelope(envelope, prov.Public)
	if err != nil {
		return err
	}
	return checkStatement(statement, digest, prov.Claims)
}

// GoAppCiAttestsTwoSegmentBinaryNames asserts a publish whose binary name
// carries a "/" still lands all three attestations.
//
// A two-segment binary name is not an edge case, it is the only working
// GHCR configuration: GHCR has no single-segment repositories, so
// `ghcr.io/<name>` cannot exist and the owner has to be folded into the
// binary name — folding it into the registry instead breaks the attach,
// because the oci module keys its credential on the registry address.
//
// It is checked end to end because the break was not in the push: the
// provenance envelope is written to the module's own filesystem before it
// is attached, and the helper that wrote it treated the name as a single
// path element. That failed only after the image and both SBOMs had
// already been pushed, leaving a published image with no provenance and a
// red build (devex#363). The SBOMs go through Dagger's Directory.withFile
// and were never affected, so they are asserted here too — the point is
// that the whole publish survives the name, not one document of it.
func (t *Tests) GoAppCiAttestsTwoSegmentBinaryNames(ctx context.Context) error {
	const (
		tag        = "v8.0.0"
		repository = "z5labs/hello"
	)
	src, err := gitFixture(ctx, helloDir(), "main", []string{tag})
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	headSha, err := headFullSha(ctx, src)
	if err != nil {
		return err
	}
	svc, _, secret, err := localRegistry(ctx)
	if err != nil {
		return err
	}
	prov, err := newProvenanceHarness(ctx, headSha)
	if err != nil {
		return err
	}
	digest, err := dag.Z5Labs().GoApp(src, prov.opts(dagger.Z5LabsGoAppOpts{
		BinaryName:      repository,
		PublishOn:       "^refs/tags/v.+",
		Registry:        registryAlias + ":5000",
		AuthUsername:    "ci",
		Auth:            secret,
		RegistryService: svc,
		Insecure:        true,
		Platforms:       []string{hostPlatform()},
	})).Ci(ctx)
	if err != nil {
		return fmt.Errorf("Ci: %v", err)
	}

	registry := testRegistry(svc, secret)
	for _, artifactType := range []string{spdxArtifactType, cycloneDxArtifactType, provenanceArtifactType} {
		found, err := referrersOf(ctx, registry, repository, digest, artifactType)
		if err != nil {
			return err
		}
		if len(found) != 1 {
			return fmt.Errorf("expected exactly 1 referrer of type %s on %s/%s, got %d", artifactType, repository, digest, len(found))
		}
	}

	// The envelope is verified rather than counted: the bug replaced the
	// bytes with an error, so a referrer that is present but unreadable
	// would be the same failure wearing a different shape.
	envelope, err := attachedDocument(ctx, registry, repository, digest, provenanceArtifactType)
	if err != nil {
		return err
	}
	statement, err := verifyEnvelope(envelope, prov.Public)
	if err != nil {
		return err
	}
	return checkStatement(statement, digest, prov.Claims)
}

// GoAppCiRefusesToPublishWithoutProvenanceMachinery asserts a publish
// that cannot produce provenance fails, and fails before pushing.
//
// Skipping provenance would be worse than failing: an image published
// without an attestation is indistinguishable from one published with
// until somebody goes looking, so the pipeline that quietly drops it is
// the one nobody notices. The registry is checked afterwards to confirm
// the refusal happened before the push and not after it.
func (t *Tests) GoAppCiRefusesToPublishWithoutProvenanceMachinery(ctx context.Context) error {
	const tag = "v6.0.0"
	src, err := gitFixture(ctx, helloDir(), "main", []string{tag})
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	svc, pwdHex, secret, err := localRegistry(ctx)
	if err != nil {
		return err
	}
	_, err = dag.Z5Labs().GoApp(src, dagger.Z5LabsGoAppOpts{
		PublishOn:       "^refs/tags/v.+",
		Registry:        registryAlias + ":5000",
		AuthUsername:    "ci",
		Auth:            secret,
		RegistryService: svc,
		Insecure:        true,
	}).Ci(ctx)
	if err == nil {
		return fmt.Errorf("expected Ci to refuse to publish without the id token machinery, got nil")
	}
	for _, want := range []string{"idTokenRequestUrl", "idTokenRequestToken", "provenance"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the refusal to name %q, got: %s", want, err.Error())
		}
	}
	code, err := curlProbeManifest(ctx, svc, registryAlias, "ci", pwdHex, "hello", tag)
	if err != nil {
		return fmt.Errorf("curl probe: %v", err)
	}
	if code == 200 {
		return fmt.Errorf("Ci refused the publish but manifest %s is present in the registry", tag)
	}
	return nil
}

// fetchManifest reads a manifest back out of the registry and decodes
// the parts these tests care about.
func fetchManifest(ctx context.Context, registry *dagger.OciRegistry, repository, reference string) (*manifest, error) {
	raw, err := registry.Manifest(ctx, repository, reference)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %v", reference, err)
	}
	out := &manifest{}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return nil, fmt.Errorf("decode manifest %s: %v", reference, err)
	}
	return out, nil
}

// referrersOf lists the artifacts attached to subject under one type.
func referrersOf(ctx context.Context, registry *dagger.OciRegistry, repository, subject, artifactType string) ([]descriptor, error) {
	raw, err := registry.Referrers(ctx, repository, subject, dagger.OciRegistryReferrersOpts{
		ArtifactType: artifactType,
	})
	if err != nil {
		return nil, fmt.Errorf("list %s referrers of %s: %v", artifactType, subject, err)
	}
	var found []descriptor
	if err := json.Unmarshal([]byte(raw), &found); err != nil {
		return nil, fmt.Errorf("decode referrers of %s: %v", subject, err)
	}
	return found, nil
}

// attachedDocument fetches the single attached document of one type: the
// referrer manifest, then its one layer's bytes.
func attachedDocument(ctx context.Context, registry *dagger.OciRegistry, repository, subject, artifactType string) ([]byte, error) {
	found, err := referrersOf(ctx, registry, repository, subject, artifactType)
	if err != nil {
		return nil, err
	}
	if len(found) != 1 {
		return nil, fmt.Errorf("expected 1 referrer of type %s, got %d", artifactType, len(found))
	}
	referrer, err := fetchManifest(ctx, registry, repository, found[0].Digest)
	if err != nil {
		return nil, err
	}
	if len(referrer.Layers) != 1 {
		return nil, fmt.Errorf("expected the %s referrer to hold 1 layer, got %d", artifactType, len(referrer.Layers))
	}
	contents, err := registry.Fetch(repository, referrer.Layers[0].Digest).Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch %s payload: %v", artifactType, err)
	}
	return []byte(contents), nil
}

// verifyEnvelope checks the DSSE signature over the statement and
// returns the statement itself.
//
// The pre-authentication encoding is rebuilt here rather than taken from
// the module: verifying against the module's own idea of what it signed
// would pass for any PAE the module happened to invent, and the whole
// point of DSSE is that the encoding is the one every verifier agrees
// on.
func verifyEnvelope(raw []byte, public *ecdsa.PublicKey) (map[string]any, error) {
	var envelope struct {
		PayloadType string `json:"payloadType"`
		Payload     string `json:"payload"`
		Signatures  []struct {
			Sig string `json:"sig"`
		} `json:"signatures"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("decode attestation envelope: %v", err)
	}
	if envelope.PayloadType != provenanceArtifactType {
		return nil, fmt.Errorf("expected payloadType %q, got %q", provenanceArtifactType, envelope.PayloadType)
	}
	if len(envelope.Signatures) != 1 {
		return nil, fmt.Errorf("expected 1 signature, got %d", len(envelope.Signatures))
	}
	payload, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		return nil, fmt.Errorf("decode attestation payload: %v", err)
	}
	sig, err := base64.StdEncoding.DecodeString(envelope.Signatures[0].Sig)
	if err != nil {
		return nil, fmt.Errorf("decode attestation signature: %v", err)
	}
	pae := fmt.Sprintf("DSSEv1 %d %s %d ", len(envelope.PayloadType), envelope.PayloadType, len(payload))
	digest := sha256.Sum256(append([]byte(pae), payload...))
	if !ecdsa.VerifyASN1(public, digest[:], sig) {
		return nil, fmt.Errorf("attestation signature does not verify against the publish's signing key")
	}
	statement := map[string]any{}
	if err := json.Unmarshal(payload, &statement); err != nil {
		return nil, fmt.Errorf("decode attestation statement: %v", err)
	}
	return statement, nil
}

// checkStatement asserts the statement is about the published digest and
// reports the identity the token endpoint minted — not values a caller
// could have supplied, which is the property that makes it provenance.
func checkStatement(statement map[string]any, digest string, claims map[string]any) error {
	if got := statement["predicateType"]; got != "https://slsa.dev/provenance/v1" {
		return fmt.Errorf("expected a SLSA v1 predicate, got %v", got)
	}
	subjects, ok := statement["subject"].([]any)
	if !ok || len(subjects) != 1 {
		return fmt.Errorf("expected exactly 1 subject, got %v", statement["subject"])
	}
	subject, err := object(subjects[0], "subject[0]")
	if err != nil {
		return err
	}
	digests, err := object(subject["digest"], "subject[0].digest")
	if err != nil {
		return err
	}
	_, encoded, _ := strings.Cut(digest, ":")
	if got := digests["sha256"]; got != encoded {
		return fmt.Errorf("expected the statement's subject to be %s, got %v", digest, got)
	}

	predicate, err := object(statement["predicate"], "predicate")
	if err != nil {
		return err
	}
	runDetails, err := object(predicate["runDetails"], "predicate.runDetails")
	if err != nil {
		return err
	}
	builder, err := object(runDetails["builder"], "predicate.runDetails.builder")
	if err != nil {
		return err
	}
	wantBuilder := fmt.Sprintf("%v#%v", claims["iss"], claims["sub"])
	if got := builder["id"]; got != wantBuilder {
		return fmt.Errorf("expected builder id %q, got %v", wantBuilder, got)
	}
	metadata, err := object(runDetails["metadata"], "predicate.runDetails.metadata")
	if err != nil {
		return err
	}
	if got := metadata["invocationId"]; got != claims["run_id"] {
		return fmt.Errorf("expected invocationId %v, got %v", claims["run_id"], got)
	}

	definition, err := object(predicate["buildDefinition"], "predicate.buildDefinition")
	if err != nil {
		return err
	}
	external, err := object(definition["externalParameters"], "predicate.buildDefinition.externalParameters")
	if err != nil {
		return err
	}
	workflow, err := object(external["workflow"], "predicate.buildDefinition.externalParameters.workflow")
	if err != nil {
		return err
	}
	if got := workflow["repository"]; got != claims["repository"] {
		return fmt.Errorf("expected repository %v, got %v", claims["repository"], got)
	}
	if got := workflow["ref"]; got != claims["job_workflow_ref"] {
		return fmt.Errorf("expected workflow ref %v, got %v", claims["job_workflow_ref"], got)
	}
	return nil
}

// object narrows one decoded JSON value to an object, naming the path it
// was reached by.
//
// Walking a statement with `value, _ := x.(map[string]any)` does not
// panic — Go reads a nil map happily — but it does turn a document with
// the wrong shape into a complaint about a field being `<nil>`, several
// levels below where the shape actually went wrong. Naming the path is
// the difference between "expected builder id X, got <nil>" and
// "predicate.runDetails is not an object".
func object(value any, path string) (map[string]any, error) {
	out, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is not an object, got %T (%v)", path, value, value)
	}
	return out, nil
}

// headFullSha returns the unabbreviated HEAD SHA of a git-backed source.
func headFullSha(ctx context.Context, src *dagger.Directory) (string, error) {
	out, err := dag.Go().Container(src).
		WithExec([]string{"git", "rev-parse", "HEAD"}).
		Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(out), nil
}

// headCommitTime returns HEAD's committer time in RFC 3339, which is
// what the created annotation must carry.
func headCommitTime(ctx context.Context, src *dagger.Directory) (string, error) {
	out, err := dag.Go().Container(src).
		WithExec([]string{"git", "show", "-s", "--format=%cI", "HEAD"}).
		Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("git show commit time: %v", err)
	}
	return strings.TrimSpace(out), nil
}
