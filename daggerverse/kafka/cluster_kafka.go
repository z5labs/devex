package main

import (
	"context"
	"encoding/base64"
	"fmt"

	"dagger/kafka/internal/dagger"
)

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
