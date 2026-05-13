package main

import (
	"context"
	"fmt"

	par "github.com/dagger/dagger/util/parallel"
)

type Tests struct{}

// All runs every random test inside this suite.
//
// parallel caps how many tests run concurrently. Defaults to 1 (sequential)
// to mirror `go test` package-level semantics; pass 0 to fan out every test
// with no limit, or any positive integer to opt into a specific level of
// concurrency.
//
// +check
// +cache="session"
func (t *Tests) All(
	ctx context.Context,
	// +default=1
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}

	jobs = jobs.WithJob("UuidV4ShouldNotBeCached", t.UuidV4ShouldNotBeCached)
	jobs = jobs.WithJob("UuidV7ShouldNotBeCached", t.UuidV7ShouldNotBeCached)
	jobs = jobs.WithJob("Sha256ShouldNotBeCached", t.Sha256ShouldNotBeCached)
	jobs = jobs.WithJob("Sha512ShouldNotBeCached", t.Sha512ShouldNotBeCached)
	jobs = jobs.WithJob("SerialShouldNotBeCached", t.SerialShouldNotBeCached)

	return jobs.Run(ctx)
}

func (t *Tests) UuidV4ShouldNotBeCached(ctx context.Context) error {
	s1, err := dag.Random().UUIDV4(ctx)
	if err != nil {
		return err
	}

	s2, err := dag.Random().UUIDV4(ctx)
	if err != nil {
		return err
	}

	if s1 == s2 {
		return fmt.Errorf("expected different UUIDs, got the same: %s", s1)
	}
	return nil
}

func (t *Tests) UuidV7ShouldNotBeCached(ctx context.Context) error {
	s1, err := dag.Random().UUIDV7(ctx)
	if err != nil {
		return err
	}

	s2, err := dag.Random().UUIDV7(ctx)
	if err != nil {
		return err
	}

	if s1 == s2 {
		return fmt.Errorf("expected different UUIDs, got the same: %s", s1)
	}
	return nil
}

func (t *Tests) Sha256ShouldNotBeCached(ctx context.Context) error {
	s1, err := dag.Random().Sha256(ctx)
	if err != nil {
		return err
	}

	s2, err := dag.Random().Sha256(ctx)
	if err != nil {
		return err
	}

	if s1 == s2 {
		return fmt.Errorf("expected different SHA256 hashes, got the same: %s", s1)
	}
	return nil
}

func (t *Tests) Sha512ShouldNotBeCached(ctx context.Context) error {
	s1, err := dag.Random().Sha512(ctx)
	if err != nil {
		return err
	}

	s2, err := dag.Random().Sha512(ctx)
	if err != nil {
		return err
	}

	if s1 == s2 {
		return fmt.Errorf("expected different SHA512 hashes, got the same: %s", s1)
	}
	return nil
}

func (t *Tests) SerialShouldNotBeCached(ctx context.Context) error {
	s1, err := dag.Random().Serial(ctx)
	if err != nil {
		return err
	}

	s2, err := dag.Random().Serial(ctx)
	if err != nil {
		return err
	}

	if s1 == s2 {
		return fmt.Errorf("expected different serials, got the same: %s", s1)
	}
	return nil
}
