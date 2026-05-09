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

The module is a pure signer: callers supply the private key as a PEM-encoded
PKCS#8 `*dagger.Secret`. `CreateCertificateAuthority` and the three `Issue*`
methods do not generate random keys themselves, so they carry no `+cache=`
directive and Dagger's default content-addressed caching applies. To force a
fresh CA or leaf, vary an input — pass a fresh password or a fresh key.

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
  --password=env:CA_PWD \
  --key=env:CA_KEY_PEM \
  --common-name="My Root CA" \
  --validity-days=3650
```

```go
ca := dag.CertificateManagement().
    CreateCertificateAuthority(pwd, key,
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
takes the leaf's private key as input.

```go
issued := ca.IssueServerCertificate("svc.example.com", leafPwd, leafKey,
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
