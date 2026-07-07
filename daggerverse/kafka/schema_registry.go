package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"dagger/kafka/internal/dagger"

	"software.sslmate.com/src/go-pkcs12"
)

// schemaRegistryPort is the HTTP(S) port the Schema Registry REST API listens
// on, both inside the container and as advertised to callers.
const schemaRegistryPort = 8081

// srContentType is the versioned media type the Schema Registry REST API
// expects on requests and returns on responses.
const srContentType = "application/vnd.schemaregistry.v1+json"

// Schema Registry TLS material mount paths inside the registry containers.
// PKCS#12 for the Java (Confluent) and Quarkus (Apicurio) images; PEM for the
// Python (Karapace) image.
const (
	srServerKeystorePath       = "/etc/schema-registry/secrets/server-keystore.p12"
	srRestTruststorePath       = "/etc/schema-registry/secrets/rest-truststore.p12"
	srKafkastoreTruststorePath = "/etc/schema-registry/secrets/kafkastore-truststore.p12"
	srKafkastoreKeystorePath   = "/etc/schema-registry/secrets/kafkastore-keystore.p12"

	karapaceServerCertPath = "/etc/karapace/certs/server.crt"
	karapaceServerKeyPath  = "/etc/karapace/certs/server.key"
	karapaceStorageCaPath  = "/etc/karapace/certs/storage-ca.crt"
	karapaceRestCaPath     = "/etc/karapace/certs/rest-ca.crt"
	karapaceClientCertPath = "/etc/karapace/certs/client.crt"
	karapaceClientKeyPath  = "/etc/karapace/certs/client.key"
)

// SchemaRegistry is the module's shared Schema Registry abstraction, bound
// to a Kafka cluster's brokers. It stores schemas in the cluster's `_schemas`
// topic and exposes a REST API for registering and looking up Avro / JSON
// Schema / Protobuf schemas by subject.
//
// The same type is returned both by Kafka.ConfluentSchemaRegistry — a
// separate `cp-schema-registry` container — and by
// RedpandaCluster.SchemaRegistry, which surfaces the Schema Registry bundled
// inside the Redpanda broker process. Callers treat the two uniformly; the
// Bundled field records which kind this is so Stop behaves correctly.
//
// The constructor is session-cached so chained calls
// (Client().RegisterSchema(...) → LookupSchemaByID(...)) all observe the
// same underlying service.
type SchemaRegistry struct {
	// +private
	SchemaRegistrySvc *dagger.Service
	// +private
	AdvertisedHost string
	// +private
	AdvertisedPort int
	// Bundled marks a registry whose service is shared with the owning
	// cluster (e.g. Redpanda's in-broker Schema Registry). The cluster owns
	// that service's lifecycle, so Stop is a no-op for a bundled registry —
	// stopping it would otherwise tear the whole cluster down.
	//
	// +private
	Bundled bool
	// BasePath is the URL path the registry's Confluent-Schema-Registry REST
	// surface is rooted at. Confluent (cp-schema-registry) and Redpanda serve
	// it at the root, so they leave this empty; Apicurio exposes it under a
	// CSR-compat prefix (`/apis/ccompat/v7`). Client() folds it into the
	// client BaseURL so every SchemaRegistryClient method composes the same
	// `/subjects/...` paths regardless of backend.
	//
	// +private
	BasePath string
	// SecurityMode records how the registry's REST endpoint is secured
	// (PLAINTEXT | TLS | MTLS). Client() reads it to choose the URL scheme
	// (http vs https) and to validate the caller's client-security profile
	// matches the server side.
	//
	// +private
	SecurityMode string
}

// SchemaRegistryClient is a pure-Go net/http client for a Schema Registry's
// admin REST API. Each method opens a fresh request so the function call is
// stateless from Dagger's perspective.
type SchemaRegistryClient struct {
	// +private
	Svc *dagger.Service
	// +private
	BaseURL string
	// +private
	SecurityMode string // PLAINTEXT | TLS | MTLS
	// +private
	TrustStore *dagger.File // PKCS#12 CA(s) verifying the registry REST cert (TLS+MTLS)
	// +private
	TrustStorePassword *dagger.Secret
	// +private
	KeyStore *dagger.File // PKCS#12 client leaf presented to the registry (MTLS)
	// +private
	KeyStorePassword *dagger.Secret
}

// RegisteredSchema is one schema version as the Confluent Schema Registry
// reports it.
//
// The field names deliberately diverge from the REST API's JSON keys
// (`id`, `schema`): an exported `ID` field collides with the synthetic
// Dagger object `id`, and `Schema` is a GraphQL keyword that breaks
// consumer-module codegen — see daggerverse/CLAUDE.md.
type RegisteredSchema struct {
	Subject    string // registry subject the schema is registered under
	Version    int    // monotonic version within the subject
	SchemaID   int    // globally-unique registry schema id
	Definition string // the schema text itself (Avro / JSON Schema / Protobuf)
	SchemaType string // AVRO | JSON | PROTOBUF
}

// validateRegistrySecurity checks a registry constructor's security profile is
// present, has a known mode, carries the material that mode requires, and
// matches the backing cluster's security mode. name is the constructor name for
// error messages. A matching mode means a PLAINTEXT registry pairs with a
// PLAINTEXT cluster (reproducing pre-TLS behaviour) and a TLS/mTLS registry
// pairs with a same-mode cluster so its kafka-storage connection authenticates.
func validateRegistrySecurity(name string, security *SchemaRegistrySecurity, clusterMode string) error {
	if security == nil {
		return fmt.Errorf("%s: security must not be nil (use Kafka.PlaintextSchemaRegistrySecurity() for an unencrypted registry)", name)
	}
	switch security.Mode {
	case "PLAINTEXT", "TLS", "MTLS":
	default:
		return fmt.Errorf("%s: unknown security mode %q (want PLAINTEXT|TLS|MTLS)", name, security.Mode)
	}
	if security.Mode != clusterMode {
		return fmt.Errorf(
			"%s: registry security is %q but the cluster client listener is %q; they must match "+
				"so the registry's kafka-storage connection authenticates against the broker",
			name, security.Mode, clusterMode,
		)
	}
	if (security.Mode == "TLS" || security.Mode == "MTLS") && (security.CaKeyStore == nil || security.CaKeyStorePassword == nil) {
		return fmt.Errorf("%s: %s security requires a CA keystore and password", name, security.Mode)
	}
	if security.Mode == "MTLS" && (security.ClientTrustStore == nil || security.ClientTrustStorePassword == nil) {
		return fmt.Errorf("%s: MTLS security requires a client trust store and password", name)
	}
	return nil
}

