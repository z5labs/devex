// Package main implements the crypto Dagger module: file digests and key
// generation utilities. All operations run in pure Go inside the module
// runtime — no helper containers or external tools — using `crypto/*` from
// the standard library plus `golang.org/x/crypto/sha3` and
// `golang.org/x/crypto/ssh` for SHA-3 hashing and OpenSSH public-key
// formatting.
package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"

	"dagger/crypto/internal/dagger"

	"golang.org/x/crypto/sha3"
	"golang.org/x/crypto/ssh"
)

// Crypto provides hashing and key-generation utilities for use in pipelines.
// All operations execute in pure Go inside the module runtime — no helper
// containers or external tools.
type Crypto struct{}

// Sha256 returns the SHA-256 hex digest of file.
func (c *Crypto) Sha256(ctx context.Context, file *dagger.File) (string, error) {
	return digestFile(ctx, file, sha256.New())
}

// Sha384 returns the SHA-384 hex digest of file.
func (c *Crypto) Sha384(ctx context.Context, file *dagger.File) (string, error) {
	return digestFile(ctx, file, sha512.New384())
}

// Sha512 returns the SHA-512 hex digest of file.
func (c *Crypto) Sha512(ctx context.Context, file *dagger.File) (string, error) {
	return digestFile(ctx, file, sha512.New())
}

// Sha3_256 returns the SHA3-256 hex digest of file.
func (c *Crypto) Sha3_256(ctx context.Context, file *dagger.File) (string, error) {
	return digestFile(ctx, file, sha3.New256())
}

// Sha3_512 returns the SHA3-512 hex digest of file.
func (c *Crypto) Sha3_512(ctx context.Context, file *dagger.File) (string, error) {
	return digestFile(ctx, file, sha3.New512())
}

