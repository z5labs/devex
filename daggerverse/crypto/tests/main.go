// Package main implements the test module for the crypto Dagger module.
package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/tests/internal/dagger"

	"github.com/dagger/dagger/util/parallel"
)

type Tests struct{}

// All runs every crypto test in parallel.
//
// +check
// +cache="session"
func (t *Tests) All(ctx context.Context) error {
	jobs := parallel.New().
		WithRollupLogs(true).
		WithRollupSpans(true)

	jobs = jobs.WithJob("Sha256MatchesKnownDigest", t.Sha256MatchesKnownDigest)
	jobs = jobs.WithJob("Sha384MatchesKnownDigest", t.Sha384MatchesKnownDigest)
	jobs = jobs.WithJob("Sha512MatchesKnownDigest", t.Sha512MatchesKnownDigest)
	jobs = jobs.WithJob("Sha3_256MatchesKnownDigest", t.Sha3_256MatchesKnownDigest)
	jobs = jobs.WithJob("Sha3_512MatchesKnownDigest", t.Sha3_512MatchesKnownDigest)

	jobs = jobs.WithJob("RsaKeyShouldNotBeCached", t.RsaKeyShouldNotBeCached)
	jobs = jobs.WithJob("EcdsaP256KeyShouldNotBeCached", t.EcdsaP256KeyShouldNotBeCached)
	jobs = jobs.WithJob("EcdsaP384KeyShouldNotBeCached", t.EcdsaP384KeyShouldNotBeCached)
	jobs = jobs.WithJob("EcdsaP521KeyShouldNotBeCached", t.EcdsaP521KeyShouldNotBeCached)
	jobs = jobs.WithJob("Ed25519KeyShouldNotBeCached", t.Ed25519KeyShouldNotBeCached)

	jobs = jobs.WithJob("RsaKeyEmitsValidFormats", t.RsaKeyEmitsValidFormats)
	jobs = jobs.WithJob("EcdsaP256KeyEmitsValidFormats", t.EcdsaP256KeyEmitsValidFormats)
	jobs = jobs.WithJob("Ed25519KeyEmitsValidFormats", t.Ed25519KeyEmitsValidFormats)

	return jobs.Run(ctx)
}

// helloFile returns a *dagger.File whose contents are "hello".
func helloFile() *dagger.File {
	return dag.Directory().WithNewFile("in", "hello").File("in")
}

// Known SHA digests of the bytes "hello".
const (
	helloSha256   = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	helloSha384   = "59e1748777448c69de6b800d7a33bbfb9ff1b463e44354c3553bcdb9c666fa90125a3c79f90397bdf5f6a13de828684f"
	helloSha512   = "9b71d224bd62f3785d96d46ad3ea3d73319bfbc2890caadae2dff72519673ca72323c3d99ba5c11d7c7acc6e14b8c5da0c4663475c2e5c3adef46f73bcdec043"
	helloSha3_256 = "3338be694f50c5f338814986cdf0686453a888b84f424d792af4b9202398f392"
	helloSha3_512 = "75d527c368f2efe848ecf6b073a36767800805e9eef2b1857d5f984f036eb6df891d75f72d9b154518c1cd58835286d1da9a38deba3de98b5a53e5ed78a84976"
)

func (t *Tests) Sha256MatchesKnownDigest(ctx context.Context) error {
	got, err := dag.Crypto().Sha256(ctx, helloFile())
	if err != nil {
		return err
	}
	if got != helloSha256 {
		return fmt.Errorf("Sha256(%q): got %s, want %s", "hello", got, helloSha256)
	}
	return nil
}

func (t *Tests) Sha384MatchesKnownDigest(ctx context.Context) error {
	got, err := dag.Crypto().Sha384(ctx, helloFile())
	if err != nil {
		return err
	}
	if got != helloSha384 {
		return fmt.Errorf("Sha384(%q): got %s, want %s", "hello", got, helloSha384)
	}
	return nil
}

func (t *Tests) Sha512MatchesKnownDigest(ctx context.Context) error {
	got, err := dag.Crypto().Sha512(ctx, helloFile())
	if err != nil {
		return err
	}
	if got != helloSha512 {
		return fmt.Errorf("Sha512(%q): got %s, want %s", "hello", got, helloSha512)
	}
	return nil
}

func (t *Tests) Sha3_256MatchesKnownDigest(ctx context.Context) error {
	got, err := dag.Crypto().Sha3256(ctx, helloFile())
	if err != nil {
		return err
	}
	if got != helloSha3_256 {
		return fmt.Errorf("Sha3_256(%q): got %s, want %s", "hello", got, helloSha3_256)
	}
	return nil
}

func (t *Tests) Sha3_512MatchesKnownDigest(ctx context.Context) error {
	got, err := dag.Crypto().Sha3512(ctx, helloFile())
	if err != nil {
		return err
	}
	if got != helloSha3_512 {
		return fmt.Errorf("Sha3_512(%q): got %s, want %s", "hello", got, helloSha3_512)
	}
	return nil
}

// rsaKeyOpts uses 2048-bit keys in tests so they generate quickly.
var rsaKeyOpts = dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}