// applyConfluentSchemaRegistryTls mounts the registry's REST server leaf and
// broker-facing truststore/keystore onto a cp-schema-registry container and
// sets the SSL env vars for both surfaces: the Jetty REST endpoint
// (SCHEMA_REGISTRY_SSL_*) and the kafkastore client connection
// (SCHEMA_REGISTRY_KAFKASTORE_SSL_*). All material is PKCS#12. Assumes the
// caller has already switched SCHEMA_REGISTRY_LISTENERS to https and the
// kafkastore bootstrap scheme to SSL://.
func applyConfluentSchemaRegistryTls(ctx context.Context, ctr *dagger.Container, security *SchemaRegistrySecurity, srHost string) (*dagger.Container, error) {
	leaf, err := mintServiceLeaf(ctx, security.CaKeyStore, security.CaKeyStorePassword, srHost, "sr-confluent-rest")
	if err != nil {
		return nil, fmt.Errorf("mint schema registry REST leaf: %w", err)
	}
	tsFile, tsPwd, err := caTrustStorePkcs12(ctx, security.CaKeyStore, security.CaKeyStorePassword)
	if err != nil {
		return nil, fmt.Errorf("derive kafkastore truststore: %w", err)
	}
	ctr = ctr.
		// REST endpoint TLS termination.
		WithFile(srServerKeystorePath, leaf.KeyStoreFile, dagger.ContainerWithFileOpts{Permissions: 0o644}).
		WithEnvVariable("SCHEMA_REGISTRY_SSL_KEYSTORE_LOCATION", srServerKeystorePath).
		WithSecretVariable("SCHEMA_REGISTRY_SSL_KEYSTORE_PASSWORD", leaf.KeyStorePassword).
		WithEnvVariable("SCHEMA_REGISTRY_SSL_KEYSTORE_TYPE", "PKCS12").
		WithSecretVariable("SCHEMA_REGISTRY_SSL_KEY_PASSWORD", leaf.KeyStorePassword).
		// The inter-instance listener (used for leader coordination) defaults
		// to the http scheme; with an https-only listener it must be pointed
		// at https or startup fails with "No listener configured with
		// requested scheme http".
		WithEnvVariable("SCHEMA_REGISTRY_INTER_INSTANCE_PROTOCOL", "https").
		// kafkastore connection verifies the broker against the cluster CA.
		WithEnvVariable("SCHEMA_REGISTRY_KAFKASTORE_SECURITY_PROTOCOL", "SSL").
		WithFile(srKafkastoreTruststorePath, tsFile, dagger.ContainerWithFileOpts{Permissions: 0o644}).
		WithEnvVariable("SCHEMA_REGISTRY_KAFKASTORE_SSL_TRUSTSTORE_LOCATION", srKafkastoreTruststorePath).
		WithSecretVariable("SCHEMA_REGISTRY_KAFKASTORE_SSL_TRUSTSTORE_PASSWORD", tsPwd).
		WithEnvVariable("SCHEMA_REGISTRY_KAFKASTORE_SSL_TRUSTSTORE_TYPE", "PKCS12")

	if security.Mode == "MTLS" {
		// REST endpoint requires client certs, trusted via ClientTrustStore.
		ctr = ctr.
			WithEnvVariable("SCHEMA_REGISTRY_SSL_CLIENT_AUTHENTICATION", "REQUIRED").
			WithFile(srRestTruststorePath, security.ClientTrustStore, dagger.ContainerWithFileOpts{Permissions: 0o644}).
			WithEnvVariable("SCHEMA_REGISTRY_SSL_TRUSTSTORE_LOCATION", srRestTruststorePath).
			WithSecretVariable("SCHEMA_REGISTRY_SSL_TRUSTSTORE_PASSWORD", security.ClientTrustStorePassword).
			WithEnvVariable("SCHEMA_REGISTRY_SSL_TRUSTSTORE_TYPE", "PKCS12")

		// kafkastore connection presents the registry's own client leaf to
		// the mTLS broker (signed by the same CA the broker trusts).
		clientLeaf, err := mintClientLeaf(ctx, security.CaKeyStore, security.CaKeyStorePassword, srHost, "sr-confluent-kafkastore")
		if err != nil {
			return nil, fmt.Errorf("mint schema registry kafkastore client leaf: %w", err)
		}
		ctr = ctr.
			WithFile(srKafkastoreKeystorePath, clientLeaf.KeyStoreFile, dagger.ContainerWithFileOpts{Permissions: 0o644}).
			WithEnvVariable("SCHEMA_REGISTRY_KAFKASTORE_SSL_KEYSTORE_LOCATION", srKafkastoreKeystorePath).
			WithSecretVariable("SCHEMA_REGISTRY_KAFKASTORE_SSL_KEYSTORE_PASSWORD", clientLeaf.KeyStorePassword).
			WithEnvVariable("SCHEMA_REGISTRY_KAFKASTORE_SSL_KEYSTORE_TYPE", "PKCS12").
			WithSecretVariable("SCHEMA_REGISTRY_KAFKASTORE_SSL_KEY_PASSWORD", clientLeaf.KeyStorePassword)
	}
	return ctr, nil
}

