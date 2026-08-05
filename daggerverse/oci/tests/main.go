// Package main implements the test module for daggerverse/oci. Each test is
// exposed as a standalone Dagger function so it can be invoked individually
// during TDD; All wires them up for parallel execution under `dagger check`.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"dagger/tests/internal/dagger"

	par "github.com/dagger/dagger/util/parallel"
)

// baseImageRef is the smallest image with a real layer that these tests can
// push. A scratch container has no layers at all, which would leave the blob
// upload path untested.
const baseImageRef = "alpine:3.20"

// annotationKey is a real OCI annotation rather than a made-up one, so the
// test proves the key a consumer would actually look for survives.
const annotationKey = "org.opencontainers.image.revision"

// titleAnnotation is the annotation the module names each artifact layer
// with.
const titleAnnotation = "org.opencontainers.image.title"

// The two artifact types the referrer tests distinguish between. Filtering
// only means something when more than one type is attached to the same
// subject, so there have to be two.
const (
	sbomArtifactType        = "application/vnd.example.sbom.v1+json"
	attestationArtifactType = "application/vnd.example.attestation.v1+json"
)

// baseImage builds a pushable single-platform container.
func baseImage(platform string) *dagger.Container {
	return dag.Container(dagger.ContainerOpts{Platform: dagger.Platform(platform)}).
		From(baseImageRef)
}

type Tests struct{}

// All runs every oci test. parallel caps concurrency; it defaults to 0
// (unbounded fan-out — GH Actions schedules each `dagger check` job onto its
// own runner, so in-runner parallelism is bounded by the VM).
//
// +check
// +cache="session"
func (t *Tests) All(
	ctx context.Context,
	// +default=0
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}
	jobs = jobs.WithJob("ResolveFailsForMissingTag", t.ResolveFailsForMissingTag)
	jobs = jobs.WithJob("AnnotationsSurvivePush", t.AnnotationsSurvivePush)
	jobs = jobs.WithJob("PushImagePushesAllVariants", t.PushImagePushesAllVariants)
	jobs = jobs.WithJob("CopyPreservesAllManifests", t.CopyPreservesAllManifests)
	jobs = jobs.WithJob("PushArtifactThenFetchRoundTripsContent", t.PushArtifactThenFetchRoundTripsContent)
	jobs = jobs.WithJob("AttachThenFetchRoundTripsContent", t.AttachThenFetchRoundTripsContent)
	jobs = jobs.WithJob("ReferrersListsAttachedArtifacts", t.ReferrersListsAttachedArtifacts)
	jobs = jobs.WithJob("ReferrersFiltersByArtifactType", t.ReferrersFiltersByArtifactType)
	jobs = jobs.WithJob("AttachFailsForUnknownSubject", t.AttachFailsForUnknownSubject)
	jobs = jobs.WithJob("ResolveIsNotCached", t.ResolveIsNotCached)
	jobs = jobs.WithJob("PushImageIsNotCached", t.PushImageIsNotCached)
	jobs = jobs.WithJob("PushFailsAgainstPlaintextRegistryByDefault", t.PushFailsAgainstPlaintextRegistryByDefault)
	jobs = jobs.WithJob("PushSucceedsAgainstPlaintextRegistryWhenInsecure", t.PushSucceedsAgainstPlaintextRegistryWhenInsecure)
	jobs = jobs.WithJob("PushFailsWithBadCredentials", t.PushFailsWithBadCredentials)
	jobs = jobs.WithJob("AuthenticatesFromDockerConfig", t.AuthenticatesFromDockerConfig)
	jobs = jobs.WithJob("DockerConfigCredentialsDoNotLeak", t.DockerConfigCredentialsDoNotLeak)
	jobs = jobs.WithJob("DockerConfigCredentialHelperIsNotSupported", t.DockerConfigCredentialHelperIsNotSupported)
	jobs = jobs.WithJob("AuthenticatesWithBearerToken", t.AuthenticatesWithBearerToken)
	jobs = jobs.WithJob("PasswordBeatsTokenAndDockerConfig", t.PasswordBeatsTokenAndDockerConfig)
	jobs = jobs.WithJob("TokenBeatsDockerConfig", t.TokenBeatsDockerConfig)
	jobs = jobs.WithJob("AnonymousAccessNeedsNoCredentials", t.AnonymousAccessNeedsNoCredentials)

	return jobs.Run(ctx)
}

