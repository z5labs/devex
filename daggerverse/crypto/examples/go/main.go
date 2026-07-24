// Package main is the crypto-examples Dagger module: a runnable cookbook of
// crypto recipes. It covers both halves of the module's surface -- digesting a
// file you already have, and generating a keypair you don't -- and each recipe
// is written the way a downstream consumer would call it, passing only files
// and primitives across the module boundary.
package main

import (
	"context"

	"dagger/crypto-examples/internal/dagger"
)

// CryptoExamples is the module's main object: a namespace for the crypto usage
// recipes.
type CryptoExamples struct{}

// sampleText is the stand-in payload every hashing recipe falls back to when
// the caller supplies no file, so `dagger call hash-source-file` prints a
// digest with no arguments at all.
const sampleText = "the quick brown fox jumps over the lazy dog\n"

// sourceOrSample returns file, or a small built-in sample file when the caller
// passed nothing.
func sourceOrSample(file *dagger.File) *dagger.File {
	if file != nil {
		return file
	}
	return dag.Directory().WithNewFile("sample.txt", sampleText).File("sample.txt")
}

// HashSourceFile returns the SHA-256 hex digest of a caller-supplied file --
// the everyday recipe for fingerprinting a build artifact, a lockfile, or a
// downloaded release before you trust it.
//
// Hashing is pure, so this function is deliberately left on Dagger's default
// caching: the same bytes always digest to the same string, and a repeat call
// should replay rather than re-read the file.
func (m *CryptoExamples) HashSourceFile(
	ctx context.Context,
	// The file to digest. Defaults to a small built-in sample so the recipe
	// runs with no arguments.
	//
	// +optional
	file *dagger.File,
) (string, error) {
	return dag.Crypto().Sha256(ctx, sourceOrSample(file))
}

// HashWithSha3 returns the SHA3-512 hex digest of the same file
// HashSourceFile digests. Run both to see that SHA-2 and SHA-3 are different
// algorithms, not different sizes of one: SHA3-512 is a Keccak sponge, so its
// digest shares no bytes with the SHA-256 one even though the input is
// identical. Reach for it when a policy mandates the SHA-3 family.
func (m *CryptoExamples) HashWithSha3(
	ctx context.Context,
	// The file to digest. Defaults to a small built-in sample so the recipe
	// runs with no arguments.
	//
	// +optional
	file *dagger.File,
) (string, error) {
	return dag.Crypto().Sha3512(ctx, sourceOrSample(file))
}

// GenerateRsaKeypair returns a directory holding a freshly generated RSA
// keypair: the PKCS#8 private key as `key.pem` and its PKIX public half as
// `key.pub.pem`. Export it to see both files at once:
//
//	dagger call generate-rsa-keypair export --path ./keys
//
// The two halves are matched because the recipe resolves the generated key to
// a single instance (via its ID) before deriving files from it. Selecting
// .pem() and .publicKeyPem() straight off the generator would build two
// independent queries, and because key generation carries `+cache="never"`
// each would run its own generator and hand back halves of two different keys.
//
// +cache="never"
func (m *CryptoExamples) GenerateRsaKeypair(
	ctx context.Context,
	// Modulus size in bits. Matches crypto's own default; drop to 2048 when
	// you want the recipe to finish quickly.
	//
	// +default=4096
	bits int,
) (*dagger.Directory, error) {
	id, err := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: bits}).ID(ctx)
	if err != nil {
		return nil, err
	}
	key := dag.LoadCryptoRsaKeyFromID(dagger.CryptoRsaKeyID(id))

	return dag.Directory().
		WithFile("key.pem", key.Pem()).
		WithFile("key.pub.pem", key.PublicKeyPem()), nil
}

// GenerateEd25519SshKey returns a directory holding a fresh Ed25519 SSH
// identity under the names ssh expects: the PKCS#8 private key as
// `id_ed25519` and the OpenSSH-formatted public key as `id_ed25519.pub`, the
// single line you paste into an `authorized_keys` file or a Git host.
//
//	dagger call generate-ed25519-ssh-key export --path ~/.ssh
//
// This is the SSH flow in miniature: crypto emits the private key as PEM and
// the public key in OpenSSH wire format from the same generated key, so no
// ssh-keygen container is needed anywhere in the pipeline. As in
// GenerateRsaKeypair, the key is resolved to one instance before both files
// are derived from it.
//
// +cache="never"
func (m *CryptoExamples) GenerateEd25519SshKey(ctx context.Context) (*dagger.Directory, error) {
	id, err := dag.Crypto().GenerateEd25519Key().ID(ctx)
	if err != nil {
		return nil, err
	}
	key := dag.LoadCryptoEd25519KeyFromID(dagger.CryptoEd25519KeyID(id))

	return dag.Directory().
		WithFile("id_ed25519", key.Pem()).
		WithFile("id_ed25519.pub", key.OpenSSHPublicKey()), nil
}
