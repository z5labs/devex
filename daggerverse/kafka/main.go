// Kafka provides Dagger functions for spinning up KRaft Kafka clusters from
// the apache/kafka-native image and a pure-Go franz-go client that targets
// either the local cluster or any reachable remote cluster.
//
// Plaintext is the only security mechanism supported in this story; TLS /
// mTLS lands in a follow-up.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"dagger/kafka/internal/dagger"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"software.sslmate.com/src/go-pkcs12"
)

type Kafka struct{}

// ServerSecurity describes how a Kafka cluster's external listener
// authenticates and encrypts traffic from clients. Internal listeners
// (inter-broker + controller-quorum) are always mTLS, regardless of mode.
type ServerSecurity struct {
	// +private
	Mode string // PLAINTEXT | TLS | MTLS
	// +private
	CaKeyStore *dagger.File // PKCS#12 CA used to mint per-broker external leaf certs (TLS + MTLS)
	// +private
	CaKeyStorePassword *dagger.Secret
	// +private
	ClientTrustStore *dagger.File // PKCS#12 of CA(s) trusted to sign incoming client certs (MTLS only)
	// +private
	ClientTrustStorePassword *dagger.Secret
}

// ClientSecurity describes how a franz-go client authenticates to a Kafka
// broker.
type ClientSecurity struct {
	// +private
	Mode string // PLAINTEXT | TLS | MTLS
	// +private
	TrustStore *dagger.File // PKCS#12 of the CA(s) the client trusts to identify the broker (TLS + MTLS)
	// +private
	TrustStorePassword *dagger.Secret
	// +private
	KeyStore *dagger.File // PKCS#12 of the client's own leaf cert + key (MTLS only)
	// +private
	KeyStorePassword *dagger.Secret
}

// Cluster represents a running KRaft Kafka cluster, holding references to
// every broker service so callers can bind them into their own containers or
// open a franz-go Client against them.
type Cluster struct {
	// +private
	ClusterID string
	// +private
	BrokerSvcs []*dagger.Service
	// +private
	BrokerHosts []string
	// +private
	ClientSecurityMode string
}

// PlaintextServerSecurity returns a ServerSecurity profile configured for
// unencrypted, unauthenticated traffic on the external listener. Internal
// listeners (inter-broker + controller-quorum) still use mTLS.
func (k *Kafka) PlaintextServerSecurity() *ServerSecurity {
	return &ServerSecurity{Mode: "PLAINTEXT"}
}

// TlsServerSecurity returns a ServerSecurity profile that terminates TLS on
// the external listener. caKeyStore is a PKCS#12 archive containing the
// CA cert + private key the cluster uses to mint per-broker leaf certs;
// each broker leaf carries its stable hostname (e.g. "broker-100") as a
// DNS SAN so franz-go clients dialing the bootstrap address can verify
// the broker against the same CA's truststore.
func (k *Kafka) TlsServerSecurity(
	caKeyStore *dagger.File,
	caKeyStorePassword *dagger.Secret,
) *ServerSecurity {
	return &ServerSecurity{
		Mode:               "TLS",
		CaKeyStore:         caKeyStore,
		CaKeyStorePassword: caKeyStorePassword,
	}
}

// MtlsServerSecurity returns a ServerSecurity profile that terminates mTLS
// on the external listener. caKeyStore signs per-broker server leaves;
// clientTrustStore holds the CA(s) the broker will accept incoming client
// certs from (this can be the same CA as caKeyStore or an independent one
// for asymmetric trust).
func (k *Kafka) MtlsServerSecurity(
	caKeyStore *dagger.File,
	caKeyStorePassword *dagger.Secret,
	clientTrustStore *dagger.File,
	clientTrustStorePassword *dagger.Secret,
) *ServerSecurity {
	return &ServerSecurity{
		Mode:                     "MTLS",
		CaKeyStore:               caKeyStore,
		CaKeyStorePassword:       caKeyStorePassword,
		ClientTrustStore:         clientTrustStore,
		ClientTrustStorePassword: clientTrustStorePassword,
	}
}

// PlaintextClientSecurity returns a ClientSecurity profile configured for
// unencrypted, unauthenticated traffic.
func (k *Kafka) PlaintextClientSecurity() *ClientSecurity {
	return &ClientSecurity{Mode: "PLAINTEXT"}
}

// TlsClientSecurity returns a ClientSecurity profile that opens a TLS
// connection to the broker. trustStore is a PKCS#12 archive of the CA(s)
// the client uses to verify the broker's leaf certificate (typically the
// truststore that pairs with the CA passed to TlsServerSecurity on the
// server side).
func (k *Kafka) TlsClientSecurity(
	trustStore *dagger.File,
	trustStorePassword *dagger.Secret,
) *ClientSecurity {
	return &ClientSecurity{
		Mode:               "TLS",
		TrustStore:         trustStore,
		TrustStorePassword: trustStorePassword,
	}
}