// ResolveIsNotCached asserts that Resolve reports what the registry holds
// now, not what it held the first time it was asked.
//
// Registry state is mutable and Dagger caches function results for a week by
// default, so without a never-cache directive the second Resolve would replay
// the first one's answer — and every caller reading a moving tag would act on
// a digest that had already been superseded.
func (t *Tests) ResolveIsNotCached(ctx context.Context) error {
	reg, err := newRegistry(ctx)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "moving-tag")
	if err != nil {
		return err
	}

	first, err := reg.pushDistinctImage(ctx, repo, "latest")
	if err != nil {
		return err
	}
	before, err := reg.client().Resolve(ctx, repo, "latest")
	if err != nil {
		return fmt.Errorf("Resolve before the second push: %v", err)
	}
	if before != first {
		return fmt.Errorf("Resolve returned %s, want the pushed digest %s", before, first)
	}

	second, err := reg.pushDistinctImage(ctx, repo, "latest")
	if err != nil {
		return err
	}
	if second == first {
		return fmt.Errorf("the two pushes produced the same digest %s; the fixture is not distinct", first)
	}

	after, err := reg.client().Resolve(ctx, repo, "latest")
	if err != nil {
		return fmt.Errorf("Resolve after the second push: %v", err)
	}
	if after == before {
		return fmt.Errorf("Resolve is cached: it returned %s both before and after the tag moved to %s",
			before, second)
	}
	if after != second {
		return fmt.Errorf("Resolve returned %s, want the second pushed digest %s", after, second)
	}
	return nil
}

// PushImageIsNotCached asserts a second push really uploads.
//
// The two pushes have identical content and therefore identical digests, so
// the returned value cannot tell a real upload from a replayed one. Deleting
// the manifest between them is what makes the difference observable: after
// the delete the tag is gone, and only a push that actually reached the
// registry can bring it back.
func (t *Tests) PushImageIsNotCached(ctx context.Context) error {
	reg, err := newRegistry(ctx)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "republished")
	if err != nil {
		return err
	}
	marker, err := uniqueName(ctx, "marker")
	if err != nil {
		return err
	}
	img := baseImage("linux/amd64").WithNewFile("/marker", marker)

	first, err := reg.client().PushImage(ctx, repo, "v1", []*dagger.Container{img})
	if err != nil {
		return fmt.Errorf("first PushImage: %v", err)
	}
	if err := reg.deleteManifest(ctx, repo, first); err != nil {
		return err
	}
	if _, err := reg.client().Resolve(ctx, repo, "v1"); err == nil {
		return fmt.Errorf("Resolve succeeded after %s was deleted; the delete did not take effect", first)
	}

	second, err := reg.client().PushImage(ctx, repo, "v1", []*dagger.Container{img})
	if err != nil {
		return fmt.Errorf("second PushImage: %v", err)
	}
	if second != first {
		return fmt.Errorf("identical content pushed twice gave %s then %s", first, second)
	}
	got, err := reg.client().Resolve(ctx, repo, "v1")
	if err != nil {
		return fmt.Errorf("PushImage is cached: the second push returned %s without re-uploading, "+
			"and the tag is still absent: %v", second, err)
	}
	if got != first {
		return fmt.Errorf("Resolve after the second push returned %s, want %s", got, first)
	}
	return nil
}

// PushFailsAgainstPlaintextRegistryByDefault asserts a client that was not
// told to accept plain HTTP refuses to push to one.
//
// This is the behaviour the old inline skopeo path did not have: it inferred
// "skip TLS verification" from a test-only registry service being present, so
// a production push over a hijacked plaintext connection would have gone
// through silently.
func (t *Tests) PushFailsAgainstPlaintextRegistryByDefault(ctx context.Context) error {
	reg, err := newRegistry(ctx)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "plaintext-refused")
	if err != nil {
		return err
	}

	variants := []*dagger.Container{baseImage("linux/amd64")}
	if _, err := reg.strict().PushImage(ctx, repo, "v1", variants); err == nil {
		return errors.New("PushImage over plain HTTP succeeded without insecure being set")
	}
	// The push failing is only half of it: nothing may have landed either.
	if _, err := reg.client().Resolve(ctx, repo, "v1"); err == nil {
		return errors.New("the refused push still left a manifest behind")
	}
	return nil
}

