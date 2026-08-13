package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"dagger/z-5-labs/internal/dagger"
)

// The cosign signature layout this module writes.
//
// It is cosign's rather than something z5labs invented, and that is the
// whole decision. A signature is worth exactly what the command verifying
// it is worth, and the command a consumer already has is:
//
//	cosign verify ghcr.io/<owner>/<app>:<version> \
//	  --certificate-identity-regexp '^https://github.com/<owner>/<repo>/\.github/workflows/' \
//	  --certificate-oidc-issuer https://token.actions.githubusercontent.com
//
// Publishing a signature stock cosign cannot read would mean publishing a
// verifier alongside it, and a verification story that ships with its own
// verifier is one nobody runs. So the layout is copied, not designed: for
// a manifest at digest `sha256:<hex>`, a one-layer OCI image manifest is
// pushed to the tag `sha256-<hex>.sig` in the same repository, its layer
// holding the simple-signing payload and its annotations holding the
// signature and the certificate that vouches for the key.
//
// The alternative was the OCI 1.1 referrers form, which cosign writes under
// `--registry-referrers-mode=oci-1-1` and reads only under
// `--experimental-oci11`. It is the better shape — a signature is a
// referrer of the thing it signs, and the tag scheme is a workaround for
// registries that had no referrers API — and it is rejected here for one
// reason: it is not what a consumer's `cosign verify` reads today. Revisit
// when reading it stops needing a flag.
const (
	// simpleSigningMediaType is the layer media type cosign takes a
	// signature payload out of.
	simpleSigningMediaType = "application/vnd.dev.cosign.simplesigning.v1+json"
	// cosignSignatureType is the `critical.type` of every simple signing
	// payload, and is part of the signed bytes: a payload cannot be
	// replayed as some other kind of claim about the same digest.
	cosignSignatureType = "cosign container image signature"
	// signatureTagSuffix completes the tag a signature lands on.
	signatureTagSuffix = ".sig"

	// The annotations cosign reads beside the payload. The signature key is
	// spelled `dev.cosignproject.cosign/...` and the rest
	// `dev.sigstore.cosign/...`; the inconsistency is upstream's and
	// copying it exactly is the point.
	cosignSignatureAnnotation   = "dev.cosignproject.cosign/signature"
	cosignCertificateAnnotation = "dev.sigstore.cosign/certificate"
	cosignChainAnnotation       = "dev.sigstore.cosign/chain"
	cosignBundleAnnotation      = "dev.sigstore.cosign/bundle"
)

// defaultRekorURL is the public sigstore transparency log.
//
// Keyless signing without a log entry is not a weaker version of keyless
// signing, it is a broken one: the certificate that binds the ephemeral key
// to a workload identity lives for ten minutes, so by the time anybody
// verifies, it has expired and nothing establishes that it was valid when
// it signed. The log entry is what does — it is countersigned by the log at
// upload time, so a verifier checks "this signature existed while the
// certificate was live" rather than having to trust a clock. That is the
// property that makes signing.go's "no key this pipeline has to manage"
// a trade rather than a hole.
const defaultRekorURL = "https://rekor.sigstore.dev"

// rekorTimeout bounds the log upload for the same reason fulcioTimeout
// bounds the certificate request.
const rekorTimeout = 60 * time.Second