// MtlsClientSecurity returns a ClientSecurity profile that opens an mTLS
// connection: the broker presents its server cert (verified against
// trustStore) and the client presents its own leaf cert from keyStore
// (signed by a CA the broker trusts via its clientTrustStore).
func (k *Kafka) MtlsClientSecurity(
	keyStore *dagger.File,
	keyStorePassword *dagger.Secret,
	trustStore *dagger.File,
	trustStorePassword *dagger.Secret,
) *ClientSecurity {
	return &ClientSecurity{
		Mode:               "MTLS",
		TrustStore:         trustStore,
		TrustStorePassword: trustStorePassword,
		KeyStore:           keyStore,
		KeyStorePassword:   keyStorePassword,
	}
}

// ApacheNativeCluster spins up a KRaft Kafka cluster of the requested
// size with dedicated controller and broker containers, using the
// `apache/kafka-native` GraalVM-compiled image.
//
// Topology: a single controller forms a one-node KRaft quorum; one or more
// brokers connect to it and discover each other over the engine's
// session-wide DNS — no broker-to-broker WithServiceBinding needed.
//
// Multi-controller (controllers > 1) is rejected for now: a true HA quorum
// needs every controller to know every other controller at static config
// time, which Dagger's WithServiceBinding model can't express without an
// unresolvable cycle. TLS / mTLS and multi-controller both land in a
// follow-up.
//
// Session-cached so that repeated chained method calls on the returned
// cluster (Client.Produce → Consume → ListTopics) all observe the SAME
// underlying broker services. The internal CA + per-node leaves are
// minted with fresh random material that we can't make content-addressable,
// so a `+cache="never"` directive here would spawn a brand-new cluster
// (with a brand-new CA the previous invocation's franz-go client doesn't
// trust) every time the test calls another method on the chain.
//
// The GraalVM-compiled image has been observed to flake during the broker
// `setup` step under load — see Dagger Cloud trace
// `377f2e176c4f0e9844cb7f958c1e911b`. If you need the JVM image instead,
// use `ApacheCluster()`.
//
// +cache="session"
func (k *Kafka) ApacheNativeCluster(
	ctx context.Context,
	clusterId string,
	// +default=1
	controllers int,
	// +default=1
	brokers int,
	// +default="docker.io"
	registry string,
	// +default="4.2.0"
	tag string,
	clientListenerSecurity *ServerSecurity,
) (*Cluster, error) {
	image := fmt.Sprintf("%s/apache/kafka-native:%s", registry, tag)
	return buildKafkaCluster(ctx, clusterId, controllers, brokers, image, nil, clientListenerSecurity)
}

// ApacheCluster spins up a KRaft Kafka cluster of the requested size with
// dedicated controller and broker containers, using the `apache/kafka`
// JVM image.
//
// Identical in topology, caching, and security semantics to
// ApacheNativeCluster — only the image differs. The JVM image runs the
// same Scala wrapper but on HotSpot, so it does not share
// `apache/kafka-native`'s AOT-compiled `getpwuid` substitution
// (`Pwd.getpwuid` from `SystemPropertiesSupport.userHomeValue`) that has
// been observed to segfault during broker startup — see Dagger Cloud
// trace `377f2e176c4f0e9844cb7f958c1e911b`. Prefer this constructor
// whenever startup robustness matters more than cold-start latency.
//
// +cache="session"
func (k *Kafka) ApacheCluster(
	ctx context.Context,
	clusterId string,
	// +default=1
	controllers int,
	// +default=1
	brokers int,
	// +default="docker.io"
	registry string,
	// +default="4.2.0"
	tag string,
	clientListenerSecurity *ServerSecurity,
) (*Cluster, error) {
	image := fmt.Sprintf("%s/apache/kafka:%s", registry, tag)
	return buildKafkaCluster(ctx, clusterId, controllers, brokers, image, nil, clientListenerSecurity)
}

// ConfluentCluster spins up a KRaft Kafka cluster of the requested size
// using the `confluentinc/cp-kafka` image — the Confluent Platform
// distribution. Confluent Platform 8.x bundles Apache Kafka 4.x (CP
// 8.2.0 ships Kafka 4.2.0), and cp-kafka speaks the same Scala-wrapper
// `KAFKA_*` env-var contract that ApacheCluster does, so the returned
// `*Cluster` and `ServerSecurity` API are identical to the Apache
// constructors — callers swap distros by changing the constructor
// name alone.
//
// The constructor silently disables Confluent's phone-home telemetry
// (`KAFKA_CONFLUENT_SUPPORT_METRICS_ENABLE=false`) on every broker so
// the cluster behaves the same way the Apache variants do at startup.
//
// +cache="session"
func (k *Kafka) ConfluentCluster(
	ctx context.Context,
	clusterId string,
	// +default=1
	controllers int,
	// +default=1
	brokers int,
	// +default="docker.io"
	registry string,
	// +default="8.2.0"
	tag string,
	clientListenerSecurity *ServerSecurity,
) (*Cluster, error) {
	image := fmt.Sprintf("%s/confluentinc/cp-kafka:%s", registry, tag)
	extraEnv := []envKV{
		{K: "KAFKA_CONFLUENT_SUPPORT_METRICS_ENABLE", V: "false"},
	}
	return buildKafkaCluster(ctx, clusterId, controllers, brokers, image, extraEnv, clientListenerSecurity)
}