// ConfluentSchemaRegistry spins up a Confluent Schema Registry service
// (`confluentinc/cp-schema-registry`) alongside the given Kafka cluster.
// The registry talks the Kafka wire protocol to the cluster's brokers for
// its `_schemas` topic and exposes its own REST API on top, so it composes
// on any *Cluster regardless of distro — cp-schema-registry simply pairs
// most naturally with a cp-kafka ConfluentCluster.
//
// Only PLAINTEXT clusters are supported in this story: the constructor
// rejects TLS / mTLS clusters and points callers at the TLS follow-up.
//
// Session-cached for the same reason the cluster constructors are — a
// `+cache="never"` directive here would mint a brand-new registry service
// for every chained client call.
//
// +cache="session"
func (k *Kafka) ConfluentSchemaRegistry(
	ctx context.Context,
	cluster *Cluster,
	// +default="docker.io"
	registry string,
	// +default="8.2.0"
	tag string,
	security *SchemaRegistrySecurity,
) (*SchemaRegistry, error) {
	if cluster == nil {
		return nil, fmt.Errorf("Kafka.ConfluentSchemaRegistry: cluster must not be nil")
	}
	if len(cluster.BrokerSvcs) == 0 {
		return nil, fmt.Errorf("Kafka.ConfluentSchemaRegistry: cluster has no brokers")
	}
	if err := validateRegistrySecurity("Kafka.ConfluentSchemaRegistry", security, cluster.ClientSecurityMode); err != nil {
		return nil, err
	}
	tls := security.Mode != "PLAINTEXT"

	image := fmt.Sprintf("%s/confluentinc/cp-schema-registry:%s", registry, tag)
	// `csr-` (confluent schema registry) keeps the hostname short — a
	// longer `schema-registry-<suffix>` alias trips runc's sethostname on
	// startup — and gives sibling registry implementations room to pick
	// their own distinct prefix on the same cluster.
	srHost := "csr-" + clusterHostSuffix(cluster.ClusterID)

	// cp-schema-registry wants its bootstrap servers scheme-prefixed;
	// Cluster.BootstrapServers reports bare host:port. On a TLS/mTLS cluster
	// the kafkastore connection dials the broker's SSL listener.
	storeScheme := "PLAINTEXT"
	if tls {
		storeScheme = "SSL"
	}
	bootstrap := cluster.BootstrapServers()
	kafkastore := make([]string, len(bootstrap))
	for i, b := range bootstrap {
		kafkastore[i] = storeScheme + "://" + b
	}

	// The brokers run with KAFKA_AUTO_CREATE_TOPICS_ENABLE=false and
	// Schema Registry creates the `_schemas` topic itself on first boot.
	// Its default replication factor is 3, which fails on the
	// single-broker clusters tests use — pin it to the broker count,
	// capped at 3, mirroring buildKafkaCluster's system-topic RF handling.
	rf := len(cluster.BrokerSvcs)
	if rf > 3 {
		rf = 3
	}

	listenerScheme := "http"
	if tls {
		listenerScheme = "https"
	}
	ctr := dag.Container().
		From(image).
		WithEnvVariable("SCHEMA_REGISTRY_HOST_NAME", srHost).
		WithEnvVariable("SCHEMA_REGISTRY_LISTENERS", fmt.Sprintf("%s://0.0.0.0:%d", listenerScheme, schemaRegistryPort)).
		WithEnvVariable("SCHEMA_REGISTRY_KAFKASTORE_BOOTSTRAP_SERVERS", strings.Join(kafkastore, ",")).
		WithEnvVariable("SCHEMA_REGISTRY_KAFKASTORE_TOPIC_REPLICATION_FACTOR", strconv.Itoa(rf)).
		WithExposedPort(schemaRegistryPort)
	if tls {
		var err error
		ctr, err = applyConfluentSchemaRegistryTls(ctx, ctr, security, srHost)
		if err != nil {
			return nil, err
		}
	}
	ctr = cluster.BindBrokers(ctr)

	srSvc := ctr.
		AsService(dagger.ContainerAsServiceOpts{UseEntrypoint: true}).
		WithHostname(srHost)

	return &SchemaRegistry{
		SchemaRegistrySvc: srSvc,
		AdvertisedHost:    srHost,
		AdvertisedPort:    schemaRegistryPort,
		SecurityMode:      security.Mode,
	}, nil
}

