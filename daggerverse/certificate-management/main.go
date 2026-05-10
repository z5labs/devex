// Package main is the certificate-management Dagger module.
package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"dagger/certificate-management/internal/dagger"

	"software.sslmate.com/src/go-pkcs12"
)

// CertificateManagement provides functions for creating and managing X.509
// certificate authorities, issuing server / client / mutual-TLS certificates,
// and packaging them as PKCS#12 keystores and truststores. The module is a
// pure signer: callers supply the private key material as a PEM-encoded
// PKCS#8 *dagger.Secret (RSA, ECDSA, or Ed25519). Pair with
// `daggerverse/crypto`'s key generators for fresh per-call keys.
type CertificateManagement struct{}

// CertificateAuthority is a self-signed X.509 root capable of issuing leaf
// certificates. It carries its own PKCS#12 password used by KeyStore() and
// TrustStore().
type CertificateAuthority struct {
	CertPemFile   *dagger.File   // PEM-encoded CA certificate (public).
	PrivateKeyPem *dagger.Secret // PEM-encoded PKCS#8 CA private key.
	Pwd           *dagger.Secret // PKCS#12 password bound at creation/load time.
}

// IssuedCertificate is a leaf certificate signed by a CA, together with the
// issuing CA's certificate (used to build trust bundles).
type IssuedCertificate struct {
	CertPemFile    *dagger.File   // PEM-encoded leaf certificate.
	PrivateKeyPem  *dagger.Secret // PEM-encoded PKCS#8 leaf private key.
	IssuerCertFile *dagger.File   // PEM-encoded issuing CA certificate.
	Pwd            *dagger.Secret // PKCS#12 password.
}

// KeyStore is a PKCS#12 archive containing a certificate and its private key,
// protected by a password.
type KeyStore struct {
	File *dagger.File
	Pwd  *dagger.Secret
}

// Pkcs12 returns the PKCS#12-encoded archive as a Dagger file.
func (k *KeyStore) Pkcs12() *dagger.File { return k.File }

// Password returns the secret used to encrypt the PKCS#12 archive.
func (k *KeyStore) Password() *dagger.Secret { return k.Pwd }

// TrustStore is a PKCS#12 archive containing one or more trusted certificates,
// protected by a password.
type TrustStore struct {
	File *dagger.File
	Pwd  *dagger.Secret
}

// Pkcs12 returns the PKCS#12-encoded archive as a Dagger file.
func (t *TrustStore) Pkcs12() *dagger.File { return t.File }

// Password returns the secret used to encrypt the PKCS#12 archive.
func (t *TrustStore) Password() *dagger.Secret { return t.Pwd }

// CreateCertificateAuthority self-signs a root CA over the caller-supplied
// private key. The key must be PEM-encoded PKCS#8 (RSA, ECDSA, or Ed25519).
// The supplied password is bound to the resulting CA's KeyStore() and
// TrustStore() output.
//
// Every field of the certificate template is fully determined by the
// function's inputs (commonName, validityDays, notBefore, serial, password,
// key). Vary notBefore and serial per call to bust Dagger's default cache
// when fresh certs are wanted; reuse them to hit the cache and re-use the
// previously signed bytes.
func (m *CertificateManagement) CreateCertificateAuthority(
	ctx context.Context,
	// Subject common name for the CA certificate.
	// +default="Devex Root CA"
	commonName string,
	// Number of days the CA certificate is valid for.
	// +default=3650
	validityDays int,
	// RFC3339 timestamp the CA becomes valid at. The CA's NotAfter is
	// notBefore + validityDays. Pass time.Now().UTC().Format(time.RFC3339)
	// for a fresh CA per call.
	notBefore string,
	// Hex-encoded certificate serial number (typically 32 hex chars =
	// 128 bits). Must be a positive integer.
	serial string,
	// PKCS#12 password used by the CA's KeyStore and TrustStore.
	password *dagger.Secret,
	// PEM-encoded PKCS#8 private key the CA will sign with and embed.
	key *dagger.Secret,
) (*CertificateAuthority, error) {
	nb, err := parseNotBefore(notBefore)
	if err != nil {
		return nil, err
	}
	sn, err := parseSerial(serial)
	if err != nil {
		return nil, err
	}
	signer, err := readKeySecret(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("read CA key: %w", err)
	}

	tmpl, err := buildCATemplate(commonName, validityDays, nb, sn)
	if err != nil {
		return nil, err
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, signer.Public(), signer)
	if err != nil {
		return nil, fmt.Errorf("self-sign CA: %w", err)
	}

	certFile, err := bytesToFile("ca.crt", pemEncodeCert(der))
	if err != nil {
		return nil, err
	}

	return &CertificateAuthority{
		CertPemFile:   certFile,
		PrivateKeyPem: key,
		Pwd:           password,
	}, nil
}