// envKV is a key/value pair for distro-specific broker env overrides
// passed into buildKafkaCluster. A slice (not a map) so iteration order
// is stable across invocations and doesn't perturb Dagger's per-arg
// cache key.
type envKV struct{ K, V string }

// buildKafkaCluster is the shared body behind every Kafka.*Cluster
// constructor. The Apache and Confluent images all speak the same
// `KAFKA_*` Scala-wrapper env-var contract under KRaft, so the only
// per-distro inputs are the image string and any extra broker-side
// env overrides (e.g. Confluent's telemetry kill switch).
func buildKafkaCluster(
	ctx context.Context,
	clusterId string,
	controllers int,
	brokers int,
	image string,
	extraBrokerEnv []envKV,
	clientListenerSecurity *ServerSecurity,
) (*Cluster, error) {
	if controllers < 1 {
		return nil, fmt.Errorf("at least one controller is required, got %d", controllers)
	}
	if controllers > 1 {
		return nil, fmt.Errorf(
			"multi-controller clusters are not supported in this story (got controllers=%d); see README for details",
			controllers,
		)
	}
	if brokers < 1 {
		return nil, fmt.Errorf("at least one broker is required, got %d", brokers)
	}
	if clientListenerSecurity == nil {
		return nil, fmt.Errorf("clientListenerSecurity must not be nil")
	}
	mode := clientListenerSecurity.Mode
	switch mode {
	case "PLAINTEXT", "TLS", "MTLS":
	default:
		return nil, fmt.Errorf("unknown clientListenerSecurity mode %q (want PLAINTEXT|TLS|MTLS)", mode)
	}
	if (mode == "TLS" || mode == "MTLS") && (clientListenerSecurity.CaKeyStore == nil || clientListenerSecurity.CaKeyStorePassword == nil) {
		return nil, fmt.Errorf("clientListenerSecurity mode %q requires a CA keystore and password", mode)
	}
	if mode == "MTLS" && (clientListenerSecurity.ClientTrustStore == nil || clientListenerSecurity.ClientTrustStorePassword == nil) {
		return nil, fmt.Errorf("MTLS clientListenerSecurity requires a client trust store and password")
	}
	if raw, err := base64.RawURLEncoding.DecodeString(clusterId); err != nil || len(raw) != 16 {
		return nil, fmt.Errorf(
			"clusterId must be 16 bytes encoded as 22 unpadded base64-url chars, got %q",
			clusterId,
		)
	}

	// Stable hostnames are scoped per-cluster so parallel test invocations
	// don't collide on `broker-100` / `controller-1`. The suffix is derived
	// from the clusterId (already random + DNS-safe after lowercasing and
	// substituting `_` → `-`) so two distinct clusters in the same engine
	// session get distinct hostnames AND each cluster's cert SANs match its
	// own services.
	hostSuffix := clusterHostSuffix(clusterId)
	controllerAlias := "controller-1-" + hostSuffix
	quorumVoters := "1@" + controllerAlias + ":9093"

	brokerHosts := make([]string, brokers)
	for i := range brokerHosts {
		brokerHosts[i] = fmt.Sprintf("broker-%d-%s", 100+i, hostSuffix)
	}

	internal, err := mintInternalCA(ctx, controllerAlias, brokerHosts)
	if err != nil {
		return nil, fmt.Errorf("mint internal CA: %w", err)
	}

	var externalLeaves []externalLeaf
	if mode == "TLS" || mode == "MTLS" {
		externalLeaves, err = mintExternalLeaves(ctx, clientListenerSecurity, brokerHosts)
		if err != nil {
			return nil, fmt.Errorf("mint external leaves: %w", err)
		}
	}

	externalProto := "PLAINTEXT"
	if mode == "TLS" || mode == "MTLS" {
		externalProto = "SSL"
	}

	ctrlCtr := dag.Container().
		From(image).
		// confluentinc/cp-kafka pre-sets KAFKA_ADVERTISED_LISTENERS=""
		// in the image; the Scala config validator rejects the empty
		// value even on controller-only nodes that have no advertised
		// listeners. Strip it so the controller boots regardless of
		// distro (no-op for the Apache images, which don't pre-set it).
		WithoutEnvVariable("KAFKA_ADVERTISED_LISTENERS").
		WithEnvVariable("KAFKA_NODE_ID", "1").
		WithEnvVariable("KAFKA_PROCESS_ROLES", "controller").
		WithEnvVariable("KAFKA_LISTENERS", "CONTROLLER://0.0.0.0:9093").
		WithEnvVariable("KAFKA_CONTROLLER_LISTENER_NAMES", "CONTROLLER").
		WithEnvVariable("KAFKA_LISTENER_SECURITY_PROTOCOL_MAP", "CONTROLLER:SSL").
		WithEnvVariable("KAFKA_CONTROLLER_QUORUM_VOTERS", quorumVoters).
		WithEnvVariable("CLUSTER_ID", clusterId).
		WithExposedPort(9093)
	ctrlCtr = applyInternalListenerSsl(ctrlCtr, "CONTROLLER", internal.Controller)
	ctrlSvc := ctrlCtr.
		AsService(dagger.ContainerAsServiceOpts{UseEntrypoint: true}).
		WithHostname(controllerAlias)

	brokerSvcs := make([]*dagger.Service, brokers)
	for i := 0; i < brokers; i++ {
		nodeID := 100 + i
		brokerHost := brokerHosts[i]
		advertised := fmt.Sprintf("INTERNAL://%s:19092,EXTERNAL://%s:9092", brokerHost, brokerHost)
		brkCtr := dag.Container().
			From(image).
			WithServiceBinding(controllerAlias, ctrlSvc).
			WithEnvVariable("KAFKA_NODE_ID", fmt.Sprintf("%d", nodeID)).
			WithEnvVariable("KAFKA_PROCESS_ROLES", "broker").
			WithEnvVariable("KAFKA_LISTENERS", "INTERNAL://0.0.0.0:19092,EXTERNAL://0.0.0.0:9092").
			WithEnvVariable("KAFKA_ADVERTISED_LISTENERS", advertised).
			WithEnvVariable("KAFKA_INTER_BROKER_LISTENER_NAME", "INTERNAL").
			WithEnvVariable("KAFKA_CONTROLLER_LISTENER_NAMES", "CONTROLLER").
			WithEnvVariable("KAFKA_LISTENER_SECURITY_PROTOCOL_MAP", "CONTROLLER:SSL,INTERNAL:SSL,EXTERNAL:"+externalProto).
			WithEnvVariable("KAFKA_CONTROLLER_QUORUM_VOTERS", quorumVoters).
			WithEnvVariable("CLUSTER_ID", clusterId).
			WithEnvVariable("KAFKA_LOG_DIRS", "/tmp/kraft-combined-logs").
			WithEnvVariable("KAFKA_AUTO_CREATE_TOPICS_ENABLE", "false").
			WithEnvVariable("KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR", "1").
			WithEnvVariable("KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR", "1").
			WithEnvVariable("KAFKA_TRANSACTION_STATE_LOG_MIN_ISR", "1").
			WithEnvVariable("KAFKA_SHARE_COORDINATOR_STATE_TOPIC_REPLICATION_FACTOR", "1").
			WithEnvVariable("KAFKA_SHARE_COORDINATOR_STATE_TOPIC_MIN_ISR", "1").
			WithEnvVariable("KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS", "0").
			WithExposedPort(9092).
			WithExposedPort(19092)
		for _, kv := range extraBrokerEnv {
			brkCtr = brkCtr.WithEnvVariable(kv.K, kv.V)
		}
		brkCtr = applyInternalListenerSsl(brkCtr, "INTERNAL", internal.Brokers[i])
		brkCtr = applyInternalListenerSsl(brkCtr, "CONTROLLER", internal.Brokers[i])
		if externalLeaves != nil {
			brkCtr = applyExternalListenerSsl(brkCtr, externalLeaves[i], clientListenerSecurity)
		}

		svc := brkCtr.
			AsService(dagger.ContainerAsServiceOpts{UseEntrypoint: true}).
			WithHostname(brokerHost)
		brokerSvcs[i] = svc
	}

	return &Cluster{
		ClusterID:          clusterId,
		BrokerSvcs:         brokerSvcs,
		BrokerHosts:        brokerHosts,
		ClientSecurityMode: clientListenerSecurity.Mode,
	}, nil
}

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