// applyApicurioSchemaRegistryTls terminates TLS on Apicurio's Quarkus REST
// endpoint (PKCS#12 keystore) and secures its kafkasql storage connection to
// the broker.
//
// NOTE: the kafkasql storage SSL env-var surface is the least-certain knob in
// this feature (Apicurio 2.6 has shifted it across `KAFKA_SSL_*`,
// `REGISTRY_KAFKASQL_SECURITY_*`, and `apicurio.kafka.common.*` between minor
// builds); it is validated by the Apicurio TLS round-trip test and iterated
// under TDD. PKCS#12 (not PEM) is fed to the storage truststore to avoid
// Apicurio's PEM-truststore-plus-password startup failure (Apicurio #4975).
func applyApicurioSchemaRegistryTls(ctx context.Context, ctr *dagger.Container, security *SchemaRegistrySecurity, srHost string) (*dagger.Container, error) {
	leaf, err := mintServiceLeaf(ctx, security.CaKeyStore, security.CaKeyStorePassword, srHost, "sr-apicurio-rest")
	if err != nil {
		return nil, fmt.Errorf("mint apicurio REST leaf: %w", err)
	}
	tsFile, tsPwd, err := caTrustStorePkcs12(ctx, security.CaKeyStore, security.CaKeyStorePassword)
	if err != nil {
		return nil, fmt.Errorf("derive kafkasql truststore: %w", err)
	}
	ctr = ctr.
		// Quarkus REST TLS: serve HTTPS only on schemaRegistryPort.
		WithFile(srServerKeystorePath, leaf.KeyStoreFile, dagger.ContainerWithFileOpts{Permissions: 0o644}).
		WithEnvVariable("QUARKUS_HTTP_SSL_PORT", strconv.Itoa(schemaRegistryPort)).
		WithEnvVariable("QUARKUS_HTTP_INSECURE_REQUESTS", "disabled").
		WithEnvVariable("QUARKUS_HTTP_SSL_CERTIFICATE_KEY_STORE_FILE", srServerKeystorePath).
		WithSecretVariable("QUARKUS_HTTP_SSL_CERTIFICATE_KEY_STORE_PASSWORD", leaf.KeyStorePassword).
		WithEnvVariable("QUARKUS_HTTP_SSL_CERTIFICATE_KEY_STORE_FILE_TYPE", "PKCS12").
		// kafkasql storage over SSL, PKCS#12 truststore.
		WithFile(srKafkastoreTruststorePath, tsFile, dagger.ContainerWithFileOpts{Permissions: 0o644}).
		WithEnvVariable("KAFKA_SECURITY_PROTOCOL", "SSL").
		WithEnvVariable("KAFKA_SSL_TRUSTSTORE_LOCATION", srKafkastoreTruststorePath).
		WithSecretVariable("KAFKA_SSL_TRUSTSTORE_PASSWORD", tsPwd).
		WithEnvVariable("KAFKA_SSL_TRUSTSTORE_TYPE", "PKCS12")

	if security.Mode == "MTLS" {
		ctr = ctr.
			WithEnvVariable("QUARKUS_HTTP_SSL_CLIENT_AUTH", "required").
			WithFile(srRestTruststorePath, security.ClientTrustStore, dagger.ContainerWithFileOpts{Permissions: 0o644}).
			WithEnvVariable("QUARKUS_HTTP_SSL_CERTIFICATE_TRUST_STORE_FILE", srRestTruststorePath).
			WithSecretVariable("QUARKUS_HTTP_SSL_CERTIFICATE_TRUST_STORE_PASSWORD", security.ClientTrustStorePassword).
			WithEnvVariable("QUARKUS_HTTP_SSL_CERTIFICATE_TRUST_STORE_FILE_TYPE", "PKCS12")

		clientLeaf, err := mintClientLeaf(ctx, security.CaKeyStore, security.CaKeyStorePassword, srHost, "sr-apicurio-kafkasql")
		if err != nil {
			return nil, fmt.Errorf("mint apicurio kafkasql client leaf: %w", err)
		}
		ctr = ctr.
			WithFile(srKafkastoreKeystorePath, clientLeaf.KeyStoreFile, dagger.ContainerWithFileOpts{Permissions: 0o644}).
			WithEnvVariable("KAFKA_SSL_KEYSTORE_LOCATION", srKafkastoreKeystorePath).
			WithSecretVariable("KAFKA_SSL_KEYSTORE_PASSWORD", clientLeaf.KeyStorePassword).
			WithEnvVariable("KAFKA_SSL_KEYSTORE_TYPE", "PKCS12").
			WithSecretVariable("KAFKA_SSL_KEY_PASSWORD", clientLeaf.KeyStorePassword)
	}
	return ctr, nil
}

// ApicurioSchemaRegistry spins up an Apicurio Registry service
// (`apicurio/apicurio-registry-kafkasql`) alongside the given Kafka cluster.
// Apicurio stores its data in a Kafka topic of its own and exposes a
// Confluent-Schema-Registry-compatible REST API under `/apis/ccompat/v7`, so
// the same *SchemaRegistryClient that drives ConfluentSchemaRegistry works
// against it unchanged — the CSR-compat prefix is folded into BasePath.
//
// Apicurio is a more permissively licensed alternative to cp-schema-registry
// with a broader native artifact-type catalogue (Avro, JSON Schema,
// Protobuf, OpenAPI, AsyncAPI, GraphQL, WSDL, XSD); over the CSR-compat
// surface only the AVRO / JSON / PROTOBUF subset is reachable.
//
// security must match the backing cluster's mode: a PLAINTEXT profile keeps
// the REST endpoint on HTTP and the kafkasql connection unencrypted; a TLS /
// mTLS profile terminates HTTPS on the REST endpoint and secures the kafkasql
// connection against a matching-mode cluster.
//
// Session-cached for the same reason ConfluentSchemaRegistry is — a
// `+cache="never"` directive here would mint a brand-new registry service
// for every chained client call.
//
// +cache="session"
func (k *Kafka) ApicurioSchemaRegistry(
	ctx context.Context,
	cluster *Cluster,
	// +default="docker.io"
	registry string,
	// +default="2.6.13.Final"
	tag string,
	security *SchemaRegistrySecurity,
) (*SchemaRegistry, error) {
	if cluster == nil {
		return nil, fmt.Errorf("Kafka.ApicurioSchemaRegistry: cluster must not be nil")
	}
	if len(cluster.BrokerSvcs) == 0 {
		return nil, fmt.Errorf("Kafka.ApicurioSchemaRegistry: cluster has no brokers")
	}
	if err := validateRegistrySecurity("Kafka.ApicurioSchemaRegistry", security, cluster.ClientSecurityMode); err != nil {
		return nil, err
	}
	tls := security.Mode != "PLAINTEXT"

	image := fmt.Sprintf("%s/apicurio/apicurio-registry-kafkasql:%s", registry, tag)
	// `asr-` (apicurio schema registry) keeps the hostname short — a longer
	// alias trips runc's sethostname on startup — and stays distinct from
	// the `csr-` prefix ConfluentSchemaRegistry uses on the same cluster.
	srHost := "asr-" + clusterHostSuffix(cluster.ClusterID)

	// Apicurio's kafkasql storage wants bare host:port bootstrap servers
	// (no scheme prefix, unlike cp-schema-registry). It auto-creates its
	// own journal topic at replication factor 1, so no broker-count RF
	// capping is needed here.
	ctr := dag.Container().
		From(image).
		WithEnvVariable("KAFKA_BOOTSTRAP_SERVERS", strings.Join(cluster.BootstrapServers(), ",")).
		WithExposedPort(schemaRegistryPort)
	// Apicurio is a Quarkus app listening on 8080 by default; pin it to
	// schemaRegistryPort so the advertised port matches cp-schema-registry.
	// Under TLS the port is the HTTPS SSL port (set in the tls helper) and the
	// plaintext HTTP port is disabled, so only pin QUARKUS_HTTP_PORT here for
	// the plaintext path.
	if !tls {
		ctr = ctr.WithEnvVariable("QUARKUS_HTTP_PORT", strconv.Itoa(schemaRegistryPort))
	} else {
		var err error
		ctr, err = applyApicurioSchemaRegistryTls(ctx, ctr, security, srHost)
		if err != nil {
			return nil, err
		}
	}
	ctr = cluster.BindBrokers(ctr)

	srSvc := ctr.
		AsService(dagger.ContainerAsServiceOpts{UseEntrypoint: true}).
		WithHostname(srHost)

	return &SchemaRegistry{
		SchemaRegistrySvc: srSvc,
		AdvertisedHost:    srHost,
		AdvertisedPort:    schemaRegistryPort,
		// Apicurio serves the Confluent-compatible REST API under this
		// prefix rather than at the root.
		BasePath:     "/apis/ccompat/v7",
		SecurityMode: security.Mode,
	}, nil
}

