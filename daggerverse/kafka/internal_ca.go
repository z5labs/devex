package main

import (
	"bytes"
	"context"
	"encoding/pem"
	"fmt"
	"time"

	"dagger/kafka/internal/dagger"

	"software.sslmate.com/src/go-pkcs12"
)

// nodePkis bundles a single node's mTLS material for the internal listeners.
type nodePkis struct {
	KeyStoreFile       *dagger.File
	KeyStorePassword   *dagger.Secret
	TrustStoreFile     *dagger.File
	TrustStorePassword *dagger.Secret
}

// internalMaterial holds per-cluster CA-derived mTLS material, one entry per
// node (controller + every broker). Truststore is shared across nodes.
type internalMaterial struct {
	Controller nodePkis
	Brokers    []nodePkis
}

// externalLeaf bundles a per-broker external-listener leaf certificate
// (signed by the caller-supplied CA) and the password sealing its PKCS#12
// keystore.
type externalLeaf struct {
	KeyStoreFile     *dagger.File
	KeyStorePassword *dagger.Secret
}

// serviceLeaf bundles a single service leaf certificate (broker external
// listener or Schema Registry REST endpoint) in both representations so
// PKCS#12-consuming images (Confluent, Kafka brokers, Apicurio REST) and
// PEM-consuming images (Karapace) can share one minting path. The PKCS#12
// keystore is eagerly staged content-addressed; the PEM cert/key stay lazy
// (resolved only when a PEM caller mounts them) so the broker path pays no
// extra I/O.
type serviceLeaf struct {
	KeyStoreFile     *dagger.File   // PKCS#12 leaf keystore (cert + private key)
	KeyStorePassword *dagger.Secret // password sealing the PKCS#12 keystore
	CertPemFile      *dagger.File   // leaf cert PEM (lazy)
	PrivateKeyPem    *dagger.Secret // leaf private key PEM (lazy)
}

const (
	internalKeystorePath   = "/etc/kafka/secrets/internal-keystore.p12"
	internalTruststorePath = "/etc/kafka/secrets/internal-truststore.p12"
	externalKeystorePath   = "/etc/kafka/secrets/external-keystore.p12"
	externalTruststorePath = "/etc/kafka/secrets/external-truststore.p12"
)

// applyInternalListenerSsl mounts the internal mTLS keystore + truststore at
// fixed paths and configures per-listener Kafka SSL env vars so the named
// listener uses them with required client auth. Mounts are idempotent across
// repeat calls with the same node material — Dagger collapses duplicate
// WithFile invocations of identical content.
func applyInternalListenerSsl(ctr *dagger.Container, listenerName string, m nodePkis) *dagger.Container {
	prefix := "KAFKA_LISTENER_NAME_" + listenerName + "_SSL_"
	return ctr.
		WithFile(internalKeystorePath, m.KeyStoreFile, dagger.ContainerWithFileOpts{Permissions: 0o644}).
		WithFile(internalTruststorePath, m.TrustStoreFile, dagger.ContainerWithFileOpts{Permissions: 0o644}).
		WithEnvVariable(prefix+"KEYSTORE_LOCATION", internalKeystorePath).
		WithSecretVariable(prefix+"KEYSTORE_PASSWORD", m.KeyStorePassword).
		WithEnvVariable(prefix+"KEYSTORE_TYPE", "PKCS12").
		WithEnvVariable(prefix+"TRUSTSTORE_LOCATION", internalTruststorePath).
		WithSecretVariable(prefix+"TRUSTSTORE_PASSWORD", m.TrustStorePassword).
		WithEnvVariable(prefix+"TRUSTSTORE_TYPE", "PKCS12").
		WithEnvVariable(prefix+"CLIENT_AUTH", "required")
}