// signImage signs the published manifest list and every per-platform
// manifest beneath it.
//
// # Why every manifest and not just the one the tag names
//
// A consumer verifies `<repo>:<version>`, which resolves to the manifest
// list, and their runtime then pulls the per-platform manifest for their
// architecture — a different digest, which the index signature says nothing
// about. Signing only the index leaves the bytes that actually run
// unsigned while `cosign verify` against the tag still passes, which is the
// worst available outcome: a verification that reports success over
// something it did not check. So each digest is signed on its own, and
// `cosign verify <repo>@<per-platform digest>` passes too.
//
// This is what cosign's own `--recursive` does, and the reason it exists.
//
// # Where it runs
//
// After the attestations and before the tag, which keeps Publish's rule
// that no *name* reaches an image until everything that can fail has
// succeeded. The signature manifests are themselves tagged — the layout
// requires it, the tag is computed from the digest — so a publish that
// fails after this point leaves `sha256-<hex>.sig` tags behind pointing at
// a signature for a manifest no release tag resolves to. That is why this
// step is last rather than first: it is the only one that can leave a name
// behind at all, so it gets the smallest window. The leftovers are inert —
// nothing resolves to them without already knowing the digest, and a
// re-publish of the same bytes overwrites them.
func (a *App) signImage(
	ctx context.Context,
	registry *dagger.OciRegistry,
	sgn *signer,
	facts buildFacts,
) error {
	digests, err := signableDigests(ctx, registry, facts.Repository, facts.Digest)
	if err != nil {
		return err
	}
	// The reference recorded in every payload is the repository, without a
	// tag and without a digest. That is what cosign writes, and it is the
	// only honest choice for the per-platform manifests: they are reachable
	// under no tag of their own, so naming one in their payload would put a
	// claim in the signed bytes that does not resolve. What ties a payload
	// to a specific manifest is the digest beside it, which is the field
	// cosign checks.
	reference := a.Registry + "/" + facts.Repository
	for _, digest := range digests {
		if err := a.signManifest(ctx, registry, sgn, facts.Repository, reference, digest); err != nil {
			return err
		}
	}
	return nil
}

// signManifest signs one manifest digest and pushes the signature.
func (a *App) signManifest(
	ctx context.Context,
	registry *dagger.OciRegistry,
	sgn *signer,
	repository, reference, digest string,
) error {
	payload, err := simpleSigningPayload(reference, digest)
	if err != nil {
		return err
	}
	signature, err := sgn.signBytes(payload)
	if err != nil {
		return fmt.Errorf("sign %s: %v", digest, err)
	}
	annotations, err := sgn.signatureAnnotations(ctx, payload, signature)
	if err != nil {
		return fmt.Errorf("sign %s: %v", digest, err)
	}
	encoded, err := json.Marshal(annotations)
	if err != nil {
		return fmt.Errorf("encode signature annotations for %s: %v", digest, err)
	}
	// The payload is content-addressed by writeWorkdirFile, so two digests
	// signed in one publish do not collide on one path despite sharing a
	// file name.
	file, err := writeWorkdirFile("signature.json", payload)
	if err != nil {
		return fmt.Errorf("write signature payload for %s: %v", digest, err)
	}
	tag, err := signatureTag(digest)
	if err != nil {
		return err
	}
	_, err = registry.PushLayer(ctx, repository, tag, file, simpleSigningMediaType,
		dagger.OciRegistryPushLayerOpts{Annotations: string(encoded)})
	if err != nil {
		return fmt.Errorf("push signature for %s to %s:%s: %v", digest, repository, tag, err)
	}
	return nil
}

// signableDigests is the published digest followed by every manifest listed
// beneath it, which is the set a recursive signature has to cover.
//
// A single-platform publish stores a bare image manifest rather than an
// index; that has no `manifests` array, so the set is the one digest and
// the recursion costs nothing. Reading the manifest back rather than
// deriving the children from a.Variants is deliberate: the child digests
// are the registry's account of what it stored, and a signature over
// anything else would be a signature over what this module believed it had
// pushed.
func signableDigests(ctx context.Context, registry *dagger.OciRegistry, repository, digest string) ([]string, error) {
	raw, err := registry.Manifest(ctx, repository, digest)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s from %s: %v", digest, repository, err)
	}
	var doc struct {
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, fmt.Errorf("decode manifest %s from %s: %v", digest, repository, err)
	}
	out := []string{digest}
	for _, child := range doc.Manifests {
		if strings.TrimSpace(child.Digest) == "" {
			return nil, fmt.Errorf("manifest %s in %s lists an entry with no digest", digest, repository)
		}
		out = append(out, child.Digest)
	}
	return out, nil
}

// signatureTag is the tag a signature for digest lands on: the digest with
// its algorithm separator turned into a dash, plus ".sig".
//
// A registry tag cannot contain a colon, which is the entire reason the
// layout is spelled this way rather than as the digest itself.
func signatureTag(digest string) (string, error) {
	algorithm, encoded, ok := strings.Cut(digest, ":")
	if !ok || algorithm == "" || encoded == "" {
		return "", fmt.Errorf("manifest digest %q is not <algorithm>:<hex>", digest)
	}
	return algorithm + "-" + encoded + signatureTagSuffix, nil
}