// applyKarapaceSchemaRegistryTls terminates TLS on Karapace's REST endpoint
// and secures its aiokafka storage connection to the broker. Karapace is a
// Python service that consumes PEM material only (no PKCS#12), so the leaf's
// PEM cert/key and the CA cert PEM are mounted rather than keystores; the
// private keys cross as *dagger.Secret so their plaintext never lands on disk.
func applyKarapaceSchemaRegistryTls(ctx context.Context, ctr *dagger.Container, security *SchemaRegistrySecurity, srHost string) (*dagger.Container, error) {
	leaf, err := mintServiceLeaf(ctx, security.CaKeyStore, security.CaKeyStorePassword, srHost, "sr-karapace-rest")
	if err != nil {
		return nil, fmt.Errorf("mint karapace REST leaf: %w", err)
	}
	caPem, err := caCertPem(ctx, security.CaKeyStore, security.CaKeyStorePassword)
	if err != nil {
		return nil, fmt.Errorf("derive karapace storage CA: %w", err)
	}
	ctr = ctr.
		// REST endpoint TLS termination (PEM).
		WithFile(karapaceServerCertPath, leaf.CertPemFile, dagger.ContainerWithFileOpts{Permissions: 0o644}).
		WithMountedSecret(karapaceServerKeyPath, leaf.PrivateKeyPem, dagger.ContainerWithMountedSecretOpts{Mode: 0o444}).
		WithEnvVariable("KARAPACE_SERVER_TLS_CERTFILE", karapaceServerCertPath).
		WithEnvVariable("KARAPACE_SERVER_TLS_KEYFILE", karapaceServerKeyPath).
		// aiokafka storage connection verifies the broker against the cluster CA.
		WithEnvVariable("KARAPACE_SECURITY_PROTOCOL", "SSL").
		WithFile(karapaceStorageCaPath, caPem, dagger.ContainerWithFileOpts{Permissions: 0o644}).
		WithEnvVariable("KARAPACE_SSL_CAFILE", karapaceStorageCaPath)

	if security.Mode == "MTLS" {
		// REST endpoint requires client certs; Karapace needs the trust anchor
		// in PEM, so convert the caller's PKCS#12 client trust store.
		restCa, err := pkcs12TruststorePem(ctx, security.ClientTrustStore, security.ClientTrustStorePassword)
		if err != nil {
			return nil, fmt.Errorf("derive karapace REST client-auth CA: %w", err)
		}
		clientLeaf, err := mintClientLeaf(ctx, security.CaKeyStore, security.CaKeyStorePassword, srHost, "sr-karapace-storage")
		if err != nil {
			return nil, fmt.Errorf("mint karapace storage client leaf: %w", err)
		}
		ctr = ctr.
			WithFile(karapaceRestCaPath, restCa, dagger.ContainerWithFileOpts{Permissions: 0o644}).
			WithEnvVariable("KARAPACE_SERVER_TLS_CAFILE", karapaceRestCaPath).
			WithEnvVariable("KARAPACE_SERVER_TLS_CLIENT_AUTH", "required").
			WithFile(karapaceClientCertPath, clientLeaf.CertPemFile, dagger.ContainerWithFileOpts{Permissions: 0o644}).
			WithMountedSecret(karapaceClientKeyPath, clientLeaf.PrivateKeyPem, dagger.ContainerWithMountedSecretOpts{Mode: 0o444}).
			WithEnvVariable("KARAPACE_SSL_CERTFILE", karapaceClientCertPath).
			WithEnvVariable("KARAPACE_SSL_KEYFILE", karapaceClientKeyPath)
	}
	return ctr, nil
}

