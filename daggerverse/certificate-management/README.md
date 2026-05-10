# certificate-management

A Dagger module for managing X.509 certificate authorities and issuing TLS
certificates packaged as PKCS#12 keystores and truststores.

The module is pure-Go (no `openssl` shell-out) and uses
[`software.sslmate.com/src/go-pkcs12`](https://pkg.go.dev/software.sslmate.com/src/go-pkcs12)
for PKCS#12 encoding.

## Concepts

- **`CertificateManagement`** — module entry point.
- **`CertificateAuthority`** — a self-signed root CA.
- **`IssuedCertificate`** — a leaf certificate signed by a CA.
- **`KeyStore`** — PKCS#12 archive holding a certificate and its private key.
- **`TrustStore`** — PKCS#12 archive holding one or more trusted certificates.

`KeyStore` and `TrustStore` both expose `Pkcs12()` (the `*dagger.File`) and
`Password()` (the `*dagger.Secret` they were sealed with).

## Determinism

The module is a pure signer: every field of the certificate template is
fully determined by the inputs. `CreateCertificateAuthority` and the three
`Issue*` methods take the private key, the password, the validity window's
`notBefore` (RFC3339), and the certificate `serial` (hex) — there is no
`time.Now()` or `rand.Int` hidden inside the template. As a result the
functions carry no `+cache=` directive and Dagger's default content-addressed
caching works as intended: identical inputs hit the cache and return the
previously signed bytes; varying `notBefore` or `serial` (or any other input)
is the natural cache-busting mechanism.

(The signature *bytes* themselves are not necessarily reproducible across
fresh signings for ECDSA keys — `crypto/ecdsa` uses a random nonce — but
this is invisible to callers because Dagger replays the cached bytes from
the first signing call.)

In practice you almost always want fresh certs per call. Pass
`time.Now().UTC().Format(time.RFC3339)` for `notBefore` and a fresh random
hex string for `serial`; both will differ each run and Dagger will re-sign
accordingly.

## Generating keys

Pair this module with [`daggerverse/crypto`](../crypto), which exposes
`GenerateRsaKey`, `GenerateEcdsaP256Key` / `P384` / `P521`, and
`GenerateEd25519Key`. Each returns an object whose `.Pem()` is a `*dagger.File`
holding a PKCS#8 PEM blob; bridge it into a `*dagger.Secret` once:

```go
pem, _ := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).
    Pem().Contents(ctx)
key := dag.SetSecret("ca-key", pem)
```

Any algorithm whose private key implements `crypto.Signer` (RSA, ECDSA,
Ed25519) is accepted.

## Minting `notBefore` and `serial`

```go
import (
    "crypto/rand"
    "encoding/hex"
    "time"
)

notBefore := time.Now().UTC().Format(time.RFC3339)
var serialBytes [16]byte
_, _ = rand.Read(serialBytes[:])
serial := hex.EncodeToString(serialBytes[:]) // 32 hex chars = 128 bits
```

## Go SDK naming

Dagger uppercases acronyms in the generated Go bindings: the source method
`IssueMutualTlsCertificate` becomes `IssueMutualTLSCertificate` on the SDK
client, and its options struct is
`CertificateManagementCertificateAuthorityIssueMutualTLSCertificateOpts`.
The CLI form is the kebab-case `issue-mutual-tls-certificate`.

## Functions

### CreateCertificateAuthority

Self-signs a root CA over the caller-supplied private key.

```sh
dagger -m github.com/z5labs/devex/daggerverse/certificate-management call \
  create-certificate-authority \
  --not-before="2026-05-09T00:00:00Z" \
  --serial="0123456789abcdef0123456789abcdef" \
  --password=env:CA_PWD \
  --key=env:CA_KEY_PEM \
  --common-name="My Root CA" \
  --validity-days=3650
```

```go
ca := dag.CertificateManagement().
    CreateCertificateAuthority(notBefore, serial, pwd, key,
        dagger.CertificateManagementCreateCertificateAuthorityOpts{
            CommonName:   "My Root CA",
            ValidityDays: 3650,
        })
```

### LoadCertificateAuthority

Restores a CA from a PKCS#12 archive containing the CA cert and key.

```go
ca := dag.CertificateManagement().LoadCertificateAuthority(p12File, pwd)
```

### IssueServerCertificate / IssueClientCertificate / IssueMutualTlsCertificate

Sign leaf certificates with `serverAuth`, `clientAuth`, or both EKUs. Each
takes the leaf's private key, `notBefore`, and `serial` as inputs. Leaves
get `KeyUsageDigitalSignature`; RSA leaves additionally get
`KeyUsageKeyEncipherment` (omitted for ECDSA / Ed25519, where it is
semantically meaningless).

```go
issued := ca.IssueServerCertificate("svc.example.com", notBefore, serial, leafPwd, leafKey,
    dagger.CertificateManagementCertificateAuthorityIssueServerCertificateOpts{
        DNSSans: []string{"svc.example.com"},
        IPSans:  []string{"10.0.0.1"},
    })
```

### KeyStore / TrustStore

Both `CertificateAuthority` and `IssuedCertificate` expose `KeyStore()` and
`TrustStore()`. The `KeyStore` of an `IssuedCertificate` includes the issuing
CA as a chain entry.

```go
ksFile := issued.KeyStore().Pkcs12()
ksPwd  := issued.KeyStore().Password()
tsFile := issued.TrustStore().Pkcs12()
tsPwd  := issued.TrustStore().Password()
```

### LoadKeyStoreFromPkcs12 / LoadTrustStoreFromPkcs12

Wrap an existing PKCS#12 archive (and its password) as a `KeyStore` /
`TrustStore` for downstream consumers.

```go
ks := dag.CertificateManagement().LoadKeyStoreFromPkcs12(p12File, pwd)
ts := dag.CertificateManagement().LoadTrustStoreFromPkcs12(p12File, pwd)
```

## Limitations

- Single-tier CAs (no intermediate CAs).
- No CRL or OCSP issuance.
