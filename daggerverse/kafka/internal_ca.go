package main

import (
	"context"
	"fmt"
	"time"

	"dagger/kafka/internal/dagger"
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
	nb := time.Now().UTC().Format(time.RFC3339)
	leaves := make([]externalLeaf, len(brokerHosts))
	for i, h := range brokerHosts {
		leafKeyPem, err := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).Pem().Contents(ctx)
		if err != nil {
			return nil, fmt.Errorf("generate %q external leaf key: %w", h, err)
		}
		leafKey := dag.SetSecret("kafka-external-leaf-key-"+randSuffix(), leafKeyPem)

		leafPwdHex, err := dag.Random().Sha256(ctx)
		if err != nil {
			return nil, fmt.Errorf("generate %q external leaf password: %w", h, err)
		}
		leafPwd := dag.SetSecret("kafka-external-leaf-pwd-"+randSuffix(), leafPwdHex)

		leafSerial, err := dag.Random().Serial(ctx)
		if err != nil {
			return nil, fmt.Errorf("generate %q external leaf serial: %w", h, err)
		}

		issued := ca.IssueServerCertificate(h, nb, leafSerial, leafPwd, leafKey,
			dagger.CertificateManagementCertificateAuthorityIssueServerCertificateOpts{
				DNSSans:      []string{h, "localhost"},
				IPSans:       []string{"127.0.0.1"},
				ValidityDays: 365,
			})
		ksBytes, err := dagFileBytes(ctx, issued.KeyStore().Pkcs12())
		if err != nil {
			return nil, fmt.Errorf("materialize %q external keystore: %w", h, err)
		}
		ksFile, err := writeWorkdirBytes("external-keystore-"+h, "external.p12", ksBytes)
		if err != nil {
			return nil, fmt.Errorf("stage %q external keystore: %w", h, err)
		}
		leaves[i] = externalLeaf{
			KeyStoreFile:     ksFile,
			KeyStorePassword: leafPwd,
		}
	}
	return leaves, nil
}
