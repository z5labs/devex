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

## Caching and freshness

`CreateCertificateAuthority`, `IssueServerCertificate`, `IssueClientCertificate`,
and `IssueMutualTlsCertificate` generate fresh RSA keys and random serials
each time they execute. They carry `+cache="session"`, so within a single
Dagger engine session the same arguments resolve to the same CA / leaf — this
is what keeps `ca.KeyStore()`, `ca.TrustStore()`, and `ca.IssueXxx()`
consistent across field accesses on a single returned object. Across
sessions, the same arguments yield a fresh CA / leaf.

To force a fresh CA or leaf within a session, vary an input — pass a fresh
password from [`daggerverse/random`](../random)'s `Sha256()`. See
`tests/main.go:newPassword` for the canonical pattern.

## Go SDK naming

Dagger uppercases acronyms in the generated Go bindings: the source method
`IssueMutualTlsCertificate` becomes `IssueMutualTLSCertificate` on the SDK
client, and its options struct is
`CertificateManagementCertificateAuthorityIssueMutualTLSCertificateOpts`.
The CLI form is the kebab-case `issue-mutual-tls-certificate`.

## Functions

### CreateCertificateAuthority

Creates a self-signed root CA.

```sh
dagger -m github.com/z5labs/devex/daggerverse/certificate-management call \
  create-certificate-authority \
  --password=env:CA_PWD \
  --common-name="My Root CA" \
  --validity-days=3650
```

```go
ca := dag.CertificateManagement().
    CreateCertificateAuthority(pwd,
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

Issue leaf certificates with `serverAuth`, `clientAuth`, or both EKUs.

```go
issued := ca.IssueServerCertificate("svc.example.com", leafPwd,
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

- RSA-3072 keys only.
- Single-tier CAs (no intermediate CAs).
- No CRL or OCSP issuance.
