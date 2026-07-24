package main

import (
	"context"
	"fmt"

	par "github.com/dagger/dagger/util/parallel"
)

type Tests struct{}

// All runs every random test inside this suite.
//
// parallel caps how many tests run concurrently. Defaults to 0 (unbounded
// fan-out) — each `dagger check` job runs on its own GH Actions runner, so
// in-runner parallelism is bounded by the VM's CPU/memory, not by the
// scheduler. Pass any positive integer to opt into a specific cap.
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

	jobs = jobs.WithJob("UuidV4ShouldNotBeCached", t.UuidV4ShouldNotBeCached)
	jobs = jobs.WithJob("UuidV7ShouldNotBeCached", t.UuidV7ShouldNotBeCached)
	jobs = jobs.WithJob("Sha256ShouldNotBeCached", t.Sha256ShouldNotBeCached)
	jobs = jobs.WithJob("Sha512ShouldNotBeCached", t.Sha512ShouldNotBeCached)
	jobs = jobs.WithJob("SerialShouldNotBeCached", t.SerialShouldNotBeCached)
	jobs = jobs.WithJob("ExamplesCookbook", t.exampleSmoke)

	return jobs.Run(ctx)
}

// exampleSmoke runs every examples/go cookbook recipe end-to-end and asserts
// it returned something, so the suite fails if the examples rot against the
// random API. It is intentionally unexported so it stays out of this module's
// Dagger schema (and the root ci/ bindings); it is driven only as a job in All.
func (t *Tests) exampleSmoke(ctx context.Context) error {
	ex := dag.RandomExamples()

	for name, mint := range map[string]func(context.Context) (string, error){
		"MintSha256Token":       ex.MintSha256Token,
		"MintCertificateSerial": ex.MintCertificateSerial,
		"MintSortableEventId":   ex.MintSortableEventID,
	} {
		v, err := mint(ctx)
		if err != nil {
			return fmt.Errorf("example recipe %s: %w", name, err)
		}
		if v == "" {
			return fmt.Errorf("example recipe %s: returned an empty value", name)
		}
	}

	// ShowNonCachingContract already errors internally when its two values
	// match; assert the pair anyway so the recipe cannot silently stop
	// demonstrating the contract it exists to teach.
	pair, err := ex.ShowNonCachingContract(ctx)
	if err != nil {
		return fmt.Errorf("example recipe ShowNonCachingContract: %w", err)
	}
	if len(pair) != 2 {
		return fmt.Errorf("example recipe ShowNonCachingContract: expected 2 values, got %d", len(pair))
	}
	if pair[0] == pair[1] {
		return fmt.Errorf("example recipe ShowNonCachingContract: expected different values, got the same: %s", pair[0])
	}
	return nil
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
