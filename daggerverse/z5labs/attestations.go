package main

import (
	"context"
	"fmt"
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
const (
	spdxArtifactType      = "application/spdx+json"
	cycloneDxArtifactType = "application/vnd.cyclonedx+json"
)

// attachAttestations attaches this app's documents and its signed
// provenance to the digest that was just published.
//
// The split of responsibility here is the point. The language chain knew
// what it compiled and produced the documents; this knows which identity
// vouched for the run; and the `oci` module attaches bytes to a digest
// without knowing that any of them are an SBOM or an attestation — it is
// handed a file and an artifact type, and that is all it ever learns. The
// loop below is the same: it reads a name, a type and a file off each
// variant and never asks what any of them describes.
//
// One document pair per platform, not one per release. A manifest list is
// several binaries, each with its own bytes and its own checksum, and a
// single document claiming to describe "the" binary would have to name one
// of those checksums and be wrong about the rest. The documents are told
// apart by their title annotation; the artifact type stays the format,
// because that is what a consumer filters on.
func (a *App) attachAttestations(
	ctx context.Context,
	registry *dagger.OciRegistry,
	sgn *signer,
	facts buildFacts,
) error {
	for _, v := range a.Variants {
		for _, doc := range v.Documents {
			if _, err := registry.Attach(ctx, facts.Repository, facts.Digest, doc.File, doc.Type); err != nil {
				return fmt.Errorf("attach %s to %s: %v", doc.Name, facts.Digest, err)
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