// KarapaceSchemaRegistry spins up a Karapace service
// (`ghcr.io/aiven-open/karapace`) alongside the given Kafka cluster. Karapace
// is Aiven's drop-in Python reimplementation of the Confluent Schema Registry:
// it talks the Kafka wire protocol to the cluster's brokers for its `_schemas`
// topic and serves a Confluent-Schema-Registry-compatible REST API at the
// root, so the same *SchemaRegistryClient that drives ConfluentSchemaRegistry
// works against it unchanged (BasePath stays empty).
//
// Unlike the other registry constructors, `registry` defaults to `ghcr.io`:
// Karapace publishes to GitHub Container Registry rather than Docker Hub,
// which also keeps CI clear of Docker Hub rate limits and Confluent's image
// licensing.
//
// security must match the backing cluster's mode. Karapace consumes PEM (not
// PKCS#12) for its own listener and aiokafka storage; the module extracts PEM
// from the supplied CA internally, so callers pass the same PKCS#12 profile
// shape as the other registries.
//
// Session-cached for the same reason ConfluentSchemaRegistry is — a
// `+cache="never"` directive here would mint a brand-new registry service
// for every chained client call.
//
// +cache="session"
func (k *Kafka) KarapaceSchemaRegistry(
	ctx context.Context,
	cluster *Cluster,
	// +default="ghcr.io"
	registry string,
	// +default="6.1.4"
	tag string,
	security *SchemaRegistrySecurity,
) (*SchemaRegistry, error) {
	if cluster == nil {
		return nil, fmt.Errorf("Kafka.KarapaceSchemaRegistry: cluster must not be nil")
	}
	if len(cluster.BrokerSvcs) == 0 {
		return nil, fmt.Errorf("Kafka.KarapaceSchemaRegistry: cluster has no brokers")
	}
	if err := validateRegistrySecurity("Kafka.KarapaceSchemaRegistry", security, cluster.ClientSecurityMode); err != nil {
		return nil, err
	}
	tls := security.Mode != "PLAINTEXT"

	image := fmt.Sprintf("%s/aiven-open/karapace:%s", registry, tag)
	// `ksr-` (karapace schema registry) keeps the hostname short — a longer
	// alias trips runc's sethostname on startup — and stays distinct from the
	// `csr-` / `asr-` prefixes the sibling registry constructors use on the
	// same cluster.
	srHost := "ksr-" + clusterHostSuffix(cluster.ClusterID)

	// Karapace creates the `_schemas` topic itself on first boot; its default
	// replication factor would fail on the single-broker clusters tests use —
	// pin it to the broker count, capped at 3, mirroring the cp-schema-registry
	// handling above and buildKafkaCluster's system-topic RF handling.
	rf := len(cluster.BrokerSvcs)
	if rf > 3 {
		rf = 3
	}

	// Karapace wants bare host:port bootstrap servers (no scheme prefix,
	// unlike cp-schema-registry).
	ctr := dag.Container().
		From(image).
		WithEnvVariable("KARAPACE_BOOTSTRAP_URI", strings.Join(cluster.BootstrapServers(), ",")).
		// Select the schema-registry role explicitly, mirroring Karapace's
		// own container/compose.yml (the REST proxy is the other role).
		WithEnvVariable("KARAPACE_KARAPACE_REGISTRY", "true").
		WithEnvVariable("KARAPACE_HOST", "0.0.0.0").
		WithEnvVariable("KARAPACE_PORT", strconv.Itoa(schemaRegistryPort)).
		WithEnvVariable("KARAPACE_ADVERTISED_HOSTNAME", srHost).
		WithEnvVariable("KARAPACE_REPLICATION_FACTOR", strconv.Itoa(rf)).
		// The 6.1.4 image ships a HEALTHCHECK that runs
		// `python3 healthcheck.py http://0.0.0.0:8081/_health` — the dial
		// target is the wildcard bind address (KARAPACE_HOST), which the
		// healthcheck script cannot connect to from inside the container,
		// so Dagger marks asService failed after the 60s start-period +
		// retries (≈90s) regardless of whether uvicorn is up. Dagger
		// prefers a Dockerfile HEALTHCHECK over its port probe when both
		// exist (see dagger v0.20.8 core/service.go:572-583), so drop the
		// broken image healthcheck and let the port probe verify 8081
		// instead.
		WithoutDockerHealthcheck().
		WithExposedPort(schemaRegistryPort)
	if tls {
		var err error
		ctr, err = applyKarapaceSchemaRegistryTls(ctx, ctr, security, srHost)
		if err != nil {
			return nil, err
		}
	}
	ctr = cluster.BindBrokers(ctr)

	// Karapace's production image ships no ENTRYPOINT/CMD. The schema
	// registry role runs as `python3 -m karapace`; the REST proxy is the
	// other role. Set it as the container entrypoint so AsService boots it
	// via UseEntrypoint, consistent with the sibling registry constructors.
	srSvc := ctr.
		WithEntrypoint([]string{"python3", "-m", "karapace"}).
		AsService(dagger.ContainerAsServiceOpts{UseEntrypoint: true}).
		WithHostname(srHost)

	return &SchemaRegistry{
		SchemaRegistrySvc: srSvc,
		AdvertisedHost:    srHost,
		AdvertisedPort:    schemaRegistryPort,
		SecurityMode:      security.Mode,
	}, nil
}

// Endpoint returns the host:port other containers (and the module runtime)
// can reach the Schema Registry REST API on.
//
// +cache="never"
func (s *SchemaRegistry) Endpoint(ctx context.Context) (string, error) {
	if s == nil || s.SchemaRegistrySvc == nil {
		return "", fmt.Errorf("SchemaRegistry.Endpoint: no underlying service")
	}
	return s.SchemaRegistrySvc.Endpoint(ctx, dagger.ServiceEndpointOpts{
		Port: s.AdvertisedPort,
	})
}

// BindTo attaches the Schema Registry service to the given container under
// the same hostname Endpoint reports, so the container resolves the
// registry at that address.
//
// +cache="never"
func (s *SchemaRegistry) BindTo(ctr *dagger.Container) *dagger.Container {
	return ctr.WithServiceBinding(s.AdvertisedHost, s.SchemaRegistrySvc)
}

// Client returns a typed HTTP client targeting this registry's REST API. The
// URL scheme (http vs https) follows the registry's own security mode; the
// supplied security profile must match it (a TLS/mTLS registry needs a TLS/mTLS
// client profile, verified when the first request runs). No I/O happens at
// construction time.
func (s *SchemaRegistry) Client(security *SchemaRegistryClientSecurity) *SchemaRegistryClient {
	scheme := "http"
	if s.SecurityMode == "TLS" || s.SecurityMode == "MTLS" {
		scheme = "https"
	}
	c := &SchemaRegistryClient{
		Svc:          s.SchemaRegistrySvc,
		BaseURL:      fmt.Sprintf("%s://%s:%d%s", scheme, s.AdvertisedHost, s.AdvertisedPort, s.BasePath),
		SecurityMode: "PLAINTEXT",
	}
	if security != nil {
		c.SecurityMode = security.Mode
		c.TrustStore = security.TrustStore
		c.TrustStorePassword = security.TrustStorePassword
		c.KeyStore = security.KeyStore
		c.KeyStorePassword = security.KeyStorePassword
	}
	return c
}