// LoadCertificateAuthority restores a CA from a PKCS#12 archive that contains
// the CA certificate and its private key. The supplied password is also bound
// to the returned CA's KeyStore() and TrustStore() output.
func (m *CertificateManagement) LoadCertificateAuthority(
	ctx context.Context,
	// PKCS#12 archive containing the CA certificate and private key.
	pkcs12File *dagger.File,
	// Password used to decrypt the archive.
	password *dagger.Secret,
) (*CertificateAuthority, error) {
	data, err := fileBytes(ctx, pkcs12File)
	if err != nil {
		return nil, err
	}
	pwd, err := password.Plaintext(ctx)
	if err != nil {
		return nil, fmt.Errorf("read password: %w", err)
	}

	priv, cert, _, err := pkcs12.DecodeChain(data, pwd)
	if err != nil {
		return nil, fmt.Errorf("decode PKCS#12: %w", err)
	}
	if _, ok := priv.(crypto.Signer); !ok {
		return nil, fmt.Errorf("loaded private key %T does not implement crypto.Signer", priv)
	}
	if !cert.IsCA || cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, fmt.Errorf("loaded certificate is not a CA (IsCA=%v, keyUsage=%v)",
			cert.IsCA, cert.KeyUsage)
	}

	keyPem, err := pemEncodeKey(priv)
	if err != nil {
		return nil, err
	}
	certFile, err := bytesToFile("ca.crt", pemEncodeCert(cert.Raw))
	if err != nil {
		return nil, err
	}
	keySecret, err := bytesToSecret(ctx, "cm-ca-key-loaded", keyPem)
	if err != nil {
		return nil, err
	}

	return &CertificateAuthority{
		CertPemFile:   certFile,
		PrivateKeyPem: keySecret,
		Pwd:           password,
	}, nil
}

// LoadKeyStoreFromPkcs12 wraps an existing PKCS#12 archive and its password
// as a KeyStore.
func (m *CertificateManagement) LoadKeyStoreFromPkcs12(
	pkcs12File *dagger.File,
	password *dagger.Secret,
) *KeyStore {
	return &KeyStore{File: pkcs12File, Pwd: password}
}

// LoadTrustStoreFromPkcs12 wraps an existing PKCS#12 archive and its password
// as a TrustStore.
func (m *CertificateManagement) LoadTrustStoreFromPkcs12(
	pkcs12File *dagger.File,
	password *dagger.Secret,
) *TrustStore {
	return &TrustStore{File: pkcs12File, Pwd: password}
}

// KeyStore returns a PKCS#12 archive containing the CA certificate and its
// private key, encrypted with the password bound at creation time.
func (ca *CertificateAuthority) KeyStore(ctx context.Context) (*KeyStore, error) {
	cert, key, pwd, err := ca.materialize(ctx)
	if err != nil {
		return nil, err
	}
	data, err := pkcs12.Modern.Encode(key, cert, nil, pwd)
	if err != nil {
		return nil, fmt.Errorf("encode CA keystore: %w", err)
	}
	file, err := bytesToFile("ca-keystore.p12", data)
	if err != nil {
		return nil, err
	}
	return &KeyStore{File: file, Pwd: ca.Pwd}, nil
}

// TrustStore returns a PKCS#12 archive containing the CA certificate, suitable
// for distribution to clients that need to trust certificates issued by this
// CA.
func (ca *CertificateAuthority) TrustStore(ctx context.Context) (*TrustStore, error) {
	cert, _, pwd, err := ca.materialize(ctx)
	if err != nil {
		return nil, err
	}
	data, err := pkcs12.Modern.EncodeTrustStore([]*x509.Certificate{cert}, pwd)
	if err != nil {
		return nil, fmt.Errorf("encode CA truststore: %w", err)
	}
	file, err := bytesToFile("ca-truststore.p12", data)
	if err != nil {
		return nil, err
	}
	return &TrustStore{File: file, Pwd: ca.Pwd}, nil
}

