// Package main implements the test module for the crypto Dagger module.
package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"strings"

	"dagger/tests/internal/dagger"

	par "github.com/dagger/dagger/util/parallel"
)

type Tests struct{}

// All runs every crypto test inside this suite.
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

	jobs = jobs.WithJob("ExamplesCookbook", t.exampleSmoke)

	return jobs.Run(ctx)
}

// exampleSmoke runs every examples/go cookbook recipe end-to-end, so the suite
// fails if the examples rot against the crypto API. It is intentionally
// unexported so it stays out of this module's Dagger schema (and the root ci/
// bindings); it is driven only as a job in All.
func (t *Tests) exampleSmoke(ctx context.Context) error {
	ex := dag.CryptoExamples()

	got, err := ex.HashSourceFile(ctx, dagger.CryptoExamplesHashSourceFileOpts{File: helloFile()})
	if err != nil {
		return fmt.Errorf("example recipe HashSourceFile: %w", err)
	}
	if got != helloSha256 {
		return fmt.Errorf("example recipe HashSourceFile: got %s, want %s", got, helloSha256)
	}

	got, err = ex.HashWithSha3(ctx, dagger.CryptoExamplesHashWithSha3Opts{File: helloFile()})
	if err != nil {
		return fmt.Errorf("example recipe HashWithSha3: %w", err)
	}
	if got != helloSha3_512 {
		return fmt.Errorf("example recipe HashWithSha3: got %s, want %s", got, helloSha3_512)
	}

	if err := exampleRsaKeypair(ctx, ex); err != nil {
		return err
	}
	return exampleEd25519SshKey(ctx, ex)
}

// exampleRsaKeypair asserts GenerateRsaKeypair emits both halves of one
// keypair. 2048 bits keeps the recipe quick; the recipe's own default is 4096.
func exampleRsaKeypair(ctx context.Context, ex *dagger.CryptoExamples) error {
	dir, err := pin(ctx, ex.GenerateRsaKeypair(dagger.CryptoExamplesGenerateRsaKeypairOpts{Bits: 2048}))
	if err != nil {
		return fmt.Errorf("example recipe GenerateRsaKeypair: %w", err)
	}

	privPem, err := dir.File("key.pem").Contents(ctx)
	if err != nil {
		return fmt.Errorf("example recipe GenerateRsaKeypair: read key.pem: %w", err)
	}
	if !strings.HasPrefix(privPem, "-----BEGIN PRIVATE KEY-----") {
		// Don't echo any portion of private-key material into CI logs.
		return fmt.Errorf("example recipe GenerateRsaKeypair: key.pem missing PKCS#8 PEM header (%d bytes)", len(privPem))
	}

	pubPem, err := dir.File("key.pub.pem").Contents(ctx)
	if err != nil {
		return fmt.Errorf("example recipe GenerateRsaKeypair: read key.pub.pem: %w", err)
	}
	if !strings.HasPrefix(pubPem, "-----BEGIN PUBLIC KEY-----") {
		return fmt.Errorf("example recipe GenerateRsaKeypair: key.pub.pem missing SPKI PEM header, got: %q", trim(pubPem))
	}

	// The halves must belong to the *same* key. Deriving them from two
	// independent selections off a `+cache="never"` generator would silently
	// run the generator twice and pair a private key with a stranger's public
	// key, so assert the recipe pins one instance before fanning out.
	priv, err := parsePrivatePem(privPem)
	if err != nil {
		return fmt.Errorf("example recipe GenerateRsaKeypair: parse key.pem: %w", err)
	}
	wantPub, err := marshalPublicPem(priv)
	if err != nil {
		return fmt.Errorf("example recipe GenerateRsaKeypair: re-encode public key: %w", err)
	}
	if pubPem != wantPub {
		return fmt.Errorf("example recipe GenerateRsaKeypair: key.pub.pem is not the public half of key.pem")
	}
	return nil
}

