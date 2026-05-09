# crypto

A Dagger module exposing common crypto utilities — file digests and ephemeral
key generation — implemented in pure Go (no helper containers) on top of
`crypto/*`, `golang.org/x/crypto/sha3`, and `golang.org/x/crypto/ssh`.

## Functions

### Hashing

Each hash function consumes a `*dagger.File` and returns its hex-encoded
digest. Hashing is deterministic, so results are cached by Dagger.

- `Sha256`, `Sha384`, `Sha512`, `Sha3_256`, `Sha3_512`

CLI:

```sh
dagger -m github.com/z5labs/devex/daggerverse/crypto call sha-256 --file=./go.mod
```

Go SDK:

```go
sum, err := dag.Crypto().Sha256(ctx, file)
```

### Key generation

Each `Generate*Key` function bypasses the cache (`+cache="never"`) so every
call yields fresh material. The returned key object exposes five
format-conversion methods that emit `*dagger.File`:

- `Pem()` — PKCS#8 PEM private key
- `Der()` — PKCS#8 DER private key
- `PublicKeyPem()` — SPKI PEM public key
- `PublicKeyDer()` — SPKI DER public key
- `OpenSshPublicKey()` — OpenSSH `authorized_keys`-style public key

Generators:

- `GenerateRsaKey(bits int = 4096) -> RsaKey`
- `GenerateEcdsaP256Key() -> EcdsaKey`
- `GenerateEcdsaP384Key() -> EcdsaKey`
- `GenerateEcdsaP521Key() -> EcdsaKey`
- `GenerateEd25519Key() -> Ed25519Key`

CLI:

```sh
dagger -m github.com/z5labs/devex/daggerverse/crypto call \
  generate-ed-25519-key open-ssh-public-key contents
# ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIK…
```

Go SDK (chained lazily — terminal calls like `Contents`/`Size`/`Sync` take
the `ctx` and return the error):

```go
pubFile := dag.Crypto().GenerateEd25519Key().OpenSSHPublicKey()
line, err := pubFile.Contents(ctx)
```
