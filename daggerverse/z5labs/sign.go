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
//
// # What it costs
//
// One signature per manifest, and in the keyless mode one transparency log
// upload per signature, issued one after another against a shared public
// service. A four-platform release is therefore five serial round trips to
// rekor.sigstore.dev inside the publish, each bounded by rekorTimeout. That
// is a publish-latency and third-party-availability characteristic rather
// than a defect, and it is written down here so it is a known cost rather
// than a surprise in a release that suddenly takes minutes.
//
// repository and digest are taken as arguments rather than read off a
// buildFacts, so this cannot drift from the repository the caller's error
// messages name. Publish rebuilds its facts per repository, which made the
// two agree; agreeing by construction is better than agreeing by care.
func (a *App) signImage(
	ctx context.Context,
	registry *dagger.OciRegistry,
	sgn *signer,
	repository, digest string,
) error {
	digests, err := signableDigests(ctx, registry, repository, digest)
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
	reference := a.Registry + "/" + repository
	for _, target := range digests {
		if err := a.signManifest(ctx, registry, sgn, repository, reference, target); err != nil {
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
//
// # It walks exactly one level, and refuses rather than assuming
//
// An index whose child is itself an index would leave the innermost
// manifests — the ones a runtime actually pulls — unsigned, while
// `cosign verify <tag>` kept passing. That is the failure this whole
// function exists to close, reintroduced one level down, so a nested index
// is an error rather than something walked past. Nothing this pipeline
// pushes can produce one: PushImageUntagged writes a flat index of the
// platform variants. The check is here because "cannot happen today" and
// "is checked" are different, and only one of them survives a change to the
// producer.
//
// Duplicates are collapsed. An index listing one digest twice would
// otherwise sign it twice and push the same tag twice — harmless, since the
// second push replaces the first, but it would make the number of signature
// tags disagree with the number of distinct manifests, which is what the
// suite compares against.
func signableDigests(ctx context.Context, registry *dagger.OciRegistry, repository, digest string) ([]string, error) {
	raw, err := registry.Manifest(ctx, repository, digest)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s from %s: %v", digest, repository, err)
	}
	var doc struct {
		Manifests []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, fmt.Errorf("decode manifest %s from %s: %v", digest, repository, err)
	}
	out := []string{digest}
	seen := map[string]bool{digest: true}
	for _, child := range doc.Manifests {
		if strings.TrimSpace(child.Digest) == "" {
			return nil, fmt.Errorf("manifest %s in %s lists an entry with no digest", digest, repository)
		}
		if isIndexMediaType(child.MediaType) {
			return nil, fmt.Errorf(
				"manifest %s in %s lists %s, which is itself an index; signing it without descending would leave the manifests beneath it unsigned while a verify against the tag still passed",
				digest, repository, child.Digest)
		}
		if seen[child.Digest] {
			continue
		}
		seen[child.Digest] = true
		out = append(out, child.Digest)
	}
	return out, nil
}

// isIndexMediaType reports whether a descriptor names a manifest list.
// Both spellings are here because a registry serves whichever the pusher
// wrote, and a check that knew only the OCI one would walk past a Docker
// manifest list.
func isIndexMediaType(mediaType string) bool {
	switch mediaType {
	case "application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json":
		return true
	default:
		return false
	}
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
	if !s.keyless {
		return out, nil
	}
	// The mode is read off the signer, never off the chain, and a keyless
	// signer without one is a refusal rather than a quieter signature. What
	// it would otherwise publish is a signature with no certificate and no
	// log entry: the identity command in Publish's doc comment has nothing
	// to check, and the supplied-key command has no public key to be given,
	// so the image ships signed and verifiable by nobody. Unreachable today,
	// because fulcioCertificate refuses an empty chain — which is exactly
	// why it costs nothing to say so here as well.
	if len(s.chain) == 0 {
		return nil, fmt.Errorf(
			"keyless signing produced no certificate chain, so nothing would vouch for the signing key; " +
				"refusing to publish a signature no documented verify command can check")
	}
	// And a keyless signer with no log to record in is a refusal for the same
	// reason, stated locally rather than left to be inferred from the fact
	// that the only constructor sets both fields together. Unreachable today:
	// newSigner sets rekorURL from sigstoreEndpoints on every keyless signer,
	// and sigstoreEndpoints returns either the public log or a resolved one,
	// never "". The check is here so that a reader of this call site does not
	// have to go and establish that, and so that a future constructor cannot
	// make it false quietly — what it would otherwise produce is a request to
	// the relative URL "/api/v1/log/entries", failing a publish with a parse
	// error that names no cause a caller could act on.
	if strings.TrimSpace(s.rekorURL) == "" {
		return nil, fmt.Errorf(
			"keyless signing has no transparency log to record in, so nothing would establish the signing certificate " +
				"was live when it signed; refusing to publish a signature that expires into unverifiability")
	}
	certificate, chain, err := splitCertificateChain(s.chain)
	if err != nil {
		return nil, err
	}
	out[cosignCertificateAnnotation] = certificate
	if chain != "" {
		out[cosignChainAnnotation] = chain
	}
	bundle, err := rekorBundle(ctx, payload, signature, certificate, s.rekorURL)
	if err != nil {
		return nil, err
	}
	out[cosignBundleAnnotation] = bundle
	return out, nil
}

// splitCertificateChain separates the leaf certificate from the
// intermediates, because cosign reads them from two different annotations:
// the leaf is the identity, the rest is how a verifier walks to a root.
//
// It is strict about two things that a lenient parse would swallow, and both
// matter because what is being assembled is the identity half of a
// signature — the half a verifier decides whom to trust from.
//
//   - A block that is not a CERTIFICATE is refused rather than skipped or
//     published. A lenient reader hands whatever came first to
//     dev.sigstore.cosign/certificate, so a PKCS#7 body or a key
//     concatenated into the response would be published as the signing
//     identity.
//   - Bytes left over after the last block are refused. pem.Decode stops at
//     the first byte it cannot parse, so a chain whose second certificate
//     is truncated would otherwise yield a leaf, an empty chain and no
//     error — a signature no verifier can walk to a root, published as
//     though it were complete.
func splitCertificateChain(chain []byte) (string, string, error) {
	const certificateBlock = "CERTIFICATE"
	var leaf bytes.Buffer
	var rest bytes.Buffer
	remaining := chain
	for {
		block, tail := pem.Decode(remaining)
		if block == nil {
			break
		}
		if block.Type != certificateBlock {
			return "", "", fmt.Errorf(
				"signing certificate chain carries a %q PEM block where a %s was expected",
				block.Type, certificateBlock)
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
	if len(bytes.TrimSpace(remaining)) > 0 {
		return "", "", fmt.Errorf(
			"signing certificate chain carries %d trailing bytes that are not a PEM block, so it is incomplete",
			len(bytes.TrimSpace(remaining)))
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
// rekorURL is the log to record in, which travels on the signer beside the
// certificate rather than being read from a constant here — see
// sigstoreEndpoints for why the CA and the log cannot be redirected apart.
//
// The test suite drives this against a log of its own, which verifies the
// signature against the certificate it was handed before it answers. So the
// entry this builds is checked, rather than merely sent; what a local log
// cannot establish is anything about the public log's availability, its
// inclusion proof, or a verifier's willingness to trust its
// countersignature. The suite says as much where it asserts.
func rekorBundle(ctx context.Context, payload []byte, signature, certificate, rekorURL string) (string, error) {
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
		rekorURL+"/api/v1/log/entries", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build transparency log request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("record signature in the transparency log at %s: %v", rekorURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read transparency log response: %v", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("transparency log at %s returned %s%s", rekorURL, resp.Status, responseDetail(raw))
	}

	// The response is keyed by the entry's UUID, which is not known until
	// the log assigns it, so the one entry is taken out of a map of one
	// rather than read from a named field. "Of one" is checked rather than
	// assumed: ranging a map of two and returning from the first iteration
	// would pick one of them by hash order, so a log that ever answered with
	// more than one entry would embed a bundle chosen at random.
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
	if len(entries) != 1 {
		return "", fmt.Errorf("transparency log at %s recorded %d entries for one signature, want exactly 1",
			rekorURL, len(entries))
	}
	for _, entry := range entries {
		if entry.Verification.SignedEntryTimestamp == "" {
			return "", fmt.Errorf("transparency log entry from %s carried no signed entry timestamp", rekorURL)
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
	// Unreachable: the length check above already refused an empty map.
	return "", fmt.Errorf("transparency log at %s recorded no entry", rekorURL)
}