// PushSucceedsAgainstPlaintextRegistryWhenInsecure is the other half: the
// same registry, the same image, one explicit opt-in.
func (t *Tests) PushSucceedsAgainstPlaintextRegistryWhenInsecure(ctx context.Context) error {
	reg, err := newRegistry(ctx)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "plaintext-allowed")
	if err != nil {
		return err
	}

	pushed, err := reg.client().PushImage(ctx, repo, "v1", []*dagger.Container{baseImage("linux/amd64")})
	if err != nil {
		return fmt.Errorf("PushImage with insecure set: %v", err)
	}
	got, err := reg.client().Resolve(ctx, repo, "v1")
	if err != nil {
		return fmt.Errorf("Resolve: %v", err)
	}
	if got != pushed {
		return fmt.Errorf("Resolve returned %s, want the pushed digest %s", got, pushed)
	}
	return nil
}

// PushFailsWithBadCredentials asserts the registry's 401 reaches the caller
// as an error, and that neither the wrong password nor the right one appears
// in its text.
//
// An error crossing the Dagger boundary is rendered into a trace and a CI
// log, both of which outlive the run. A client library that echoed its
// request would put the credential in both, so the module scrubs the
// password out and this is what holds it to that.
func (t *Tests) PushFailsWithBadCredentials(ctx context.Context) error {
	reg, err := newRegistry(ctx)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "unauthorized")
	if err != nil {
		return err
	}
	wrong, err := uniqueName(ctx, "wrong")
	if err != nil {
		return err
	}
	secretName, err := uniqueName(ctx, "oci-bad-pwd")
	if err != nil {
		return err
	}

	client := dag.Oci().Registry("test-registry.invalid", dagger.OciRegistryOpts{
		Username: registryUser,
		Password: dag.SetSecret(secretName, wrong),
		Service:  reg.Service,
		Insecure: true,
	})

	_, err = client.PushImage(ctx, repo, "v1", []*dagger.Container{baseImage("linux/amd64")})
	if err == nil {
		return errors.New("PushImage with a wrong password succeeded")
	}
	text := err.Error()
	if strings.Contains(text, wrong) {
		return fmt.Errorf("the error text leaks the password that was used: %s", text)
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

// ReferrersListsAttachedArtifacts asserts that both artifacts attached to a
// subject come back from Referrers, and — first — that the registry answering
// is serving the native OCI 1.1 referrers API.
//
// That second assertion is the point of the test. oras falls back to the tag
// schema against a registry without /v2/<name>/referrers/<digest>, and a
// green suite over the fallback would be evidence about the fallback and
// nothing about GHCR, which serves the real endpoint.
func (t *Tests) ReferrersListsAttachedArtifacts(ctx context.Context) error {
	reg, err := newRegistry(ctx)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "referrers")
	if err != nil {
		return err
	}

	subject, err := reg.client().PushImage(ctx, repo, "v1", []*dagger.Container{baseImage("linux/amd64")})
	if err != nil {
		return fmt.Errorf("PushImage: %v", err)
	}
	if err := requireNativeReferrersAPI(ctx, reg, repo, subject); err != nil {
		return err
	}

	sbom, err := reg.attach(ctx, repo, subject, "sbom.json", sbomArtifactType)
	if err != nil {
		return err
	}
	attestation, err := reg.attach(ctx, repo, subject, "attestation.json", attestationArtifactType)
	if err != nil {
		return err
	}

	raw, err := reg.client().Referrers(ctx, repo, subject)
	if err != nil {
		return fmt.Errorf("Referrers: %v", err)
	}
	got, err := decodeDescriptors(raw)
	if err != nil {
		return err
	}
	return wantDigests(got, raw, sbom, attestation)
}

// ReferrersFiltersByArtifactType asserts the artifactType filter narrows the
// listing to one type. oras applies the filter client-side when the registry
// does not report having applied it, so this holds either way.
func (t *Tests) ReferrersFiltersByArtifactType(ctx context.Context) error {
	reg, err := newRegistry(ctx)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "referrers-filtered")
	if err != nil {
		return err
	}

	subject, err := reg.client().PushImage(ctx, repo, "v1", []*dagger.Container{baseImage("linux/amd64")})
	if err != nil {
		return fmt.Errorf("PushImage: %v", err)
	}
	sbom, err := reg.attach(ctx, repo, subject, "sbom.json", sbomArtifactType)
	if err != nil {
		return err
	}
	if _, err := reg.attach(ctx, repo, subject, "attestation.json", attestationArtifactType); err != nil {
		return err
	}

	raw, err := reg.client().Referrers(ctx, repo, subject, dagger.OciRegistryReferrersOpts{
		ArtifactType: sbomArtifactType,
	})
	if err != nil {
		return fmt.Errorf("Referrers filtered by %s: %v", sbomArtifactType, err)
	}
	got, err := decodeDescriptors(raw)
	if err != nil {
		return err
	}
	if err := wantDigests(got, raw, sbom); err != nil {
		return err
	}
	if got[0].ArtifactType != sbomArtifactType {
		return fmt.Errorf("referrer artifactType: want %q, got %q (%s)", sbomArtifactType, got[0].ArtifactType, raw)
	}
	return nil
}