// IssueServerCertificate signs a leaf TLS server certificate from the
// caller-supplied private key (PEM PKCS#8) using this CA. The leaf is
// embedded with the given DNS and IP Subject Alternative Names.
//
// Pure given its inputs; vary notBefore and serial per call to bust caching.
func (ca *CertificateAuthority) IssueServerCertificate(
	ctx context.Context,
	// Subject common name for the server certificate.
	commonName string,
	// DNS names to embed as Subject Alternative Names.
	// +optional
	dnsSans []string,
	// IP addresses to embed as Subject Alternative Names.
	// +optional
	ipSans []string,
	// Number of days the certificate is valid for.
	// +default=365
	validityDays int,
	// RFC3339 timestamp the certificate becomes valid at.
	notBefore string,
	// Hex-encoded certificate serial number (typically 32 hex chars =
	// 128 bits). Must be a positive integer.
	serial string,
	// PKCS#12 password used by the issued certificate's KeyStore and
	// TrustStore.
	password *dagger.Secret,
	// PEM-encoded PKCS#8 private key for the leaf certificate.
	key *dagger.Secret,
) (*IssuedCertificate, error) {
	return ca.issueLeaf(ctx, commonName, dnsSans, ipSans, validityDays,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, notBefore, serial, password, key)
}

// IssueClientCertificate signs a leaf TLS client certificate from the
// caller-supplied private key (PEM PKCS#8) using this CA.
//
// Pure given its inputs; vary notBefore and serial per call to bust caching.
func (ca *CertificateAuthority) IssueClientCertificate(
	ctx context.Context,
	commonName string,
	// +default=365
	validityDays int,
	// RFC3339 timestamp the certificate becomes valid at.
	notBefore string,
	// Hex-encoded certificate serial number (typically 32 hex chars =
	// 128 bits). Must be a positive integer.
	serial string,
	password *dagger.Secret,
	// PEM-encoded PKCS#8 private key for the leaf certificate.
	key *dagger.Secret,
) (*IssuedCertificate, error) {
	return ca.issueLeaf(ctx, commonName, nil, nil, validityDays,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, notBefore, serial, password, key)
}

// IssueMutualTlsCertificate signs a leaf certificate that is valid for both
// server and client authentication, suitable for mutual-TLS use, from the
// caller-supplied private key (PEM PKCS#8).
//
// Pure given its inputs; vary notBefore and serial per call to bust caching.
func (ca *CertificateAuthority) IssueMutualTlsCertificate(
	ctx context.Context,
	commonName string,
	// +optional
	dnsSans []string,
	// +optional
	ipSans []string,
	// +default=365
	validityDays int,
	// RFC3339 timestamp the certificate becomes valid at.
	notBefore string,
	// Hex-encoded certificate serial number (typically 32 hex chars =
	// 128 bits). Must be a positive integer.
	serial string,
	password *dagger.Secret,
	// PEM-encoded PKCS#8 private key for the leaf certificate.
	key *dagger.Secret,
) (*IssuedCertificate, error) {
	return ca.issueLeaf(ctx, commonName, dnsSans, ipSans, validityDays,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		notBefore, serial, password, key)
}

// KeyStore returns a PKCS#12 archive containing the leaf certificate, its
// private key, and the issuing CA certificate as a chain entry.
func (ic *IssuedCertificate) KeyStore(ctx context.Context) (*KeyStore, error) {
	leafCert, leafKey, caCert, pwd, err := ic.materialize(ctx)
	if err != nil {
		return nil, err
	}
	data, err := pkcs12.Modern.Encode(leafKey, leafCert,
		[]*x509.Certificate{caCert}, pwd)
	if err != nil {
		return nil, fmt.Errorf("encode keystore: %w", err)
	}
	file, err := bytesToFile("keystore.p12", data)
	if err != nil {
		return nil, err
	}
	return &KeyStore{File: file, Pwd: ic.Pwd}, nil
}