// simpleSigningPayload renders the document that gets signed.
//
// The shape is Red Hat's "simple signing" payload, which cosign adopted and
// which is why the field names read like a Docker reference rather than
// like anything in this repository. It is rendered from typed structs so
// the bytes are the same on every run and so a field cannot be quietly
// dropped by a map literal typo.
func simpleSigningPayload(reference, digest string) ([]byte, error) {
	if strings.TrimSpace(reference) == "" {
		return nil, fmt.Errorf("signature payload requires an image reference")
	}
	if _, encoded, ok := strings.Cut(digest, ":"); !ok || encoded == "" {
		return nil, fmt.Errorf("manifest digest %q is not <algorithm>:<hex>", digest)
	}
	type identity struct {
		DockerReference string `json:"docker-reference"`
	}
	type image struct {
		DockerManifestDigest string `json:"docker-manifest-digest"`
	}
	type critical struct {
		Identity identity `json:"identity"`
		Image    image    `json:"image"`
		Kind     string   `json:"type"`
	}
	// Optional is present and null rather than absent, which is what cosign
	// emits. It is where cosign puts `--annotations`; this module sets none,
	// because everything a caller could annotate an image with is already in
	// the provenance predicate, where it is attested rather than asserted.
	payload := struct {
		Critical critical `json:"critical"`
		Optional any      `json:"optional"`
	}{
		Critical: critical{
			Identity: identity{DockerReference: reference},
			Image:    image{DockerManifestDigest: digest},
			Kind:     cosignSignatureType,
		},
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode signature payload: %v", err)
	}
	return out, nil
}