// applyExternalListenerSsl mounts the per-broker external-listener leaf
// keystore (and, for mTLS, the caller-supplied client truststore) and
// configures Kafka's per-listener SSL env vars for the EXTERNAL listener.
// TLS-only mode sets client.auth=none; MTLS sets it to required and points
// at the truststore.
func applyExternalListenerSsl(ctr *dagger.Container, leaf externalLeaf, sec *ServerSecurity) *dagger.Container {
	const prefix = "KAFKA_LISTENER_NAME_EXTERNAL_SSL_"
	ctr = ctr.
		WithFile(externalKeystorePath, leaf.KeyStoreFile, dagger.ContainerWithFileOpts{Permissions: 0o644}).
		WithEnvVariable(prefix+"KEYSTORE_LOCATION", externalKeystorePath).
		WithSecretVariable(prefix+"KEYSTORE_PASSWORD", leaf.KeyStorePassword).
		WithEnvVariable(prefix+"KEYSTORE_TYPE", "PKCS12")
	if sec.Mode == "MTLS" {
		ctr = ctr.
			WithFile(externalTruststorePath, sec.ClientTrustStore, dagger.ContainerWithFileOpts{Permissions: 0o644}).
			WithEnvVariable(prefix+"TRUSTSTORE_LOCATION", externalTruststorePath).
			WithSecretVariable(prefix+"TRUSTSTORE_PASSWORD", sec.ClientTrustStorePassword).
			WithEnvVariable(prefix+"TRUSTSTORE_TYPE", "PKCS12").
			WithEnvVariable(prefix+"CLIENT_AUTH", "required")
	} else {
		ctr = ctr.WithEnvVariable(prefix+"CLIENT_AUTH", "none")
	}
	return ctr
}

// mintInternalCA mints a fresh per-cluster CA and a leaf certificate for
// every node. Each leaf carries both serverAuth and clientAuth EKUs so the
// node can both accept peer connections and originate connections to peers
// or to the controller, all under the same internal trust domain. The CA's
// truststore is shared across nodes; it never crosses the module boundary.
//
// All PKCS#12 archives (truststore + per-node keystores) are eagerly
// materialized to byte arrays via Export and then re-staged as fresh files
// in the kafka module's workdir. This guarantees that two distinct
// references that should hold the same CA bytes are byte-identical even
// when consumed concurrently — there is no possibility of a downstream
// container build pulling the lazy `ca` chain a second time and getting a
// re-derived (different) CA.
func mintInternalCA(
	ctx context.Context,
	controllerHost string,
	brokerHosts []string,
) (*internalMaterial, error) {
	caKeyPem, err := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 4096}).Pem().Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate internal CA key: %w", err)
	}
	caKey := dag.SetSecret("kafka-internal-ca-key-"+randSuffix(), caKeyPem)

	caPwdHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate internal CA password: %w", err)
	}
	caPwd := dag.SetSecret("kafka-internal-ca-pwd-"+randSuffix(), caPwdHex)

	caSerial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate internal CA serial: %w", err)
	}
	nb := time.Now().UTC().Format(time.RFC3339)

	ca := dag.CertificateManagement().CreateCertificateAuthority(nb, caSerial, caPwd, caKey,
		dagger.CertificateManagementCreateCertificateAuthorityOpts{
			CommonName:   "Kafka Internal CA",
			ValidityDays: 3650,
		})

	tsBytes, err := dagFileBytes(ctx, ca.TrustStore().Pkcs12())
	if err != nil {
		return nil, fmt.Errorf("materialize internal CA truststore: %w", err)
	}
	tsFile, err := writeWorkdirBytes("internal-truststore", "ca-truststore.p12", tsBytes)
	if err != nil {
		return nil, fmt.Errorf("stage internal CA truststore: %w", err)
	}
	// caPwd doubles as the truststore password (both KeyStore + TrustStore in
	// the certificate-management module are sealed with the CA's bound Pwd).
	tsPwd := caPwd

	mintNode := func(hostname string) (nodePkis, error) {
		leafKeyPem, err := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).Pem().Contents(ctx)
		if err != nil {
			return nodePkis{}, fmt.Errorf("generate %q leaf key: %w", hostname, err)
		}
		leafKey := dag.SetSecret("kafka-internal-leaf-key-"+randSuffix(), leafKeyPem)
		leafPwdHex, err := dag.Random().Sha256(ctx)
		if err != nil {
			return nodePkis{}, fmt.Errorf("generate %q leaf password: %w", hostname, err)
		}
		leafPwdName := "kafka-internal-leaf-pwd-" + randSuffix()
		leafPwd := dag.SetSecret(leafPwdName, leafPwdHex)
		leafSerial, err := dag.Random().Serial(ctx)
		if err != nil {
			return nodePkis{}, fmt.Errorf("generate %q leaf serial: %w", hostname, err)
		}
		issued := ca.IssueMutualTLSCertificate(hostname, nb, leafSerial, leafPwd, leafKey,
			dagger.CertificateManagementCertificateAuthorityIssueMutualTLSCertificateOpts{
				DNSSans:      []string{hostname, "localhost"},
				IPSans:       []string{"127.0.0.1"},
				ValidityDays: 365,
			})
		ksBytes, err := dagFileBytes(ctx, issued.KeyStore().Pkcs12())
		if err != nil {
			return nodePkis{}, fmt.Errorf("materialize %q keystore: %w", hostname, err)
		}
		ksFile, err := writeWorkdirBytes("internal-keystore-"+hostname, "node-keystore.p12", ksBytes)
		if err != nil {
			return nodePkis{}, fmt.Errorf("stage %q keystore: %w", hostname, err)
		}
		return nodePkis{
			KeyStoreFile:       ksFile,
			KeyStorePassword:   leafPwd,
			TrustStoreFile:     tsFile,
			TrustStorePassword: tsPwd,
		}, nil
	}

	ctrl, err := mintNode(controllerHost)
	if err != nil {
		return nil, err
	}
	brks := make([]nodePkis, len(brokerHosts))
	for i, h := range brokerHosts {
		brks[i], err = mintNode(h)
		if err != nil {
			return nil, err
		}
	}
	return &internalMaterial{Controller: ctrl, Brokers: brks}, nil
}

