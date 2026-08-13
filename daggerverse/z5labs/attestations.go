package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"dagger/z-5-labs/internal/dagger"
)

// The artifact types the SBOMs are attached under. A consumer filters
// referrers by these, which is what "distinguished by predicate type" means
// at the registry: the SBOM formats are told apart from each other and from
// the provenance without opening any of them.
//
// They live here rather than beside the generation because they are a
// contract with whoever lists referrers, not a detail of the `go` module
// that wrote the bytes.
//
// Referrers are the *only* way these are discoverable — nothing here writes
// a second copy under cosign's `sha256-<hex>.att` tag, so
// `cosign verify-attestation` reports no attestations against an image this
// module published. That is a decision, and the package doc records it
// along with the commands that do find them; see "Why the documents are
// referrers alone" in main.go.
const (
	spdxArtifactType      = "application/spdx+json"
	cycloneDxArtifactType = "application/vnd.cyclonedx+json"
)

// documentStem names one platform's pair of documents.
//
// It is the repository's last segment and the platform, because that is
// what an App knows: it holds no binary name — it does not know it came
// from a language chain at all — and the documents describe the published
// image rather than any artifact inside it. A consumer picks a platform's
// document off the org.opencontainers.image.title annotation, so the
// platform has to be in here; the "/" in it becomes a "-" so the value is
// a legal file name.
func documentStem(repository string, platform dagger.Platform) string {
	return basenameAfterSlash(repository) + "-" + strings.ReplaceAll(string(platform), "/", "-")
}

// attachAttestations attaches this app's documents and its signed
// provenance to the digest that was just published.
//
// # Where this runs, and why it is not the last step
//
// Publish pushes the manifest list *untagged*, calls this, and moves the tag
// only once this has returned. That ordering is the answer to devex#361 and it
// is the reason PushImageUntagged and Tag exist on the oci module at all, so it
// is worth stating what it buys before someone simplifies it back.
//
// The failure it exists to prevent is the one devex#360 produced: the image was
// pushed and tagged, an attach then failed on the registry, and what was left
// behind was a tag a consumer could pull, carrying no attestations, beside a
// red build claiming the publish had failed. Nobody resolving that tag can tell
// "the attach failed" from "nobody attaches anything here", which is the same
// ambiguity newSigner refuses to create by making an unprovenanced publish fail
// rather than proceed. A referrer needs its subject present in the repository,
// so nothing can be attached before the push; what can move is the tag, and
// moving it last makes a failed attach leave an unreferenced manifest that
// nothing names. That is the blast radius of a failed push, which is the one
// callers already know how to reason about.
//
// Two consequences to keep in mind:
//
//   - Re-publishing a version that already exists is safer, not riskier. A
//     failed attach leaves the old tag exactly where it was, still pointing at
//     the previous — fully attested — image, because nothing writes the tag
//     until every attach has landed.
//   - The provenance predicate names the tag this is about to be published
//     under, and is signed before the tag exists. That is not a lie waiting to
//     happen: if the tagging step fails the digest stays unreferenced, so the
//     statement describes something nobody can reach, rather than describing a
//     tag that resolves elsewhere.
//
// The two orderings not taken, recorded so they are rejected rather than
// rediscovered:
//
//   - *Do every fallible thing before the push* — build the documents, sign the
//     provenance, hold the bytes, and let the push be the only thing that can
//     fail. It narrows the window without closing it: attaching is itself a
//     registry operation, and devex#360 failed on the registry side with every
//     byte already in hand. Publish does the cheap half of this anyway, calling
//     newSigner before the first byte moves, because a run that cannot produce
//     provenance should not push at all.
//   - *Treat a failed attach as advisory* — publish stays green, the failure is
//     a warning. Rejected on principle rather than on balance: devex#400 states
//     verifiability as one of the four things this module exists to enforce, and
//     an attestation a publish is allowed to skip is one no consumer can rely on.
//     It is also what newSigner already decided for provenance, and one pipeline
//     cannot hold both answers.
//
// # What this function knows
//
// The split of responsibility here is the point. The language chain knew
// what it compiled and produced the documents; this knows which identity
// vouched for the run; and the `oci` module attaches bytes to a digest
// without knowing that any of them are an SBOM or an attestation — it is
// handed a file and an artifact type, and that is all it ever learns.
//
// This module sits between those two and does know, because assembling one
// document out of several is not something a party ignorant of the format
// can do. What #330 asked for and what still holds is that the *attachment*
// stays ignorant: nothing below this function learns what any of these
// bytes mean.
//
// One document pair per platform, not one per release. A manifest list is
// several images, each with its own bytes and its own contents, and a
// single document claiming to describe "the" image would have to pick one
// of them and be wrong about the rest. The documents are told apart by
// their title annotation; the artifact type stays the format, because that
// is what a consumer filters on.
//
// The pair is *assembled* here rather than carried from the build, because
// the subject is the digest that was just published and it does not exist
// until the push has happened. What was carried from the build is one
// document per contribution — see sbom.go, which is where the reasoning for
// the whole arrangement lives. The fallible half of assembly, reading and
// parsing those documents, has already happened by the time this runs:
// Publish resolves them before the first byte moves, so a run that cannot
// produce a complete document fails without leaving an image behind.
func (a *App) attachAttestations(
	ctx context.Context,
	registry *dagger.OciRegistry,
	sgn *signer,
	facts buildFacts,
	boms []variantBom,
) error {
	assembledAt := time.Now()
	for _, vb := range boms {
		stem := documentStem(facts.Repository, vb.Platform)
		ib := vb.assemble(facts.Repository, facts.Digest, a.Version, assembledAt)
		rendered := []struct {
			name   string
			kind   string
			render func() ([]byte, error)
		}{
			{name: stem + ".spdx.json", kind: spdxArtifactType, render: ib.spdxDocument},
			{name: stem + ".cdx.json", kind: cycloneDxArtifactType, render: ib.cycloneDxDocument},
		}
		for _, doc := range rendered {
			raw, err := doc.render()
			if err != nil {
				return fmt.Errorf("assemble %s: %v", doc.name, err)
			}
			file, err := writeWorkdirFile(doc.name, raw)
			if err != nil {
				return fmt.Errorf("write %s: %v", doc.name, err)
			}
			if _, err := registry.Attach(ctx, facts.Repository, facts.Digest, file, doc.kind); err != nil {
				return fmt.Errorf("attach %s to %s: %v", doc.name, facts.Digest, err)
			}
		}
	}

	statement, err := provenanceStatement(sgn.identity, facts, time.Now())
	if err != nil {
		return err
	}
	envelope, err := sgn.dsseEnvelope(statement)
	if err != nil {
		return err
	}
	// Named after the repository published to, which is the thing the
	// statement is about. A repository is "<owner>/<name>" on any registry
	// without single-segment repositories — GHCR, which is the shape this
	// module's own guidance leads to — so the name carries a separator and
	// writeWorkdirFile is built to take one.
	file, err := writeWorkdirFile(facts.Repository+".intoto.jsonl", envelope)
	if err != nil {
		return fmt.Errorf("write provenance envelope: %v", err)
	}
	if _, err := registry.Attach(ctx, facts.Repository, facts.Digest, file, provenanceArtifactID); err != nil {
		return fmt.Errorf("attach provenance to %s: %v", facts.Digest, err)
	}
	return nil
}