// exampleEd25519SshKey asserts GenerateEd25519SshKey emits an ssh-shaped
// identity whose public line really belongs to the private key beside it.
func exampleEd25519SshKey(ctx context.Context, ex *dagger.CryptoExamples) error {
	dir, err := pin(ctx, ex.GenerateEd25519SshKey())
	if err != nil {
		return fmt.Errorf("example recipe GenerateEd25519SshKey: %w", err)
	}

	privPem, err := dir.File("id_ed25519").Contents(ctx)
	if err != nil {
		return fmt.Errorf("example recipe GenerateEd25519SshKey: read id_ed25519: %w", err)
	}
	if !strings.HasPrefix(privPem, "-----BEGIN PRIVATE KEY-----") {
		return fmt.Errorf("example recipe GenerateEd25519SshKey: id_ed25519 missing PKCS#8 PEM header (%d bytes)", len(privPem))
	}

	sshPub, err := dir.File("id_ed25519.pub").Contents(ctx)
	if err != nil {
		return fmt.Errorf("example recipe GenerateEd25519SshKey: read id_ed25519.pub: %w", err)
	}
	if !strings.HasPrefix(sshPub, "ssh-ed25519 ") {
		return fmt.Errorf("example recipe GenerateEd25519SshKey: id_ed25519.pub missing %q prefix, got: %q", "ssh-ed25519", trim(sshPub))
	}

	priv, err := parsePrivatePem(privPem)
	if err != nil {
		return fmt.Errorf("example recipe GenerateEd25519SshKey: parse id_ed25519: %w", err)
	}
	edPriv, ok := priv.(ed25519.PrivateKey)
	if !ok {
		return fmt.Errorf("example recipe GenerateEd25519SshKey: id_ed25519 holds a %T, want an Ed25519 key", priv)
	}

	// An OpenSSH public key line is `<algo> <base64 blob>[ comment]`, and the
	// blob's last field is the raw 32-byte Ed25519 public key — so a suffix
	// match is an exact check that both files came from one generated key.
	fields := strings.Fields(sshPub)
	if len(fields) < 2 {
		return fmt.Errorf("example recipe GenerateEd25519SshKey: id_ed25519.pub is not an authorized_keys line, got: %q", trim(sshPub))
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return fmt.Errorf("example recipe GenerateEd25519SshKey: decode id_ed25519.pub blob: %w", err)
	}
	if !bytes.HasSuffix(blob, edPriv.Public().(ed25519.PublicKey)) {
		return fmt.Errorf("example recipe GenerateEd25519SshKey: id_ed25519.pub is not the public half of id_ed25519")
	}
	return nil
}

// pin resolves dir to a concrete Directory ID and reloads it, so every later
// read selects off one snapshot. Reading two files straight off the returned
// handle would instead build two independent queries, and both key recipes
// carry `+cache="never"` — so each read would re-run the recipe and the test
// would compare files from two different keypairs.
func pin(ctx context.Context, dir *dagger.Directory) (*dagger.Directory, error) {
	id, err := dir.ID(ctx)
	if err != nil {
		return nil, err
	}
	return dag.LoadDirectoryFromID(dagger.DirectoryID(id)), nil
}

// parsePrivatePem decodes a PKCS#8 PEM private key.
func parsePrivatePem(s string) (any, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	return x509.ParsePKCS8PrivateKey(block.Bytes)
}

// marshalPublicPem renders priv's public half as an SPKI PEM block, in the
// same encoding crypto's PublicKeyPem emits.
func marshalPublicPem(priv any) (string, error) {
	pub, ok := priv.(interface{ Public() crypto.PublicKey })
	if !ok {
		return "", fmt.Errorf("private key type %T has no Public() method", priv)
	}
	der, err := x509.MarshalPKIXPublicKey(pub.Public())
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
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
		// Don't echo any portion of private-key material into CI logs.
		return fmt.Errorf("key.pem missing PKCS#8 PEM header (%d bytes)", len(privPem))
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