// AttachFailsForUnknownSubject asserts that attaching to a manifest that is
// not in the repository fails, and that the error names the subject digest.
// Without the up-front resolve the registry would accept the referrer and
// leave it dangling, which nothing downstream can detect.
func (t *Tests) AttachFailsForUnknownSubject(ctx context.Context) error {
	reg, err := newRegistry(ctx)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "dangling")
	if err != nil {
		return err
	}
	if _, err := reg.client().PushImage(ctx, repo, "v1", []*dagger.Container{baseImage("linux/amd64")}); err != nil {
		return fmt.Errorf("PushImage: %v", err)
	}

	// A well-formed digest of content this repository has never seen.
	absent, err := dag.Random().Sha256(ctx)
	if err != nil {
		return fmt.Errorf("random sha256 (absent digest): %v", err)
	}
	subject := "sha256:" + absent

	content := dag.Directory().WithNewFile("sbom.json", "{}").File("sbom.json")
	if _, err := reg.client().Attach(ctx, repo, subject, content, sbomArtifactType); err == nil {
		return fmt.Errorf("Attach to %s: expected an error for a subject that is not in the repository", subject)
	} else if !strings.Contains(err.Error(), subject) {
		return fmt.Errorf("Attach error does not name the subject %s: %v", subject, err)
	}
	return nil
}

// attach uploads a one-file artifact against subject and returns its digest.
func (tr *testRegistry) attach(ctx context.Context, repo, subject, name, artifactType string) (string, error) {
	body, err := uniqueName(ctx, "body")
	if err != nil {
		return "", err
	}
	content := dag.Directory().WithNewFile(name, body).File(name)
	digest, err := tr.client().Attach(ctx, repo, subject, content, artifactType)
	if err != nil {
		return "", fmt.Errorf("Attach %s (%s): %v", name, artifactType, err)
	}
	return digest, nil
}

// descriptor is the slice of an OCI descriptor the referrer tests read.
type descriptor struct {
	MediaType    string `json:"mediaType"`
	ArtifactType string `json:"artifactType"`
	Digest       string `json:"digest"`
}

func decodeDescriptors(raw string) ([]descriptor, error) {
	var descs []descriptor
	if err := json.Unmarshal([]byte(raw), &descs); err != nil {
		return nil, fmt.Errorf("decode referrers %s: %v", raw, err)
	}
	return descs, nil
}

// wantDigests asserts the listing holds exactly the given digests, in any
// order — the referrers API guarantees no ordering.
func wantDigests(got []descriptor, raw string, want ...string) error {
	if len(got) != len(want) {
		return fmt.Errorf("want %d referrer(s), got %d (%s)", len(want), len(got), raw)
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g.Digest == w {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("referrer %s is missing from %s", w, raw)
		}
	}
	return nil
}

// PushArtifactThenFetchRoundTripsContent asserts that the bytes of a file in
// a pushed artifact come back identical: one layer per file, the file's own
// bytes, no archive wrapper the caller did not ask for.
func (t *Tests) PushArtifactThenFetchRoundTripsContent(ctx context.Context) error {
	reg, err := newRegistry(ctx)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "artifact")
	if err != nil {
		return err
	}
	payload, err := uniqueName(ctx, "payload")
	if err != nil {
		return err
	}

	contents := dag.Directory().WithNewFile("report.json", payload)
	if _, err := reg.client().PushArtifact(ctx, repo, "v1", contents, sbomArtifactType); err != nil {
		return fmt.Errorf("PushArtifact: %v", err)
	}

	raw, err := reg.client().Manifest(ctx, repo, "v1")
	if err != nil {
		return fmt.Errorf("Manifest: %v", err)
	}
	manifest, err := decodeManifest(raw)
	if err != nil {
		return err
	}
	if manifest.ArtifactType != sbomArtifactType {
		return fmt.Errorf("artifactType: want %q, got %q (manifest %s)", sbomArtifactType, manifest.ArtifactType, raw)
	}
	if len(manifest.Layers) != 1 {
		return fmt.Errorf("want exactly one layer, got %d (manifest %s)", len(manifest.Layers), raw)
	}
	if title := manifest.Layers[0].Annotations[titleAnnotation]; title != "report.json" {
		return fmt.Errorf("layer title: want %q, got %q (manifest %s)", "report.json", title, raw)
	}

	got, err := reg.client().Fetch(repo, manifest.Layers[0].Digest).Contents(ctx)
	if err != nil {
		return fmt.Errorf("Fetch %s: %v", manifest.Layers[0].Digest, err)
	}
	if got != payload {
		return fmt.Errorf("fetched contents: want %q, got %q", payload, got)
	}
	return nil
}

