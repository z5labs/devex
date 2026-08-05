package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"dagger/z-5-labs/internal/dagger"
)

// The artifact types the attestations are attached under. A consumer
// filters referrers by these, which is what "distinguished by predicate
// type" means at the registry: the SBOM formats are told apart from each
// other and from the provenance without opening any of them.
const (
	spdxArtifactType      = "application/spdx+json"
	cycloneDxArtifactType = "application/vnd.cyclonedx+json"
)

// attachAttestations attaches the SBOMs and the signed provenance to the
// digest that was just published.
//
// The split of responsibility here is the point. This module knows what
// it built and which identity vouched for the run; the `go` module knows
// what is inside a Go binary and produces the SBOMs from the binaries it
// compiled; and the `oci` module attaches bytes to a digest without
// knowing that any of them are an SBOM or an attestation — it is handed
// a file and an artifact type, and that is all it ever learns.
func (a *GoApp) attachAttestations(
	ctx context.Context,
	registry *dagger.OciRegistry,
	sgn *signer,
	facts buildFacts,
	binaries map[string]*dagger.File,
) error {
	// One SBOM pair per platform, not one for the release. A manifest
	// list is several binaries, each with its own bytes and its own
	// checksum, and a single document claiming to describe "the" binary
	// would have to name one of those checksums and be wrong about the
	// rest. The documents are told apart by their title annotation; the
	// artifact type stays the format, because that is what a consumer
	// filters on.
	for _, platform := range facts.Platforms {
		binary, ok := binaries[platform]
		if !ok {
			return fmt.Errorf("no binary recorded for platform %s", platform)
		}
		stem := facts.BinaryName + "-" + strings.ReplaceAll(platform, "/", "-")
		documents := []struct {
			name         string
			artifactType string
			file         *dagger.File
		}{
			{stem + ".spdx.json", spdxArtifactType, dag.Go().Spdx(binary, a.Source)},
			{stem + ".cdx.json", cycloneDxArtifactType, dag.Go().CycloneDx(binary, a.Source)},
		}
		for _, doc := range documents {
			if _, err := registry.Attach(ctx, facts.Repository, facts.Digest, renamed(doc.file, doc.name), doc.artifactType); err != nil {
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
	file, err := writeWorkdirFile(facts.BinaryName+".intoto.jsonl", envelope)
	if err != nil {
		return fmt.Errorf("write provenance envelope: %v", err)
	}
	if _, err := registry.Attach(ctx, facts.Repository, facts.Digest, file, provenanceArtifactID); err != nil {
		return fmt.Errorf("attach provenance to %s: %v", facts.Digest, err)
	}
	return nil
}

// renamed gives file a new name without reading its bytes into this
// module. The attachment's title annotation is taken from the file's
// name, and the `go` module names its documents after the binary — which
// is the same name for every platform, so the names have to be made
// distinct on this side of the boundary rather than by teaching the `go`
// module about platforms it does not build for.
func renamed(file *dagger.File, name string) *dagger.File {
	return dag.Directory().WithFile(name, file).File(name)
}
