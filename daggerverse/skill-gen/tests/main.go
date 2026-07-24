// Tests for the skill-gen daggerverse module. Each test is exposed as a
// standalone dagger function so it can be invoked individually during TDD;
// All wires them up for parallel execution under `dagger call all`.
//
// Every password, cluster name, and database name is minted at runtime via
// dag.Random().Sha256 — no secret literals enter git. The schema DDL (table and
// column names) is fixed test input, not a secret, so it stays inline below.
package main

import (
	"context"

	par "github.com/dagger/dagger/util/parallel"

	"dagger/tests/internal/dagger"
)

type Tests struct{}

// All runs every skill-gen test for local `dagger call all`.
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
	jobs = jobs.WithJob("rejects-invalid-db-name", t.RejectsInvalidDbName)
	jobs = jobs.WithJob("rejects-invalid-psql-image", t.RejectsInvalidPsqlImage)
	jobs = jobs.WithJob("bakes-custom-psql-image", t.BakesCustomPsqlImage)
	jobs = jobs.WithJob("defaults-psql-image", t.DefaultsPsqlImage)
	jobs = jobs.WithJob("introspection-failure-aborts", t.IntrospectionFailureAborts)
	jobs = jobs.WithJob("generates-pg-skill-from-cluster", t.GeneratesPgSkillFromCluster)
	jobs = jobs.WithJob("postgres-should-not-be-cached", t.PostgresShouldNotBeCached)
	jobs = jobs.WithJob("regen-changeset-empty-when-unchanged", t.RegenChangesetEmptyWhenUnchanged)
	jobs = jobs.WithJob("regen-changeset-reflects-schema-drift", t.RegenChangesetReflectsSchemaDrift)
	jobs = jobs.WithJob("generates-pg-skill-over-tls", t.GeneratesPgSkillOverTls)
	jobs = jobs.WithJob("generates-pg-skill-over-mtls", t.GeneratesPgSkillOverMtls)
	jobs = jobs.WithJob("plaintext-params-against-tls-abort", t.PlaintextParamsAgainstTlsAbort)
	return jobs.Run(ctx)
}

// randHex returns a fresh 12-hex-char value via the random module.
func randHex(ctx context.Context) (string, error) {
	h, err := dag.Random().Sha256(ctx, dagger.RandomSha256Opts{N: 16})
	if err != nil {
		return "", err
	}
	return h[:12], nil
}

// randSecret mints a random password wrapped in a uniquely-named *dagger.Secret.
func randSecret(ctx context.Context) (*dagger.Secret, error) {
	full, err := dag.Random().Sha256(ctx, dagger.RandomSha256Opts{N: 32})
	if err != nil {
		return nil, err
	}
	return dag.SetSecret("pg-pw-"+full[:12], full), nil
}