// AttachThenFetchRoundTripsContent asserts an attached file's bytes survive
// the round trip through the referrers API.
func (t *Tests) AttachThenFetchRoundTripsContent(ctx context.Context) error {
	reg, err := newRegistry(ctx)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "attach")
	if err != nil {
		return err
	}
	payload, err := uniqueName(ctx, "attestation")
	if err != nil {
		return err
	}

	subject, err := reg.client().PushImage(ctx, repo, "v1", []*dagger.Container{baseImage("linux/amd64")})
	if err != nil {
		return fmt.Errorf("PushImage: %v", err)
	}

	content := dag.Directory().WithNewFile("attestation.json", payload).File("attestation.json")
	referrer, err := reg.client().Attach(ctx, repo, subject, content, attestationArtifactType)
	if err != nil {
		return fmt.Errorf("Attach: %v", err)
	}

	raw, err := reg.client().Manifest(ctx, repo, referrer)
	if err != nil {
		return fmt.Errorf("Manifest of the referrer: %v", err)
	}
	manifest, err := decodeManifest(raw)
	if err != nil {
		return err
	}
	if manifest.Subject == nil || manifest.Subject.Digest != subject {
		return fmt.Errorf("referrer subject: want %s, got %+v (manifest %s)", subject, manifest.Subject, raw)
	}
	if len(manifest.Layers) != 1 {
		return fmt.Errorf("want exactly one layer, got %d (manifest %s)", len(manifest.Layers), raw)
	}

	got, err := reg.client().Fetch(repo, manifest.Layers[0].Digest).Contents(ctx)
	if err != nil {
		return fmt.Errorf("Fetch %s: %v", manifest.Layers[0].Digest, err)
	}
	if got != payload {
		return fmt.Errorf("fetched contents: want %q, got %q", payload, got)
	}
	return nil
}

// ociManifest is the slice of an OCI manifest these tests read back.
type ociManifest struct {
	MediaType    string `json:"mediaType"`
	ArtifactType string `json:"artifactType"`
	Layers       []struct {
		MediaType   string            `json:"mediaType"`
		Digest      string            `json:"digest"`
		Annotations map[string]string `json:"annotations"`
	} `json:"layers"`
	Subject *struct {
		Digest string `json:"digest"`
	} `json:"subject"`
}

func decodeManifest(raw string) (*ociManifest, error) {
	var m ociManifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("decode manifest %s: %v", raw, err)
	}
	return &m, nil
}

// PushImagePushesAllVariants asserts that more than one variant becomes one
// manifest list naming every platform, rather than the last push winning the
// tag.
func (t *Tests) PushImagePushesAllVariants(ctx context.Context) error {
	reg, err := newRegistry(ctx)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "multiarch")
	if err != nil {
		return err
	}

	variants := []*dagger.Container{baseImage("linux/amd64"), baseImage("linux/arm64")}
	if _, err := reg.client().PushImage(ctx, repo, "v1", variants); err != nil {
		return fmt.Errorf("PushImage: %v", err)
	}

	raw, err := reg.client().Manifest(ctx, repo, "v1")
	if err != nil {
		return fmt.Errorf("Manifest: %v", err)
	}
	platforms, err := indexPlatforms(raw)
	if err != nil {
		return err
	}
	return wantPlatforms(platforms, raw, "linux/amd64", "linux/arm64")
}

