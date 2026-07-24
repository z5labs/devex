// Package main is the random-examples Dagger module: a runnable cookbook of
// random recipes. Each recipe shows one realistic use for a fresh value --
// a per-run test secret, a certificate serial, a sortable event id -- and,
// because random exists precisely to defeat the function cache, every recipe
// re-runs on every invocation instead of replaying a cached result.
package main

import (
	"context"
	"fmt"

	"dagger/random-examples/internal/dagger"
)

// RandomExamples is the module's main object: a namespace for the random
// usage recipes.
type RandomExamples struct{}

// MintSha256Token returns a 64-character hex token derived from 32 fresh
// random bytes, the shape you want for a per-run test password or API token.
// Feed it to dag.SetSecret to hand it to a service without it ever landing in
// source.
//
// +cache="never"
func (m *RandomExamples) MintSha256Token(ctx context.Context) (string, error) {
	return dag.Random().Sha256(ctx)
}

// MintCertificateSerial returns a random 64-bit (8-byte, 16 hex character)
// X.509 serial number. Serial forces the low bit, so the value is always a
// positive integer a CA can issue against.
//
// +cache="never"
func (m *RandomExamples) MintCertificateSerial(ctx context.Context) (string, error) {
	return dag.Random().Serial(ctx, dagger.RandomSerialOpts{N: 8})
}

// MintSortableEventId returns a UUID version 7. Unlike a v4, a v7 leads with a
// millisecond timestamp, so ids minted over time sort lexicographically in the
// order they were created -- handy as a primary key or an event id.
//
// +cache="never"
func (m *RandomExamples) MintSortableEventId(ctx context.Context) (string, error) {
	return dag.Random().UUIDV7(ctx)
}

// ShowNonCachingContract calls the same generator twice in one invocation and
// returns both values, which always differ. This is the module's whole point:
// random's functions carry +cache="never", so the engine re-executes them
// instead of replaying the first result for the identical second call.
//
// +cache="never"
func (m *RandomExamples) ShowNonCachingContract(ctx context.Context) ([]string, error) {
	first, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, err
	}

	second, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, err
	}

	// A cached second call would make this fire; it is here so the recipe
	// fails loudly rather than quietly teaching the opposite lesson.
	if first == second {
		return nil, fmt.Errorf("expected two distinct values, got %s twice", first)
	}

	return []string{first, second}, nil
}