const (
	internalKeystorePath   = "/etc/kafka/secrets/internal-keystore.p12"
	internalTruststorePath = "/etc/kafka/secrets/internal-truststore.p12"
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

// writeWorkdirBytes writes content into a content-addressed subdir of the
// kafka module runtime's scratch workdir and returns it as a *dagger.File.
// Distinct callers writing distinct content land at distinct paths;
// identical content collapses to one file (idempotent across re-entry).
func writeWorkdirBytes(label, name string, content []byte) (*dagger.File, error) {
	sum := sha256.Sum256(content)
	dir := "kafka-" + label + "-" + hex.EncodeToString(sum[:8])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return nil, fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("write %s: %w", tmpPath, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("rename to %s: %w", path, err)
	}
	return dag.CurrentModule().WorkdirFile(path), nil
}

// externalLeaf bundles a per-broker external-listener leaf certificate
// (signed by the caller-supplied CA) and the password sealing its PKCS#12
// keystore.
type externalLeaf struct {
	KeyStoreFile     *dagger.File
	KeyStorePassword *dagger.Secret
}

const (
	externalKeystorePath   = "/etc/kafka/secrets/external-keystore.p12"
	externalTruststorePath = "/etc/kafka/secrets/external-truststore.p12"
)

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

// clusterHostSuffix derives a short DNS-safe suffix from a clusterId so
// per-cluster service hostnames don't collide across parallel runs in the
// same engine session. SHA-256 hex first 10 chars: deterministic per
// clusterId (so cache keys are stable) and trivially DNS-LDH compliant
// (lowercase hex, never empty, no leading/trailing dashes).
func clusterHostSuffix(clusterId string) string {
	sum := sha256.Sum256([]byte(clusterId))
	return hex.EncodeToString(sum[:5]) // 10 hex chars = 40 bits
}

// randSuffix returns a fresh hex suffix for naming Dagger secrets uniquely
// across concurrent helper calls.
func randSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is fatal in module runtime context.
		panic(fmt.Sprintf("randSuffix: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// Client constructs a franz-go-backed Kafka client that targets the given
// bootstrap servers. No I/O happens at construction time.
func (k *Kafka) Client(bootstrapServers []string, security *ClientSecurity) *Client {
	return clientFrom(bootstrapServers, security)
}

// clientFrom builds a Client struct from a *ClientSecurity, copying only the
// fields the franz-go path needs. PLAINTEXT mode leaves the TLS-material
// fields nil; TLS / MTLS modes copy them through verbatim.
func clientFrom(bootstrapServers []string, security *ClientSecurity) *Client {
	c := &Client{
		Bootstrap:    bootstrapServers,
		SecurityMode: "PLAINTEXT",
	}
	if security == nil {
		return c
	}
	c.SecurityMode = security.Mode
	c.TrustStore = security.TrustStore
	c.TrustStorePassword = security.TrustStorePassword
	c.KeyStore = security.KeyStore
	c.KeyStorePassword = security.KeyStorePassword
	return c
}

// BootstrapServers returns the host:port pairs each broker advertises on its
// client-facing listener.
//
// +cache="never"
func (c *Cluster) BootstrapServers() []string {
	out := make([]string, len(c.BrokerHosts))
	for i, h := range c.BrokerHosts {
		out[i] = h + ":9092"
	}
	return out
}

// BindBrokers attaches every broker service to the given container under the
// same hostname BootstrapServers reports, so the container can dial brokers
// using the same address strings as a franz-go Client returned from
// Cluster.Client.
//
// +cache="never"
func (c *Cluster) BindBrokers(ctr *dagger.Container) *dagger.Container {
	for i, svc := range c.BrokerSvcs {
		ctr = ctr.WithServiceBinding(c.BrokerHosts[i], svc)
	}
	return ctr
}

// Client starts every broker service in the cluster and returns a franz-go
// Client wired with their bootstrap addresses.
//
// +cache="never"
func (c *Cluster) Client(ctx context.Context, security *ClientSecurity) (*Client, error) {
	for i, svc := range c.BrokerSvcs {
		if _, err := svc.Start(ctx); err != nil {
			return nil, fmt.Errorf("start broker %d: %w", i, err)
		}
	}
	return clientFrom(c.BootstrapServers(), security), nil
}

// Client is a franz-go-backed Kafka client. Each method opens a fresh
// connection so the function call is stateless from Dagger's perspective.
type Client struct {
	// +private
	Bootstrap []string
	// +private
	SecurityMode string
	// +private
	TrustStore *dagger.File
	// +private
	TrustStorePassword *dagger.Secret
	// +private
	KeyStore *dagger.File
	// +private
	KeyStorePassword *dagger.Secret
}

// ConsumedRecord is a single record returned by Client.Consume, with key and
// value already encoded into the requested string representation.
type ConsumedRecord struct {
	Key   string
	Value string
}

// decodeString turns a producer-supplied string into raw bytes per the named
// encoding. Supported encodings: "raw" (literal UTF-8 bytes), "hex",
// "base64" (standard padding).
func decodeString(s, encoding string) ([]byte, error) {
	switch encoding {
	case "raw":
		return []byte(s), nil
	case "hex":
		return hex.DecodeString(s)
	case "base64":
		return base64.StdEncoding.DecodeString(s)
	default:
		return nil, fmt.Errorf("unsupported encoding %q (want raw|hex|base64)", encoding)
	}
}

// encodeBytes renders raw bytes into a string per the named encoding, the
// inverse of decodeString. raw rejects non-UTF-8 input because the result
// crosses GraphQL/JSON, which would silently replace invalid bytes with
// U+FFFD; callers with arbitrary binary should use hex or base64.
func encodeBytes(b []byte, encoding string) (string, error) {
	switch encoding {
	case "raw":
		if !utf8.Valid(b) {
			return "", fmt.Errorf("raw encoding requires valid UTF-8 bytes; use hex or base64 for arbitrary binary")
		}
		return string(b), nil
	case "hex":
		return hex.EncodeToString(b), nil
	case "base64":
		return base64.StdEncoding.EncodeToString(b), nil
	default:
		return "", fmt.Errorf("unsupported encoding %q (want raw|hex|base64)", encoding)
	}
}

// PropertiesFile renders this client's connection settings as a Java
// `client.properties` file so callers can hand it to the Apache Kafka
// command-line tools or to other JVM-based consumers.
//
// For TLS / mTLS modes the properties reference PKCS#12 truststore (and
// keystore for mTLS) by basename — the matching p12 files are written
// alongside `client.properties` in the same directory. Callers should
// export the parent directory (`props.Directory()`) so the relative
// references resolve. Passwords appear plaintext, which is a Kafka CLI
// constraint.
//
// +cache="never"
func (c *Client) PropertiesFile(ctx context.Context) (*dagger.File, error) {
	proto := "PLAINTEXT"
	switch c.SecurityMode {
	case "PLAINTEXT":
	case "TLS", "MTLS":
		proto = "SSL"
	default:
		return nil, fmt.Errorf("PropertiesFile: unsupported SecurityMode %q", c.SecurityMode)
	}
	if c.SecurityMode == "TLS" || c.SecurityMode == "MTLS" {
		if c.TrustStore == nil || c.TrustStorePassword == nil {
			return nil, fmt.Errorf("PropertiesFile: %s mode requires TrustStore + TrustStorePassword", c.SecurityMode)
		}
	}
	if c.SecurityMode == "MTLS" {
		if c.KeyStore == nil || c.KeyStorePassword == nil {
			return nil, fmt.Errorf("PropertiesFile: MTLS mode requires KeyStore + KeyStorePassword")
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "bootstrap.servers=%s\n", strings.Join(c.Bootstrap, ","))
	fmt.Fprintf(&sb, "security.protocol=%s\n", proto)

	type sidecar struct {
		name string
		data []byte
	}
	var sidecars []sidecar

	if c.SecurityMode == "TLS" || c.SecurityMode == "MTLS" {
		tsBytes, err := dagFileBytes(ctx, c.TrustStore)
		if err != nil {
			return nil, fmt.Errorf("export truststore: %w", err)
		}
		tsPwd, err := c.TrustStorePassword.Plaintext(ctx)
		if err != nil {
			return nil, fmt.Errorf("read truststore password: %w", err)
		}
		sidecars = append(sidecars, sidecar{name: "truststore.p12", data: tsBytes})
		fmt.Fprintf(&sb, "ssl.truststore.location=truststore.p12\n")
		fmt.Fprintf(&sb, "ssl.truststore.password=%s\n", tsPwd)
		fmt.Fprintf(&sb, "ssl.truststore.type=PKCS12\n")
	}
	if c.SecurityMode == "MTLS" {
		ksBytes, err := dagFileBytes(ctx, c.KeyStore)
		if err != nil {
			return nil, fmt.Errorf("export keystore: %w", err)
		}
		ksPwd, err := c.KeyStorePassword.Plaintext(ctx)
		if err != nil {
			return nil, fmt.Errorf("read keystore password: %w", err)
		}
		sidecars = append(sidecars, sidecar{name: "keystore.p12", data: ksBytes})
		fmt.Fprintf(&sb, "ssl.keystore.location=keystore.p12\n")
		fmt.Fprintf(&sb, "ssl.keystore.password=%s\n", ksPwd)
		fmt.Fprintf(&sb, "ssl.keystore.type=PKCS12\n")
	}

	content := []byte(sb.String())
	h := sha256.New()
	h.Write(content)
	for _, sc := range sidecars {
		fmt.Fprintf(h, "\x00%s\x00%d\x00", sc.name, len(sc.data))
		h.Write(sc.data)
	}
	dir := "props-" + hex.EncodeToString(h.Sum(nil))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %q: %w", dir, err)
	}

	for _, sc := range sidecars {
		scPath := filepath.Join(dir, sc.name)
		if err := os.WriteFile(scPath, sc.data, 0o600); err != nil {
			return nil, fmt.Errorf("write %s: %w", scPath, err)
		}
	}

	path := filepath.Join(dir, "client.properties")
	tmp, err := os.CreateTemp(dir, ".client.properties-*")
	if err != nil {
		return nil, fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("write %q: %w", tmpPath, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("chmod %q: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("close %q: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("rename to %q: %w", path, err)
	}
	return dag.CurrentModule().WorkdirFile(path), nil
}

// newKgoClient opens a fresh franz-go client against the configured bootstrap
// servers, applying TLS / mTLS dial options when the client is configured
// for them. Callers are responsible for Close.
func (c *Client) newKgoClient(ctx context.Context, extra ...kgo.Opt) (*kgo.Client, error) {
	opts := []kgo.Opt{kgo.SeedBrokers(c.Bootstrap...)}
	cfg, err := c.tlsConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("build tls config: %w", err)
	}
	if cfg != nil {
		opts = append(opts, kgo.DialTLSConfig(cfg))
	}
	opts = append(opts, extra...)
	return kgo.NewClient(opts...)
}

// tlsConfig materializes the client-side *tls.Config from the Client's
// PKCS#12 truststore (and, for mTLS, keystore). Returns (nil, nil) for
// PLAINTEXT mode.
func (c *Client) tlsConfig(ctx context.Context) (*tls.Config, error) {
	if c.SecurityMode == "PLAINTEXT" {
		return nil, nil
	}
	if c.TrustStore == nil || c.TrustStorePassword == nil {
		return nil, fmt.Errorf("%s mode requires TrustStore + TrustStorePassword", c.SecurityMode)
	}
	tsBytes, err := dagFileBytes(ctx, c.TrustStore)
	if err != nil {
		return nil, fmt.Errorf("export truststore: %w", err)
	}
	tsPwd, err := c.TrustStorePassword.Plaintext(ctx)
	if err != nil {
		return nil, fmt.Errorf("read truststore password: %w", err)
	}
	rootCerts, err := pkcs12.DecodeTrustStore(tsBytes, tsPwd)
	if err != nil {
		return nil, fmt.Errorf("decode truststore: %w", err)
	}
	pool := x509.NewCertPool()
	for _, ca := range rootCerts {
		pool.AddCert(ca)
	}
	cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	if c.SecurityMode == "MTLS" {
		if c.KeyStore == nil || c.KeyStorePassword == nil {
			return nil, fmt.Errorf("MTLS mode requires KeyStore + KeyStorePassword")
		}
		ksBytes, err := dagFileBytes(ctx, c.KeyStore)
		if err != nil {
			return nil, fmt.Errorf("export keystore: %w", err)
		}
		ksPwd, err := c.KeyStorePassword.Plaintext(ctx)
		if err != nil {
			return nil, fmt.Errorf("read keystore password: %w", err)
		}
		priv, leaf, chain, err := pkcs12.DecodeChain(ksBytes, ksPwd)
		if err != nil {
			return nil, fmt.Errorf("decode keystore: %w", err)
		}
		certBytes := [][]byte{leaf.Raw}
		for _, link := range chain {
			certBytes = append(certBytes, link.Raw)
		}
		cfg.Certificates = []tls.Certificate{{
			Certificate: certBytes,
			PrivateKey:  priv,
			Leaf:        leaf,
		}}
	}
	return cfg, nil
}

// dagFileBytes materializes a *dagger.File via Export then ReadFile. Used
// for binary content (PKCS#12 archives) where File.Contents() would corrupt
// non-UTF-8 bytes when round-tripped through the GraphQL String type.
func dagFileBytes(ctx context.Context, f *dagger.File) ([]byte, error) {
	local := "kafka-tls-in-" + randSuffix()
	if _, err := f.Export(ctx, local); err != nil {
		return nil, fmt.Errorf("export file: %w", err)
	}
	defer os.Remove(local)
	return os.ReadFile(local)
}

// CreateTopic creates a new topic with the given partition count and
// replication factor. Errors out if the topic already exists.
//
// +cache="never"
func (c *Client) CreateTopic(
	ctx context.Context,
	name string,
	// +default=1
	partitions int,
	// +default=1
	replicationFactor int,
) error {
	if partitions <= 0 {
		return fmt.Errorf("partitions must be > 0, got %d", partitions)
	}
	if replicationFactor <= 0 {
		return fmt.Errorf("replicationFactor must be > 0, got %d", replicationFactor)
	}
	cl, err := c.newKgoClient(ctx)
	if err != nil {
		return fmt.Errorf("new kafka client: %w", err)
	}
	defer cl.Close()

	adm := kadm.NewClient(cl)
	resp, err := adm.CreateTopic(ctx, int32(partitions), int16(replicationFactor), nil, name)
	if err != nil {
		return fmt.Errorf("create topic %q: %w", name, err)
	}
	if resp.Err != nil {
		return fmt.Errorf("create topic %q: %w", name, resp.Err)
	}
	return nil
}

// DeleteTopic deletes the named topic.
//
// +cache="never"
func (c *Client) DeleteTopic(ctx context.Context, name string) error {
	cl, err := c.newKgoClient(ctx)
	if err != nil {
		return fmt.Errorf("new kafka client: %w", err)
	}
	defer cl.Close()

	adm := kadm.NewClient(cl)
	resps, err := adm.DeleteTopics(ctx, name)
	if err != nil {
		return fmt.Errorf("delete topic %q: %w", name, err)
	}
	for _, r := range resps {
		if r.Err != nil {
			return fmt.Errorf("delete topic %q: %w", name, r.Err)
		}
	}
	return nil
}

// Produce synchronously writes one record to the topic. Key and value are
// decoded from their named encodings into raw bytes before being sent.
//
// +cache="never"
func (c *Client) Produce(
	ctx context.Context,
	topic string,
	key string,
	value string,
	// +default="raw"
	keyEncoding string,
	// +default="raw"
	valueEncoding string,
) error {
	keyBytes, err := decodeString(key, keyEncoding)
	if err != nil {
		return fmt.Errorf("decode key: %w", err)
	}
	valBytes, err := decodeString(value, valueEncoding)
	if err != nil {
		return fmt.Errorf("decode value: %w", err)
	}

	cl, err := c.newKgoClient(ctx)
	if err != nil {
		return fmt.Errorf("new kafka client: %w", err)
	}
	defer cl.Close()

	res := cl.ProduceSync(ctx, &kgo.Record{Topic: topic, Key: keyBytes, Value: valBytes})
	if err := res.FirstErr(); err != nil {
		return fmt.Errorf("produce to %q: %w", topic, err)
	}
	return nil
}

// Consume reads up to maxMessages records from the topic, starting at the
// earliest offset, returning when either maxMessages have been gathered or
// the parsed timeout elapses. Each record's key and value are encoded into
// the requested string forms before being returned.
//
// When group is non-empty, the consume runs as a member of that consumer
// group: the broker assigns partitions and the join itself writes group
// metadata to __consumer_offsets (offsets are not committed — the function
// stays idempotent under +cache="never"). When group is empty (the
// default), partitions are consumed directly with no group state.
//
// +cache="never"
func (c *Client) Consume(
	ctx context.Context,
	topic string,
	// +default=1
	maxMessages int,
	// +default="10s"
	timeout string,
	// +default="raw"
	keyEncoding string,
	// +default="raw"
	valueEncoding string,
	// +default=""
	group string,
) ([]ConsumedRecord, error) {
	if maxMessages <= 0 {
		return nil, fmt.Errorf("maxMessages must be > 0, got %d", maxMessages)
	}
	d, err := time.ParseDuration(timeout)
	if err != nil {
		return nil, fmt.Errorf("parse timeout %q: %w", timeout, err)
	}
	if d <= 0 {
		return nil, fmt.Errorf("timeout must be > 0, got %s", d)
	}

	opts := []kgo.Opt{
		kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	}
	if group != "" {
		// DisableAutoCommit keeps Consume idempotent under
		// +cache="never": re-runs triggered by lazy record loading
		// always re-read from the start instead of resuming past a
		// committed offset. The group join itself still exercises
		// __consumer_offsets, which is what proves the system-topic
		// replication-factor defaults are correct.
		opts = append(opts, kgo.ConsumerGroup(group), kgo.DisableAutoCommit())
	}
	cl, err := c.newKgoClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("new kafka client: %w", err)
	}
	defer cl.Close()

	deadlineCtx, cancel := context.WithTimeout(ctx, d)
	defer cancel()

	out := make([]ConsumedRecord, 0, maxMessages)
	for len(out) < maxMessages {
		fetches := cl.PollFetches(deadlineCtx)
		if errs := fetches.Errors(); len(errs) > 0 {
			for _, e := range errs {
				if errors.Is(e.Err, context.DeadlineExceeded) || errors.Is(e.Err, context.Canceled) {
					continue
				}
				return nil, fmt.Errorf("poll fetches: %w", e.Err)
			}
			return out, nil
		}
		iter := fetches.RecordIter()
		for !iter.Done() && len(out) < maxMessages {
			r := iter.Next()
			keyStr, err := encodeBytes(r.Key, keyEncoding)
			if err != nil {
				return nil, fmt.Errorf("encode key: %w", err)
			}
			valStr, err := encodeBytes(r.Value, valueEncoding)
			if err != nil {
				return nil, fmt.Errorf("encode value: %w", err)
			}
			out = append(out, ConsumedRecord{Key: keyStr, Value: valStr})
		}
	}
	return out, nil
}

// ListTopics returns the names of every topic the broker reports.
//
// +cache="never"
func (c *Client) ListTopics(ctx context.Context) ([]string, error) {
	cl, err := c.newKgoClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("new kafka client: %w", err)
	}
	defer cl.Close()

	adm := kadm.NewClient(cl)
	topics, err := adm.ListTopics(ctx)
	if err != nil {
		return nil, fmt.Errorf("list topics: %w", err)
	}
	out := make([]string, 0, len(topics))
	for name := range topics {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}