// CopyPreservesAllManifests asserts that copying a multi-platform image keeps
// every platform. skopeo needed --all for this; a copy that silently reduced
// a manifest list to the running platform would break every non-amd64
// consumer of a copied image.
func (t *Tests) CopyPreservesAllManifests(ctx context.Context) error {
	reg, err := newRegistry(ctx)
	if err != nil {
		return err
	}
	src, err := uniqueName(ctx, "copy-src")
	if err != nil {
		return err
	}
	dst, err := uniqueName(ctx, "copy-dst")
	if err != nil {
		return err
	}

	variants := []*dagger.Container{baseImage("linux/amd64"), baseImage("linux/arm64")}
	pushed, err := reg.client().PushImage(ctx, src, "v1", variants)
	if err != nil {
		return fmt.Errorf("PushImage: %v", err)
	}

	endpoint, err := reg.endpoint(ctx)
	if err != nil {
		return err
	}
	copied, err := reg.client().Copy(ctx, fmt.Sprintf("%s/%s:v1", endpoint, src), dst, "v1")
	if err != nil {
		return fmt.Errorf("Copy: %v", err)
	}
	if copied != pushed {
		return fmt.Errorf("Copy digest %s does not match the pushed digest %s", copied, pushed)
	}

	raw, err := reg.client().Manifest(ctx, dst, "v1")
	if err != nil {
		return fmt.Errorf("Manifest of the copy: %v", err)
	}
	platforms, err := indexPlatforms(raw)
	if err != nil {
		return err
	}
	return wantPlatforms(platforms, raw, "linux/amd64", "linux/arm64")
}

// indexPlatforms pulls the os/architecture pairs out of a manifest list.
func indexPlatforms(raw string) ([]string, error) {
	var index struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal([]byte(raw), &index); err != nil {
		return nil, fmt.Errorf("decode manifest %s: %v", raw, err)
	}
	if len(index.Manifests) == 0 {
		return nil, fmt.Errorf("manifest is not a manifest list: %s", raw)
	}
	platforms := make([]string, 0, len(index.Manifests))
	for _, m := range index.Manifests {
		platforms = append(platforms, m.Platform.OS+"/"+m.Platform.Architecture)
	}
	return platforms, nil
}

// wantPlatforms asserts the manifest list names exactly the given platforms.
func wantPlatforms(got []string, raw string, want ...string) error {
	if len(got) != len(want) {
		return fmt.Errorf("manifest list names %v, want %v (manifest %s)", got, want, raw)
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("manifest list %v is missing %s (manifest %s)", got, w, raw)
		}
	}
	return nil
}

// AnnotationsSurvivePush asserts that an annotation set on the container with
// WithAnnotation is readable through Manifest after the push. Annotations are
// how provenance, source links and SBOM pointers travel with an image, and a
// push that quietly drops them is indistinguishable from one that works.
func (t *Tests) AnnotationsSurvivePush(ctx context.Context) error {
	reg, err := newRegistry(ctx)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "annotated")
	if err != nil {
		return err
	}
	value, err := uniqueName(ctx, "revision")
	if err != nil {
		return err
	}

	img := baseImage("linux/amd64").WithAnnotation(annotationKey, value)
	if _, err := reg.client().PushImage(ctx, repo, "v1", []*dagger.Container{img}); err != nil {
		return fmt.Errorf("PushImage: %v", err)
	}

	raw, err := reg.client().Manifest(ctx, repo, "v1")
	if err != nil {
		return fmt.Errorf("Manifest: %v", err)
	}
	var manifest struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		return fmt.Errorf("decode manifest %s: %v", raw, err)
	}
	if got := manifest.Annotations[annotationKey]; got != value {
		return fmt.Errorf("annotation %s: want %q, got %q (manifest %s)", annotationKey, value, got, raw)
	}
	return nil
}

// ResolveFailsForMissingTag asserts that resolving a tag nothing was ever
// pushed to fails, and that the error names the tag — an error that only says
// "not found" leaves the caller guessing which of its references was wrong.
func (t *Tests) ResolveFailsForMissingTag(ctx context.Context) error {
	reg, err := newRegistry(ctx)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "missing")
	if err != nil {
		return err
	}
	tag, err := uniqueName(ctx, "tag")
	if err != nil {
		return err
	}

	_, err = reg.client().Resolve(ctx, repo, tag)
	if err == nil {
		return fmt.Errorf("Resolve(%s, %s): expected an error for a tag that was never pushed", repo, tag)
	}
	if !strings.Contains(err.Error(), tag) {
		return fmt.Errorf("Resolve error does not name the tag %q: %v", tag, err)
	}
	return nil
}