func digestFile(ctx context.Context, file *dagger.File, h hash.Hash) (string, error) {
	dir, err := os.MkdirTemp(".", "in-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "f")
	if _, err := file.Export(ctx, path); err != nil {
		return "", err
	}

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// GenerateRsaKey generates a fresh RSA private key of the requested size.
//
// +cache="never"
func (c *Crypto) GenerateRsaKey(
	// +default=4096
	bits int,
) (*RsaKey, error) {
	priv, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return &RsaKey{Pkcs8Der: der}, nil
}

// GenerateEcdsaP256Key generates a fresh ECDSA private key over the P-256 curve.
//
// +cache="never"
func (c *Crypto) GenerateEcdsaP256Key() (*EcdsaKey, error) {
	return generateEcdsa(elliptic.P256())
}

// GenerateEcdsaP384Key generates a fresh ECDSA private key over the P-384 curve.
//
// +cache="never"
func (c *Crypto) GenerateEcdsaP384Key() (*EcdsaKey, error) {
	return generateEcdsa(elliptic.P384())
}

// GenerateEcdsaP521Key generates a fresh ECDSA private key over the P-521 curve.
//
// +cache="never"
func (c *Crypto) GenerateEcdsaP521Key() (*EcdsaKey, error) {
	return generateEcdsa(elliptic.P521())
}

func generateEcdsa(curve elliptic.Curve) (*EcdsaKey, error) {
	priv, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return &EcdsaKey{Pkcs8Der: der}, nil
}

// GenerateEd25519Key generates a fresh Ed25519 private key.
//
// +cache="never"
func (c *Crypto) GenerateEd25519Key() (*Ed25519Key, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Ed25519Key{PrivateKey: priv, PublicKey: pub}, nil
}

// RsaKey wraps a generated RSA keypair. The private key bytes live off the
// GraphQL surface and are materialized to a *dagger.File on demand.
//
// Each format method carries `+cache="never"`: without it, Dagger caches the
// chained query (`generateRsaKey.pem.contents`) by chain shape, so repeat
// invocations would return the first call's File even though the parent
// generator runs fresh. Forcing re-execution at every method node keeps
// every call genuinely fresh.
//
// Stored as PKCS#8 DER because the Dagger code generator only round-trips
// `[]byte` cleanly across runtime invocations — `*rsa.PrivateKey` directly
// would generate broken bindings.
type RsaKey struct {
	//+private
	Pkcs8Der []byte
}

// +cache="never"
func (k *RsaKey) Pem() (*dagger.File, error) {
	return writePkcs8Pem(k.Pkcs8Der)
}

// +cache="never"
func (k *RsaKey) Der() (*dagger.File, error) {
	return writeWorkdirFile("key.der", k.Pkcs8Der)
}

// +cache="never"
func (k *RsaKey) PublicKeyPem() (*dagger.File, error) {
	pub, err := publicFromPkcs8(k.Pkcs8Der)
	if err != nil {
		return nil, err
	}
	return publicPem(pub)
}

// +cache="never"
func (k *RsaKey) PublicKeyDer() (*dagger.File, error) {
	pub, err := publicFromPkcs8(k.Pkcs8Der)
	if err != nil {
		return nil, err
	}
	return publicDer(pub)
}

// +cache="never"
func (k *RsaKey) OpenSshPublicKey() (*dagger.File, error) {
	pub, err := publicFromPkcs8(k.Pkcs8Der)
	if err != nil {
		return nil, err
	}
	return openSshPublicKey(pub)
}

// EcdsaKey wraps a generated ECDSA keypair. See RsaKey for why each method
// carries `+cache="never"`.
type EcdsaKey struct {
	//+private
	Pkcs8Der []byte
}

// +cache="never"
func (k *EcdsaKey) Pem() (*dagger.File, error) {
	return writePkcs8Pem(k.Pkcs8Der)
}

// +cache="never"
func (k *EcdsaKey) Der() (*dagger.File, error) {
	return writeWorkdirFile("key.der", k.Pkcs8Der)
}

// +cache="never"
func (k *EcdsaKey) PublicKeyPem() (*dagger.File, error) {
	pub, err := publicFromPkcs8(k.Pkcs8Der)
	if err != nil {
		return nil, err
	}
	return publicPem(pub)
}

// +cache="never"
func (k *EcdsaKey) PublicKeyDer() (*dagger.File, error) {
	pub, err := publicFromPkcs8(k.Pkcs8Der)
	if err != nil {
		return nil, err
	}
	return publicDer(pub)
}

// +cache="never"
func (k *EcdsaKey) OpenSshPublicKey() (*dagger.File, error) {
	pub, err := publicFromPkcs8(k.Pkcs8Der)
	if err != nil {
		return nil, err
	}
	return openSshPublicKey(pub)
}

// Ed25519Key wraps a generated Ed25519 keypair. See RsaKey for why each
// method carries `+cache="never"`.
type Ed25519Key struct {
	//+private
	PublicKey ed25519.PublicKey
	//+private
	PrivateKey ed25519.PrivateKey
}

// +cache="never"
func (k *Ed25519Key) Pem() (*dagger.File, error) { return privatePem(k.PrivateKey) }

// +cache="never"
func (k *Ed25519Key) Der() (*dagger.File, error) { return privateDer(k.PrivateKey) }

// +cache="never"
func (k *Ed25519Key) PublicKeyPem() (*dagger.File, error) { return publicPem(k.PublicKey) }

// +cache="never"
func (k *Ed25519Key) PublicKeyDer() (*dagger.File, error) { return publicDer(k.PublicKey) }

// +cache="never"
func (k *Ed25519Key) OpenSshPublicKey() (*dagger.File, error) {
	return openSshPublicKey(k.PublicKey)
}

// privatePem encodes priv as PKCS#8 PEM and stages it as a *dagger.File via
// the module runtime's scratch workdir.
func privatePem(priv any) (*dagger.File, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return writePkcs8Pem(der)
}

func privateDer(priv any) (*dagger.File, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return writeWorkdirFile("key.der", der)
}

// writePkcs8Pem wraps PKCS#8 DER bytes in a PEM PRIVATE KEY block.
func writePkcs8Pem(der []byte) (*dagger.File, error) {
	body := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	return writeWorkdirFile("key.pem", body)
}

// publicFromPkcs8 parses a PKCS#8 DER blob and returns the embedded public
// key, suitable for x509.MarshalPKIXPublicKey or ssh.NewPublicKey.
func publicFromPkcs8(der []byte) (any, error) {
	priv, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	pub, ok := priv.(interface{ Public() crypto.PublicKey })
	if !ok {
		return nil, fmt.Errorf("private key type %T has no Public() method", priv)
	}
	return pub.Public(), nil
}

func publicPem(pub any) (*dagger.File, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	body := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	return writeWorkdirFile("pub.pem", body)
}

func publicDer(pub any) (*dagger.File, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return writeWorkdirFile("pub.der", der)
}

func openSshPublicKey(pub any) (*dagger.File, error) {
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return writeWorkdirFile("pub.openssh", ssh.MarshalAuthorizedKey(sshPub))
}

// writeWorkdirFile writes content to a content-addressed subdir of the
// module's scratch workdir and returns it as a *dagger.File. The subdir name
// is derived from a hash of the content, so distinct outputs land at distinct
// WorkdirFile paths (different Dagger File IDs) and identical outputs are
// idempotent.
func writeWorkdirFile(name string, content []byte) (*dagger.File, error) {
	sum := sha256.Sum256(content)
	dir := "out-" + hex.EncodeToString(sum[:])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return nil, err
	}
	return dag.CurrentModule().WorkdirFile(path), nil
}