func (t *Tests) RsaKeyShouldNotBeCached(ctx context.Context) error {
	a, err := dag.Crypto().GenerateRsaKey(rsaKeyOpts).Pem().Contents(ctx)
	if err != nil {
		return err
	}
	b, err := dag.Crypto().GenerateRsaKey(rsaKeyOpts).Pem().Contents(ctx)
	if err != nil {
		return err
	}
	if a == b {
		return fmt.Errorf("expected different RSA keys, got the same")
	}
	return nil
}

func (t *Tests) EcdsaP256KeyShouldNotBeCached(ctx context.Context) error {
	a, err := dag.Crypto().GenerateEcdsaP256Key().Pem().Contents(ctx)
	if err != nil {
		return err
	}
	b, err := dag.Crypto().GenerateEcdsaP256Key().Pem().Contents(ctx)
	if err != nil {
		return err
	}
	if a == b {
		return fmt.Errorf("expected different ECDSA P-256 keys, got the same")
	}
	return nil
}

func (t *Tests) EcdsaP384KeyShouldNotBeCached(ctx context.Context) error {
	a, err := dag.Crypto().GenerateEcdsaP384Key().Pem().Contents(ctx)
	if err != nil {
		return err
	}
	b, err := dag.Crypto().GenerateEcdsaP384Key().Pem().Contents(ctx)
	if err != nil {
		return err
	}
	if a == b {
		return fmt.Errorf("expected different ECDSA P-384 keys, got the same")
	}
	return nil
}

func (t *Tests) EcdsaP521KeyShouldNotBeCached(ctx context.Context) error {
	a, err := dag.Crypto().GenerateEcdsaP521Key().Pem().Contents(ctx)
	if err != nil {
		return err
	}
	b, err := dag.Crypto().GenerateEcdsaP521Key().Pem().Contents(ctx)
	if err != nil {
		return err
	}
	if a == b {
		return fmt.Errorf("expected different ECDSA P-521 keys, got the same")
	}
	return nil
}

func (t *Tests) Ed25519KeyShouldNotBeCached(ctx context.Context) error {
	a, err := dag.Crypto().GenerateEd25519Key().Pem().Contents(ctx)
	if err != nil {
		return err
	}
	b, err := dag.Crypto().GenerateEd25519Key().Pem().Contents(ctx)
	if err != nil {
		return err
	}
	if a == b {
		return fmt.Errorf("expected different Ed25519 keys, got the same")
	}
	return nil
}

// keyFormatChecks returns a function that asserts the five output files of a
// generated key all have the expected shapes. sshPubPrefix is the expected
// algorithm prefix on the OpenSSH line (e.g. "ssh-ed25519").
func keyFormatChecks(
	ctx context.Context,
	pem func() *dagger.File,
	der func() *dagger.File,
	pubPem func() *dagger.File,
	pubDer func() *dagger.File,
	openSshPub func() *dagger.File,
	sshPubPrefix string,
) error {
	privPem, err := pem().Contents(ctx)
	if err != nil {
		return fmt.Errorf("read key.pem: %w", err)
	}
	if !strings.HasPrefix(privPem, "-----BEGIN PRIVATE KEY-----") {
		return fmt.Errorf("key.pem missing PKCS#8 PEM header, got: %q", trim(privPem))
	}

	publicPem, err := pubPem().Contents(ctx)
	if err != nil {
		return fmt.Errorf("read pub.pem: %w", err)
	}
	if !strings.HasPrefix(publicPem, "-----BEGIN PUBLIC KEY-----") {
		return fmt.Errorf("pub.pem missing SPKI PEM header, got: %q", trim(publicPem))
	}

	openSsh, err := openSshPub().Contents(ctx)
	if err != nil {
		return fmt.Errorf("read pub.openssh: %w", err)
	}
	if !strings.HasPrefix(openSsh, sshPubPrefix+" ") {
		return fmt.Errorf("pub.openssh missing %q prefix, got: %q", sshPubPrefix, trim(openSsh))
	}

	privDerSize, err := der().Size(ctx)
	if err != nil {
		return fmt.Errorf("size key.der: %w", err)
	}
	if privDerSize == 0 {
		return fmt.Errorf("key.der is empty")
	}

	pubDerSize, err := pubDer().Size(ctx)
	if err != nil {
		return fmt.Errorf("size pub.der: %w", err)
	}
	if pubDerSize == 0 {
		return fmt.Errorf("pub.der is empty")
	}
	return nil
}

func trim(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

func (t *Tests) RsaKeyEmitsValidFormats(ctx context.Context) error {
	k := dag.Crypto().GenerateRsaKey(rsaKeyOpts)
	return keyFormatChecks(ctx, k.Pem, k.Der, k.PublicKeyPem, k.PublicKeyDer, k.OpenSSHPublicKey, "ssh-rsa")
}

func (t *Tests) EcdsaP256KeyEmitsValidFormats(ctx context.Context) error {
	k := dag.Crypto().GenerateEcdsaP256Key()
	return keyFormatChecks(ctx, k.Pem, k.Der, k.PublicKeyPem, k.PublicKeyDer, k.OpenSSHPublicKey, "ecdsa-sha2-nistp256")
}

func (t *Tests) Ed25519KeyEmitsValidFormats(ctx context.Context) error {
	k := dag.Crypto().GenerateEd25519Key()
	return keyFormatChecks(ctx, k.Pem, k.Der, k.PublicKeyPem, k.PublicKeyDer, k.OpenSSHPublicKey, "ssh-ed25519")
}