// Stop tears down the Schema Registry service. Kill is set so the stop
// returns immediately rather than waiting on graceful shutdown, mirroring
// Cluster.Stop — tests should call this in a defer.
//
// For a bundled registry (Bundled == true) the service is shared with the
// owning cluster, so Stop is a no-op: the cluster owns that lifecycle and
// stopping it here would tear the whole cluster down. Callers that uniformly
// `defer sr.Stop(ctx)` stay safe regardless of which registry they hold.
//
// +cache="never"
func (s *SchemaRegistry) Stop(ctx context.Context) error {
	if s == nil || s.SchemaRegistrySvc == nil || s.Bundled {
		return nil
	}
	if _, err := s.SchemaRegistrySvc.Stop(ctx, dagger.ServiceStopOpts{Kill: true}); err != nil {
		return fmt.Errorf("stop schema registry: %w", err)
	}
	return nil
}

// srError is the error body the Schema Registry REST API returns on a
// non-2xx response.
type srError struct {
	ErrorCode int    `json:"error_code"`
	Message   string `json:"message"`
}

// do issues one HTTP request against the Schema Registry REST API. It first
// starts the registry service so the module runtime can resolve its
// hostname, then retries transient failures — connection refused while the
// HTTP listener is still coming up, and 5xx responses while the `_schemas`
// store is still bootstrapping — until a fixed deadline.
func (c *SchemaRegistryClient) do(ctx context.Context, method, path string, reqBody any) ([]byte, int, error) {
	if c.Svc == nil {
		return nil, 0, fmt.Errorf("schema registry client has no service")
	}
	https := strings.HasPrefix(c.BaseURL, "https://")
	if https && c.SecurityMode == "PLAINTEXT" {
		return nil, 0, fmt.Errorf("schema registry serves HTTPS but the client security is PLAINTEXT; pass a TLS or mTLS SchemaRegistryClientSecurity")
	}
	if !https && c.SecurityMode != "PLAINTEXT" {
		return nil, 0, fmt.Errorf("schema registry serves plain HTTP but the client security is %s; pass PlaintextSchemaRegistryClientSecurity", c.SecurityMode)
	}
	if _, err := c.Svc.Start(ctx); err != nil {
		return nil, 0, fmt.Errorf("start schema registry service: %w", err)
	}
	cfg, err := c.tlsConfig(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("build tls config: %w", err)
	}
	httpClient := http.DefaultClient
	if cfg != nil {
		httpClient = &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
	}

	var body []byte
	if reqBody != nil {
		var err error
		body, err = json.Marshal(reqBody)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal request body: %w", err)
		}
	}

	const (
		attempts = 30
		backoff  = time.Second
	)
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-time.After(backoff):
			}
		}

		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
		if err != nil {
			return nil, 0, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Accept", srContentType)
		if body != nil {
			req.Header.Set("Content-Type", srContentType)
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			// The registry container may still be starting its HTTP
			// listener; connection-level failures are retryable.
			var netErr net.Error
			if errors.As(err, &netErr) || isConnRefused(err) {
				lastErr = err
				continue
			}
			return nil, 0, fmt.Errorf("%s %s: %w", method, path, err)
		}
		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, resp.StatusCode, fmt.Errorf("read response body: %w", readErr)
		}

		// 5xx means the registry is up but not yet ready (store still
		// loading); retry. Everything else is a definitive answer.
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("%s %s: server returned %d: %s", method, path, resp.StatusCode, respBody)
			continue
		}
		return respBody, resp.StatusCode, nil
	}
	return nil, 0, fmt.Errorf("%s %s: schema registry not ready after %d attempts: %w", method, path, attempts, lastErr)
}