// mintExternalLeaves loads the caller-supplied CA and signs one leaf
// certificate per broker. Each leaf carries the broker's stable hostname
// (e.g. "broker-100") as a DNS SAN so franz-go clients dialing the
// bootstrap address verify the SSL endpoint identity successfully.
func mintExternalLeaves(
	ctx context.Context,
	sec *ServerSecurity,
	brokerHosts []string,
) ([]externalLeaf, error) {
	ca := dag.CertificateManagement().LoadCertificateAuthority(sec.CaKeyStore, sec.CaKeyStorePassword)
	leaves := make([]externalLeaf, len(brokerHosts))
	for i, h := range brokerHosts {
		leaf, err := mintServiceLeafFromCA(ctx, ca, h, "external")
		if err != nil {
			return nil, err
		}
		leaves[i] = externalLeaf{
			KeyStoreFile:     leaf.KeyStoreFile,
			KeyStorePassword: leaf.KeyStorePassword,
		}
	}
	return leaves, nil
}

// leafInputs generates the fresh key / keystore-password / serial / notBefore
// material for one leaf certificate. The private key crosses as a sealed
// Dagger secret; the keystore password is a random SHA-256 hex.
func leafInputs(ctx context.Context, host string) (key *dagger.Secret, pwd *dagger.Secret, serial string, nb string, err error) {
	keyPem, err := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).Pem().Contents(ctx)
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("generate %q leaf key: %w", host, err)
	}
	key = dag.SetSecret("kafka-leaf-key-"+randSuffix(), keyPem)

	pwdHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("generate %q leaf password: %w", host, err)
	}
	pwd = dag.SetSecret("kafka-leaf-pwd-"+randSuffix(), pwdHex)

	serial, err = dag.Random().Serial(ctx)
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("generate %q leaf serial: %w", host, err)
	}
	nb = time.Now().UTC().Format(time.RFC3339)
	return key, pwd, serial, nb, nil
}

// stageLeaf materializes the PKCS#12 keystore of an issued leaf content-addressed
// (byte-stable across concurrent consumers) and returns it plus the lazy PEM
// cert/key. label + host disambiguate the staged file's scratch subdir.
func stageLeaf(ctx context.Context, issued *dagger.CertificateManagementIssuedCertificate, pwd *dagger.Secret, host, label string) (serviceLeaf, error) {
	ksBytes, err := dagFileBytes(ctx, issued.KeyStore().Pkcs12())
	if err != nil {
		return serviceLeaf{}, fmt.Errorf("materialize %q keystore: %w", host, err)
	}
	ksFile, err := writeWorkdirBytes(label+"-keystore-"+host, "leaf.p12", ksBytes)
	if err != nil {
		return serviceLeaf{}, fmt.Errorf("stage %q keystore: %w", host, err)
	}
	return serviceLeaf{
		KeyStoreFile:     ksFile,
		KeyStorePassword: pwd,
		CertPemFile:      issued.CertPemFile(),
		PrivateKeyPem:    issued.PrivateKeyPem(),
	}, nil
}

// mintServiceLeafFromCA signs one server leaf (serverAuth EKU) whose hostname
// is bound as a DNS SAN — the shared core of broker external-listener and
// Schema Registry REST-endpoint leaf minting.
func mintServiceLeafFromCA(ctx context.Context, ca *dagger.CertificateManagementCertificateAuthority, host, label string) (serviceLeaf, error) {
	key, pwd, serial, nb, err := leafInputs(ctx, host)
	if err != nil {
		return serviceLeaf{}, err
	}
	issued := ca.IssueServerCertificate(host, nb, serial, pwd, key,
		dagger.CertificateManagementCertificateAuthorityIssueServerCertificateOpts{
			DNSSans:      []string{host, "localhost"},
			IPSans:       []string{"127.0.0.1"},
			ValidityDays: 365,
		})
	return stageLeaf(ctx, issued, pwd, host, label)
}