// signBytes signs a payload the way cosign does — ECDSA over its SHA-256,
// ASN.1 DER, base64 — and returns the annotation value.
//
// Note what is *not* here: no DSSE wrapper. The attestations are DSSE
// because a verifier has to be stopped from replaying an in-toto statement
// as some other kind of document; a simple signing payload names its own
// type in the signed bytes, so it carries that property itself. Wrapping it
// anyway would produce something cosign cannot read, which is the one
// property this layout exists to have.
func (s *signer) signBytes(payload []byte) (string, error) {
	digest := sha256.Sum256(payload)
	sig, err := ecdsa.SignASN1(rand.Reader, s.key, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign payload: %v", err)
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// signatureAnnotations is what goes on the signature layer beside the
// payload: always the signature, and in the keyless mode the certificate
// chain and the transparency log entry that make it verifiable by identity
// rather than by key.
//
// The two modes produce two different verifying commands, and both are
// written down rather than one being treated as the real one:
//
//   - Keyless, which is what a CI publish does:
//
//     cosign verify <ref> \
//     --certificate-identity-regexp '^https://github.com/<owner>/<repo>/\.github/workflows/' \
//     --certificate-oidc-issuer https://token.actions.githubusercontent.com
//
//   - A caller-supplied key, which is for a build that cannot reach the
//     public CA. There is no certificate to bind the key to an identity and
//     no log entry, so the verifier needs the public key and has to be told
//     not to look for one:
//
//     cosign verify <ref> --key cosign.pub --insecure-ignore-tlog=true
//
// The second is weaker and says so: `--insecure-ignore-tlog` is spelled the
// way it is on purpose, and a caller who does not want to hand their
// consumers that flag should not be supplying a key.
func (s *signer) signatureAnnotations(ctx context.Context, payload []byte, signature string) (map[string]string, error) {
	out := map[string]string{cosignSignatureAnnotation: signature}
	if len(s.chain) == 0 {
		return out, nil
	}
	certificate, chain, err := splitCertificateChain(s.chain)
	if err != nil {
		return nil, err
	}
	out[cosignCertificateAnnotation] = certificate
	if chain != "" {
		out[cosignChainAnnotation] = chain
	}
	bundle, err := rekorBundle(ctx, payload, signature, certificate)
	if err != nil {
		return nil, err
	}
	out[cosignBundleAnnotation] = bundle
	return out, nil
}

// splitCertificateChain separates the leaf certificate from the
// intermediates, because cosign reads them from two different annotations:
// the leaf is the identity, the rest is how a verifier walks to a root.
func splitCertificateChain(chain []byte) (string, string, error) {
	var leaf bytes.Buffer
	var rest bytes.Buffer
	remaining := chain
	for {
		block, tail := pem.Decode(remaining)
		if block == nil {
			break
		}
		remaining = tail
		target := &rest
		if leaf.Len() == 0 {
			target = &leaf
		}
		if err := pem.Encode(target, block); err != nil {
			return "", "", fmt.Errorf("re-encode signing certificate: %v", err)
		}
	}
	if leaf.Len() == 0 {
		return "", "", fmt.Errorf("signing certificate chain carried no PEM certificate")
	}
	return leaf.String(), rest.String(), nil
}

// rekorBundle uploads the signature to the public transparency log and
// returns the bundle cosign carries beside it.
//
// The entry is a `hashedrekord`: the log is told the payload's hash, the
// signature and the certificate, never the payload. That matters for a
// private image — the log is public and permanent, and a simple signing
// payload names the repository.
//
// What comes back and is stored is the log's countersignature (the signed
// entry timestamp) over the entry, plus enough of the entry to check it.
// That is what a verifier uses to establish the certificate was valid at
// signing time without contacting the log at all, which is why the bundle
// is embedded rather than left to be looked up: a consumer verifying from
// inside a network that cannot reach rekor.sigstore.dev still can.
//
// Like fulcioCertificate, this is a part of the publish path the test suite
// cannot exercise — it needs the live public log, and the suite runs against
// a registry with no sigstore beside it. The suite covers the layout and the
// signature by driving the supplied-key mode, where neither this nor a
// certificate exists; what is untested here is the HTTP shape of one
// request.
func rekorBundle(ctx context.Context, payload []byte, signature, certificate string) (string, error) {
	sum := sha256.Sum256(payload)
	body, err := json.Marshal(map[string]any{
		"apiVersion": "0.0.1",
		"kind":       "hashedrekord",
		"spec": map[string]any{
			"data": map[string]any{
				"hash": map[string]string{
					"algorithm": "sha256",
					"value":     hex.EncodeToString(sum[:]),
				},
			},
			"signature": map[string]any{
				"content": signature,
				"publicKey": map[string]string{
					"content": base64.StdEncoding.EncodeToString([]byte(certificate)),
				},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("encode transparency log entry: %v", err)
	}

	ctx, cancel := context.WithTimeout(ctx, rekorTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		defaultRekorURL+"/api/v1/log/entries", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build transparency log request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("record signature in the transparency log at %s: %v", defaultRekorURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read transparency log response: %v", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("transparency log at %s returned %s", defaultRekorURL, resp.Status)
	}

	// The response is keyed by the entry's UUID, which is not known until
	// the log assigns it, so the one entry is taken out of a map of one
	// rather than read from a named field.
	var entries map[string]struct {
		Body           string `json:"body"`
		IntegratedTime int64  `json:"integratedTime"`
		LogID          string `json:"logID"`
		LogIndex       int64  `json:"logIndex"`
		Verification   struct {
			SignedEntryTimestamp string `json:"signedEntryTimestamp"`
		} `json:"verification"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return "", fmt.Errorf("decode transparency log response: %v", err)
	}
	for _, entry := range entries {
		if entry.Verification.SignedEntryTimestamp == "" {
			return "", fmt.Errorf("transparency log entry from %s carried no signed entry timestamp", defaultRekorURL)
		}
		// The field names are capitalized because cosign's bundle type
		// spells them that way; they are a wire format, not a style choice.
		bundle, err := json.Marshal(map[string]any{
			"SignedEntryTimestamp": entry.Verification.SignedEntryTimestamp,
			"Payload": map[string]any{
				"body":           entry.Body,
				"integratedTime": entry.IntegratedTime,
				"logIndex":       entry.LogIndex,
				"logID":          entry.LogID,
			},
		})
		if err != nil {
			return "", fmt.Errorf("encode transparency log bundle: %v", err)
		}
		return string(bundle), nil
	}
	return "", fmt.Errorf("transparency log at %s recorded no entry", defaultRekorURL)
}