// TrustStore returns a PKCS#12 archive containing the issuing CA certificate.
func (ic *IssuedCertificate) TrustStore(ctx context.Context) (*TrustStore, error) {
	_, _, caCert, pwd, err := ic.materialize(ctx)
	if err != nil {
		return nil, err
	}
	data, err := pkcs12.Modern.EncodeTrustStore(
		[]*x509.Certificate{caCert}, pwd)
	if err != nil {
		return nil, fmt.Errorf("encode truststore: %w", err)
	}
	file, err := bytesToFile("truststore.p12", data)
	if err != nil {
		return nil, err
	}
	return &TrustStore{File: file, Pwd: ic.Pwd}, nil
}

func (ca *CertificateAuthority) issueLeaf(
	ctx context.Context,
	commonName string,
	dnsSans []string,
	ipSans []string,
	validityDays int,
	eku []x509.ExtKeyUsage,
	notBefore string,
	serial string,
	password *dagger.Secret,
	leafKeySecret *dagger.Secret,
) (*IssuedCertificate, error) {
	nb, err := parseNotBefore(notBefore)
	if err != nil {
		return nil, err
	}
	sn, err := parseSerial(serial)
	if err != nil {
		return nil, err
	}
	caCert, caKey, _, err := ca.materialize(ctx)
	if err != nil {
		return nil, err
	}

	leafSigner, err := readKeySecret(ctx, leafKeySecret)
	if err != nil {
		return nil, fmt.Errorf("read leaf key: %w", err)
	}
	tmpl, err := buildLeafTemplate(commonName, dnsSans, ipSans, eku, validityDays, nb, sn, leafSigner.Public())
	if err != nil {
		return nil, err
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert,
		leafSigner.Public(), caKey)
	if err != nil {
		return nil, fmt.Errorf("sign leaf certificate: %w", err)
	}

	certFile, err := bytesToFile("leaf.crt", pemEncodeCert(der))
	if err != nil {
		return nil, err
	}

	return &IssuedCertificate{
		CertPemFile:    certFile,
		PrivateKeyPem:  leafKeySecret,
		IssuerCertFile: ca.CertPemFile,
		Pwd:            password,
	}, nil
}

func (ca *CertificateAuthority) materialize(ctx context.Context) (*x509.Certificate, crypto.Signer, string, error) {
	cert, err := readCertFile(ctx, ca.CertPemFile)
	if err != nil {
		return nil, nil, "", fmt.Errorf("read CA cert: %w", err)
	}
	key, err := readKeySecret(ctx, ca.PrivateKeyPem)
	if err != nil {
		return nil, nil, "", fmt.Errorf("read CA key: %w", err)
	}
	pwd, err := ca.Pwd.Plaintext(ctx)
	if err != nil {
		return nil, nil, "", fmt.Errorf("read CA password: %w", err)
	}
	return cert, key, pwd, nil
}

func (ic *IssuedCertificate) materialize(ctx context.Context) (*x509.Certificate, crypto.Signer, *x509.Certificate, string, error) {
	leafCert, err := readCertFile(ctx, ic.CertPemFile)
	if err != nil {
		return nil, nil, nil, "", fmt.Errorf("read leaf cert: %w", err)
	}
	leafKey, err := readKeySecret(ctx, ic.PrivateKeyPem)
	if err != nil {
		return nil, nil, nil, "", fmt.Errorf("read leaf key: %w", err)
	}
	caCert, err := readCertFile(ctx, ic.IssuerCertFile)
	if err != nil {
		return nil, nil, nil, "", fmt.Errorf("read issuer cert: %w", err)
	}
	pwd, err := ic.Pwd.Plaintext(ctx)
	if err != nil {
		return nil, nil, nil, "", fmt.Errorf("read password: %w", err)
	}
	return leafCert, leafKey, caCert, pwd, nil
}

func buildCATemplate(commonName string, validityDays int, notBefore time.Time, serial *big.Int) (*x509.Certificate, error) {
	if validityDays <= 0 {
		return nil, fmt.Errorf("validityDays must be positive, got %d", validityDays)
	}
	return &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             notBefore,
		NotAfter:              notBefore.AddDate(0, 0, validityDays),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}, nil
}