// mintServiceLeaf loads a caller CA and signs one server leaf for host. Used
// by the Schema Registry constructors for the per-registry REST server cert.
func mintServiceLeaf(ctx context.Context, caKeyStore *dagger.File, caKeyStorePassword *dagger.Secret, host, label string) (serviceLeaf, error) {
	ca := dag.CertificateManagement().LoadCertificateAuthority(caKeyStore, caKeyStorePassword)
	return mintServiceLeafFromCA(ctx, ca, host, label)
}

// mintClientLeaf loads a caller CA and signs one client leaf (clientAuth EKU)
// with commonName cn. Used for a Schema Registry's own mTLS client cert when
// its kafka-storage connection authenticates to an mTLS broker.
func mintClientLeaf(ctx context.Context, caKeyStore *dagger.File, caKeyStorePassword *dagger.Secret, cn, label string) (serviceLeaf, error) {
	ca := dag.CertificateManagement().LoadCertificateAuthority(caKeyStore, caKeyStorePassword)
	key, pwd, serial, nb, err := leafInputs(ctx, cn)
	if err != nil {
		return serviceLeaf{}, err
	}
	issued := ca.IssueClientCertificate(cn, nb, serial, pwd, key)
	return stageLeaf(ctx, issued, pwd, cn, label)
}

// caTrustStorePkcs12 loads a caller CA and re-exports its trust store as a
// content-addressed PKCS#12 archive plus the sealing password — the
// broker-facing truststore a Schema Registry uses to verify the cluster's
// TLS/mTLS broker over its kafka-storage connection.
func caTrustStorePkcs12(ctx context.Context, caKeyStore *dagger.File, caKeyStorePassword *dagger.Secret) (*dagger.File, *dagger.Secret, error) {
	ts := dag.CertificateManagement().LoadCertificateAuthority(caKeyStore, caKeyStorePassword).TrustStore()
	tsBytes, err := dagFileBytes(ctx, ts.Pkcs12())
	if err != nil {
		return nil, nil, fmt.Errorf("materialize CA truststore: %w", err)
	}
	tsFile, err := writeWorkdirBytes("sr-kafkastore-truststore", "truststore.p12", tsBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("stage CA truststore: %w", err)
	}
	return tsFile, ts.Password(), nil
}

// caCertPem loads a caller CA and returns its cert as a content-addressed PEM
// file — the broker-facing truststore for PEM-consuming registries (Karapace).
func caCertPem(ctx context.Context, caKeyStore *dagger.File, caKeyStorePassword *dagger.Secret) (*dagger.File, error) {
	ca := dag.CertificateManagement().LoadCertificateAuthority(caKeyStore, caKeyStorePassword)
	pemBytes, err := dagFileBytes(ctx, ca.CertPemFile())
	if err != nil {
		return nil, fmt.Errorf("materialize CA cert PEM: %w", err)
	}
	return writeWorkdirBytes("sr-ca-cert", "ca.crt", pemBytes)
}

// pkcs12TruststorePem decodes a PKCS#12 truststore (a caller-supplied client
// trust store) and re-encodes its CA certificates as a single PEM bundle —
// PEM-consuming registries (Karapace) need their REST client-auth trust anchor
// in PEM even when the caller supplies PKCS#12.
func pkcs12TruststorePem(ctx context.Context, trustStore *dagger.File, password *dagger.Secret) (*dagger.File, error) {
	tsBytes, err := dagFileBytes(ctx, trustStore)
	if err != nil {
		return nil, fmt.Errorf("export client trust store: %w", err)
	}
	pwd, err := password.Plaintext(ctx)
	if err != nil {
		return nil, fmt.Errorf("read client trust store password: %w", err)
	}
	certs, err := pkcs12.DecodeTrustStore(tsBytes, pwd)
	if err != nil {
		return nil, fmt.Errorf("decode client trust store: %w", err)
	}
	var buf bytes.Buffer
	for _, c := range certs {
		if err := pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}); err != nil {
			return nil, fmt.Errorf("encode client trust cert PEM: %w", err)
		}
	}
	return writeWorkdirBytes("sr-rest-ca", "rest-ca.crt", buf.Bytes())
}
