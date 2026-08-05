package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The in-toto and SLSA identifiers this module emits. They are the
// published spec URIs rather than anything z5labs made up, because the
// value of an attestation is that a verifier nobody here wrote already
// knows what it means.
const (
	inTotoStatementType  = "https://in-toto.io/Statement/v1"
	slsaProvenanceType   = "https://slsa.dev/provenance/v1"
	goAppBuildType       = "https://z5labs.github.io/devex/goapp/buildtype/v1"
	provenanceArtifactID = "application/vnd.in-toto+json"
)

// buildFacts is everything the pipeline knows about the build it just
// performed, as distinct from what the identity provider says about the
// run. Both go into the predicate and they are kept apart on purpose: a
// verifier trusts the second and reads the first.
type buildFacts struct {
	// Repository is the image repository published to.
	Repository string
	// Tags are the tags the same bytes were published under.
	Tags []string
	// Digest is the manifest digest the attestation is about.
	Digest string
	// Platforms are the platforms built.
	Platforms []string
	// Pkg is the Go package built.
	Pkg string
	// BinaryName is the artifact's name inside the image.
	BinaryName string
	// SourceURI is the origin remote, empty when the tree has no origin.
	SourceURI string
	// Commit is the full HEAD SHA the build read.
	Commit string
	// Version is the version stamped into the binary.
	Version string
}

// provenanceStatement renders the in-toto statement for one published
// digest.
//
// The subject is the digest, not a tag: a tag is a mutable name and an
// attestation about a mutable name attests to nothing. The tags are
// recorded inside the predicate as an observation about the publish, so
// the information survives without being what the statement is about.
func provenanceStatement(identity *workloadIdentity, facts buildFacts, at time.Time) ([]byte, error) {
	algorithm, encoded, ok := strings.Cut(facts.Digest, ":")
	if !ok || encoded == "" {
		return nil, fmt.Errorf("published digest %q is not <algorithm>:<hex>", facts.Digest)
	}

	statement := map[string]any{
		"_type": inTotoStatementType,
		"subject": []map[string]any{{
			"name":   facts.Repository,
			"digest": map[string]string{algorithm: encoded},
		}},
		"predicateType": slsaProvenanceType,
		"predicate": map[string]any{
			"buildDefinition": map[string]any{
				"buildType":            goAppBuildType,
				"externalParameters":   externalParameters(identity, facts),
				"internalParameters":   internalParameters(facts),
				"resolvedDependencies": resolvedDependencies(identity, facts),
			},
			"runDetails": map[string]any{
				"builder": map[string]any{
					"id": identity.BuilderID(),
				},
				"metadata": map[string]any{
					"invocationId": identity.RunID,
					"finishedOn":   at.UTC().Format(time.RFC3339),
				},
			},
		},
	}
	out, err := json.MarshalIndent(statement, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode provenance statement: %v", err)
	}
	return append(out, '\n'), nil
}

// externalParameters are the inputs a rebuilder would need, and every
// identifying one of them is taken from the token rather than from a
// parameter. A repository, a workflow ref or a commit accepted as an
// argument would make the predicate a record of what the caller asked to
// have written down, which is not provenance.
func externalParameters(identity *workloadIdentity, facts buildFacts) map[string]any {
	out := map[string]any{
		"workflow": map[string]any{
			"repository": identity.Repository,
			"ref":        identity.WorkflowRef,
		},
		"image": map[string]any{
			"repository": facts.Repository,
			"tags":       facts.Tags,
		},
	}
	if identity.Commit != "" {
		out["source"] = map[string]any{
			"digest": map[string]string{"gitCommit": identity.Commit},
		}
	}
	return out
}

// internalParameters are the builder's own choices: things the pipeline
// decided, which a verifier may want to see but must not confuse with
// anything the identity provider vouched for.
func internalParameters(facts buildFacts) map[string]any {
	return map[string]any{
		"pkg":        facts.Pkg,
		"binaryName": facts.BinaryName,
		"platforms":  facts.Platforms,
		"version":    facts.Version,
	}
}

// resolvedDependencies records the source tree the build actually read.
// The URI is the origin remote where there is one; the digest is the
// commit the module resolved from the working tree, which is the one
// fact here the builder observed rather than was told.
func resolvedDependencies(identity *workloadIdentity, facts buildFacts) []map[string]any {
	if facts.Commit == "" {
		return nil
	}
	dep := map[string]any{
		"digest": map[string]string{"gitCommit": facts.Commit},
	}
	if facts.SourceURI != "" {
		dep["uri"] = "git+" + facts.SourceURI
	}
	if identity.Commit != "" && identity.Commit != facts.Commit {
		// Reached only when the provider names a commit and the tree
		// holds a different one. It is recorded rather than silently
		// reconciled, because the two disagreeing is the interesting
		// case: the run was minted for one revision and built another.
		dep["annotations"] = map[string]any{
			"z5labs.devex/identityCommit": identity.Commit,
		}
	}
	return []map[string]any{dep}
}