func buildLeafTemplate(commonName string, dnsSans []string, ipSans []string, eku []x509.ExtKeyUsage, validityDays int, notBefore time.Time, serial *big.Int, pub crypto.PublicKey) (*x509.Certificate, error) {
	if validityDays <= 0 {
		return nil, fmt.Errorf("validityDays must be positive, got %d", validityDays)
	}
	ips := make([]net.IP, 0, len(ipSans))
	for _, raw := range ipSans {
		ip := net.ParseIP(raw)
		if ip == nil {
			return nil, fmt.Errorf("invalid IP SAN %q", raw)
		}
		ips = append(ips, ip)
	}
	// KeyEncipherment is only meaningful for RSA key transport; ECDSA and
	// Ed25519 leaves get DigitalSignature alone.
	keyUsage := x509.KeyUsageDigitalSignature
	if _, isRSA := pub.(*rsa.PublicKey); isRSA {
		keyUsage |= x509.KeyUsageKeyEncipherment
	}
	return &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             notBefore,
		NotAfter:              notBefore.AddDate(0, 0, validityDays),
		KeyUsage:              keyUsage,
		ExtKeyUsage:           eku,
		BasicConstraintsValid: true,
		DNSNames:              dnsSans,
		IPAddresses:           ips,
	}, nil
}

func parseNotBefore(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse notBefore %q: %w", s, err)
	}
	return t.UTC(), nil
}

func parseSerial(s string) (*big.Int, error) {
	if s == "" {
		return nil, fmt.Errorf("serial must not be empty")
	}
	n, ok := new(big.Int).SetString(s, 16)
	if !ok {
		return nil, fmt.Errorf("parse serial %q: not valid hex", s)
	}
	if n.Sign() <= 0 {
		return nil, fmt.Errorf("serial must be a positive integer, got %s", s)
	}
	return n, nil
}

func pemEncodeCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func pemEncodeKey(k crypto.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func parsePemCert(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	return cert, nil
}

func parsePemKey(data []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	priv, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKCS#8 key: %w", err)
	}
	signer, ok := priv.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("private key type %T does not implement crypto.Signer", priv)
	}
	return signer, nil
}

func readCertFile(ctx context.Context, f *dagger.File) (*x509.Certificate, error) {
	b, err := fileBytes(ctx, f)
	if err != nil {
		return nil, err
	}
	return parsePemCert(b)
}

func readKeySecret(ctx context.Context, s *dagger.Secret) (crypto.Signer, error) {
	pt, err := s.Plaintext(ctx)
	if err != nil {
		return nil, err
	}
	return parsePemKey([]byte(pt))
}

// fileBytes materializes the content of a Dagger file via Export to the
// module runtime's scratch directory and reads it back as raw bytes. This
// path is binary-safe; reading File.Contents() through GraphQL would corrupt
// non-UTF-8 byte sequences (relevant for PKCS#12 archives).
func fileBytes(ctx context.Context, f *dagger.File) ([]byte, error) {
	suffix, err := uniqueSuffix()
	if err != nil {
		return nil, err
	}
	local := "cm-in-" + suffix
	if _, err := f.Export(ctx, local); err != nil {
		return nil, fmt.Errorf("export file: %w", err)
	}
	defer os.Remove(local)
	return os.ReadFile(local)
}

// bytesToFile writes raw bytes to the module's scratch working directory and
// returns a *dagger.File pointing at the resulting file. Each call uses a
// fresh subdirectory so concurrent invocations do not collide. Permissions
// are restrictive (0700 dir / 0600 file) because callers may pass PKCS#12
// archives containing private keys.
func bytesToFile(name string, data []byte) (*dagger.File, error) {
	suffix, err := uniqueSuffix()
	if err != nil {
		return nil, err
	}
	dir := "cm-out-" + suffix
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create scratch dir: %w", err)
	}
	rel := filepath.Join(dir, name)
	if err := os.WriteFile(rel, data, 0o600); err != nil {
		return nil, fmt.Errorf("write %s: %w", rel, err)
	}
	return dag.CurrentModule().WorkdirFile(rel), nil
}

func bytesToSecret(ctx context.Context, prefix string, data []byte) (*dagger.Secret, error) {
	suffix, err := uniqueSuffix()
	if err != nil {
		return nil, err
	}
	return dag.SetSecret(prefix+"-"+suffix, string(data)), nil
}

func uniqueSuffix() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate secret name suffix: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