// tlsConfig materializes the client-side *tls.Config from the client's PKCS#12
// truststore (and, for mTLS, keystore), mirroring Client.tlsConfig on the
// franz-go path. Returns (nil, nil) for a plaintext registry client.
func (c *SchemaRegistryClient) tlsConfig(ctx context.Context) (*tls.Config, error) {
	if c.SecurityMode == "PLAINTEXT" {
		return nil, nil
	}
	if c.TrustStore == nil || c.TrustStorePassword == nil {
		return nil, fmt.Errorf("%s registry client requires a trust store and password", c.SecurityMode)
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
			return nil, fmt.Errorf("MTLS registry client requires a key store and password")
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

// isConnRefused reports whether err is a connection-refused dial failure,
// which happens while the registry's HTTP listener is still binding.
func isConnRefused(err error) bool {
	return err != nil && strings.Contains(err.Error(), "connection refused")
}

// decodeOK unmarshals a 2xx response body into out. A non-2xx status is
// turned into a Go error carrying the registry's error_code and message.
func decodeOK(respBody []byte, status string, statusCode int, out any) error {
	if statusCode < 200 || statusCode >= 300 {
		var se srError
		if json.Unmarshal(respBody, &se) == nil && se.Message != "" {
			return fmt.Errorf("schema registry: %s (code %d, http %d)", se.Message, se.ErrorCode, statusCode)
		}
		return fmt.Errorf("schema registry: %s returned http %d: %s", status, statusCode, respBody)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// validSchemaTypes is the set of schema types the registry accepts.
var validSchemaTypes = map[string]bool{"AVRO": true, "JSON": true, "PROTOBUF": true}

// validCompatLevels is the set of compatibility levels the registry accepts.
var validCompatLevels = map[string]bool{
	"NONE": true, "BACKWARD": true, "BACKWARD_TRANSITIVE": true,
	"FORWARD": true, "FORWARD_TRANSITIVE": true, "FULL": true, "FULL_TRANSITIVE": true,
}

// normalizeSchemaType maps the registry's omitted schemaType (it leaves the
// field empty for Avro, its default) back to an explicit "AVRO".
func normalizeSchemaType(t string) string {
	if t == "" {
		return "AVRO"
	}
	return t
}

// RegisterSchema registers schema under subject and returns the globally
// unique schema id the registry assigned. schemaType must be one of AVRO,
// JSON, or PROTOBUF.
//
// +cache="never"
func (c *SchemaRegistryClient) RegisterSchema(
	ctx context.Context,
	subject string,
	schema string,
	// +default="AVRO"
	schemaType string,
) (int, error) {
	if subject == "" {
		return 0, fmt.Errorf("RegisterSchema: subject must not be empty")
	}
	if !validSchemaTypes[schemaType] {
		return 0, fmt.Errorf("RegisterSchema: unsupported schemaType %q (want AVRO|JSON|PROTOBUF)", schemaType)
	}
	reqBody := map[string]string{"schema": schema, "schemaType": schemaType}
	body, code, err := c.do(ctx, http.MethodPost, "/subjects/"+url.PathEscape(subject)+"/versions", reqBody)
	if err != nil {
		return 0, err
	}
	var out struct {
		ID int `json:"id"`
	}
	if err := decodeOK(body, "register schema", code, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// LookupSchemaByID returns the schema registered under the given global id.
//
// The registry's GET /schemas/ids/{id} endpoint reports only the schema
// text and type, so a second call to GET /schemas/ids/{id}/versions
// resolves the subject and version. When an id maps to more than one
// subject/version pair, the first association is returned.
//
// +cache="never"
func (c *SchemaRegistryClient) LookupSchemaByID(ctx context.Context, id int) (RegisteredSchema, error) {
	body, code, err := c.do(ctx, http.MethodGet, "/schemas/ids/"+strconv.Itoa(id), nil)
	if err != nil {
		return RegisteredSchema{}, err
	}
	var schema struct {
		Schema     string `json:"schema"`
		SchemaType string `json:"schemaType"`
	}
	if err := decodeOK(body, "lookup schema by id", code, &schema); err != nil {
		return RegisteredSchema{}, err
	}

	versBody, versCode, err := c.do(ctx, http.MethodGet, "/schemas/ids/"+strconv.Itoa(id)+"/versions", nil)
	if err != nil {
		return RegisteredSchema{}, err
	}
	var versions []struct {
		Subject string `json:"subject"`
		Version int    `json:"version"`
	}
	if err := decodeOK(versBody, "lookup schema versions", versCode, &versions); err != nil {
		return RegisteredSchema{}, err
	}
	if len(versions) == 0 {
		return RegisteredSchema{}, fmt.Errorf("LookupSchemaByID: id %d has no subject/version associations", id)
	}

	return RegisteredSchema{
		Subject:    versions[0].Subject,
		Version:    versions[0].Version,
		SchemaID:   id,
		Definition: schema.Schema,
		SchemaType: normalizeSchemaType(schema.SchemaType),
	}, nil
}

// LookupLatestBySubject returns the latest registered schema version for
// the given subject.
//
// +cache="never"
func (c *SchemaRegistryClient) LookupLatestBySubject(ctx context.Context, subject string) (RegisteredSchema, error) {
	if subject == "" {
		return RegisteredSchema{}, fmt.Errorf("LookupLatestBySubject: subject must not be empty")
	}
	body, code, err := c.do(ctx, http.MethodGet, "/subjects/"+url.PathEscape(subject)+"/versions/latest", nil)
	if err != nil {
		return RegisteredSchema{}, err
	}
	var out struct {
		Subject    string `json:"subject"`
		Version    int    `json:"version"`
		ID         int    `json:"id"`
		Schema     string `json:"schema"`
		SchemaType string `json:"schemaType"`
	}
	if err := decodeOK(body, "lookup latest by subject", code, &out); err != nil {
		return RegisteredSchema{}, err
	}
	return RegisteredSchema{
		Subject:    out.Subject,
		Version:    out.Version,
		SchemaID:   out.ID,
		Definition: out.Schema,
		SchemaType: normalizeSchemaType(out.SchemaType),
	}, nil
}

// ListSubjects returns the names of every subject registered.
//
// +cache="never"
func (c *SchemaRegistryClient) ListSubjects(ctx context.Context) ([]string, error) {
	body, code, err := c.do(ctx, http.MethodGet, "/subjects", nil)
	if err != nil {
		return nil, err
	}
	var out []string
	if err := decodeOK(body, "list subjects", code, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteSubject deletes every version of the given subject and returns the
// version numbers that were deleted.
//
// +cache="never"
func (c *SchemaRegistryClient) DeleteSubject(ctx context.Context, subject string) ([]int, error) {
	if subject == "" {
		return nil, fmt.Errorf("DeleteSubject: subject must not be empty")
	}
	body, code, err := c.do(ctx, http.MethodDelete, "/subjects/"+url.PathEscape(subject), nil)
	if err != nil {
		return nil, err
	}
	var out []int
	if err := decodeOK(body, "delete subject", code, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SetCompatibility sets the compatibility level for the given subject.
// level must be one of NONE, BACKWARD, BACKWARD_TRANSITIVE, FORWARD,
// FORWARD_TRANSITIVE, FULL, or FULL_TRANSITIVE.
//
// +cache="never"
func (c *SchemaRegistryClient) SetCompatibility(ctx context.Context, subject string, level string) error {
	if subject == "" {
		return fmt.Errorf("SetCompatibility: subject must not be empty")
	}
	if !validCompatLevels[level] {
		return fmt.Errorf("SetCompatibility: unsupported level %q", level)
	}
	body, code, err := c.do(ctx, http.MethodPut, "/config/"+url.PathEscape(subject),
		map[string]string{"compatibility": level})
	if err != nil {
		return err
	}
	return decodeOK(body, "set compatibility", code, nil)
}

// GetCompatibility returns the compatibility level configured for the given
// subject, falling back to the registry-wide default when the subject has
// no explicit configuration.
//
// +cache="never"
func (c *SchemaRegistryClient) GetCompatibility(ctx context.Context, subject string) (string, error) {
	if subject == "" {
		return "", fmt.Errorf("GetCompatibility: subject must not be empty")
	}
	body, code, err := c.do(ctx, http.MethodGet,
		"/config/"+url.PathEscape(subject)+"?defaultToGlobal=true", nil)
	if err != nil {
		return "", err
	}
	var out struct {
		CompatibilityLevel string `json:"compatibilityLevel"`
	}
	if err := decodeOK(body, "get compatibility", code, &out); err != nil {
		return "", err
	}
	return out.CompatibilityLevel, nil
}
