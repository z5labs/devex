// Kafka provides Dagger functions for spinning up KRaft Kafka clusters from
// the apache/kafka-native image and a pure-Go franz-go client that targets
// either the local cluster or any reachable remote cluster.
//
// Plaintext is the only security mechanism supported in this story; TLS /
// mTLS lands in a follow-up.
package main

import (
	"context"
	"crypto/sha256"
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
)

type Kafka struct{}

// ServerSecurity describes how a Kafka cluster's listener authenticates and
// encrypts traffic from clients. Only PLAINTEXT is supported in this story.
type ServerSecurity struct {
	// +private
	Mode string
}

// ClientSecurity describes how a franz-go client authenticates to a Kafka
// broker. Only PLAINTEXT is supported in this story.
type ClientSecurity struct {
	// +private
	Mode string
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
// unencrypted, unauthenticated traffic.
func (k *Kafka) PlaintextServerSecurity() *ServerSecurity {
	return &ServerSecurity{Mode: "PLAINTEXT"}
}

// PlaintextClientSecurity returns a ClientSecurity profile configured for
// unencrypted, unauthenticated traffic.
func (k *Kafka) PlaintextClientSecurity() *ClientSecurity {
	return &ClientSecurity{Mode: "PLAINTEXT"}
}

// Cluster spins up a KRaft Kafka cluster of the requested size with
// dedicated controller and broker containers.
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
// +cache="never"
func (k *Kafka) Cluster(
	ctx context.Context,
	clusterId string,
	// +default=1
	controllers int,
	// +default=1
	brokers int,
	// +default="docker.io"
	registry string,
	tag string,
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
	if clientListenerSecurity == nil || clientListenerSecurity.Mode != "PLAINTEXT" {
		return nil, fmt.Errorf("only PLAINTEXT clientListenerSecurity is supported")
	}
	if raw, err := base64.RawURLEncoding.DecodeString(clusterId); err != nil || len(raw) != 16 {
		return nil, fmt.Errorf(
			"clusterId must be 16 bytes encoded as 22 unpadded base64-url chars, got %q",
			clusterId,
		)
	}

	image := fmt.Sprintf("%s/apache/kafka-native:%s", registry, tag)

	const (
		controllerAlias = "controller-1"
		quorumVoters    = "1@" + controllerAlias + ":9093"
	)

	ctrlCtr := dag.Container().
		From(image).
		WithEnvVariable("KAFKA_NODE_ID", "1").
		WithEnvVariable("KAFKA_PROCESS_ROLES", "controller").
		WithEnvVariable("KAFKA_LISTENERS", "CONTROLLER://0.0.0.0:9093").
		WithEnvVariable("KAFKA_CONTROLLER_LISTENER_NAMES", "CONTROLLER").
		WithEnvVariable("KAFKA_LISTENER_SECURITY_PROTOCOL_MAP", "CONTROLLER:PLAINTEXT").
		WithEnvVariable("KAFKA_CONTROLLER_QUORUM_VOTERS", quorumVoters).
		WithEnvVariable("CLUSTER_ID", clusterId).
		WithExposedPort(9093)
	ctrlSvc := ctrlCtr.AsService(dagger.ContainerAsServiceOpts{UseEntrypoint: true})

	brokerSvcs := make([]*dagger.Service, brokers)
	brokerHosts := make([]string, brokers)
	for i := 0; i < brokers; i++ {
		nodeID := 100 + i
		brkCtr := dag.Container().
			From(image).
			WithServiceBinding(controllerAlias, ctrlSvc).
			WithEnvVariable("KAFKA_NODE_ID", fmt.Sprintf("%d", nodeID)).
			WithEnvVariable("KAFKA_PROCESS_ROLES", "broker").
			WithEnvVariable("KAFKA_LISTENERS", "PLAINTEXT://0.0.0.0:19092,PLAINTEXT_HOST://0.0.0.0:9092").
			WithEnvVariable("KAFKA_INTER_BROKER_LISTENER_NAME", "PLAINTEXT").
			WithEnvVariable("KAFKA_CONTROLLER_LISTENER_NAMES", "CONTROLLER").
			WithEnvVariable("KAFKA_LISTENER_SECURITY_PROTOCOL_MAP", "CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT,PLAINTEXT_HOST:PLAINTEXT").
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
			WithExposedPort(19092).
			WithEntrypoint([]string{"sh", "-c"}).
			WithDefaultArgs([]string{
				`export KAFKA_ADVERTISED_LISTENERS="PLAINTEXT://$(hostname):19092,PLAINTEXT_HOST://$(hostname):9092" && exec /etc/kafka/docker/run`,
			})

		svc := brkCtr.AsService(dagger.ContainerAsServiceOpts{UseEntrypoint: true})
		host, err := svc.Hostname(ctx)
		if err != nil {
			return nil, fmt.Errorf("get broker %d hostname: %w", i, err)
		}
		brokerSvcs[i] = svc
		brokerHosts[i] = host
	}

	return &Cluster{
		ClusterID:          clusterId,
		BrokerSvcs:         brokerSvcs,
		BrokerHosts:        brokerHosts,
		ClientSecurityMode: clientListenerSecurity.Mode,
	}, nil
}

// Client constructs a franz-go-backed Kafka client that targets the given
// bootstrap servers. No I/O happens at construction time.
func (k *Kafka) Client(bootstrapServers []string, security *ClientSecurity) *Client {
	mode := "PLAINTEXT"
	if security != nil {
		mode = security.Mode
	}
	return &Client{
		Bootstrap:    bootstrapServers,
		SecurityMode: mode,
	}
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
	mode := "PLAINTEXT"
	if security != nil {
		mode = security.Mode
	}
	return &Client{
		Bootstrap:    c.BootstrapServers(),
		SecurityMode: mode,
	}, nil
}

// Client is a franz-go-backed Kafka client. Each method opens a fresh
// connection so the function call is stateless from Dagger's perspective.
type Client struct {
	// +private
	Bootstrap []string
	// +private
	SecurityMode string
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
// `client.properties` file (bootstrap.servers + security.protocol) so callers
// can hand it to the Apache Kafka command-line tools or to other JVM-based
// consumers.
//
// +cache="never"
func (c *Client) PropertiesFile() (*dagger.File, error) {
	content := []byte(fmt.Sprintf(
		"bootstrap.servers=%s\nsecurity.protocol=%s\n",
		strings.Join(c.Bootstrap, ","),
		c.SecurityMode,
	))

	sum := sha256.Sum256(content)
	dir := "props-" + hex.EncodeToString(sum[:])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %q: %w", dir, err)
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
// servers. Callers are responsible for Close.
func (c *Client) newKgoClient(extra ...kgo.Opt) (*kgo.Client, error) {
	opts := []kgo.Opt{kgo.SeedBrokers(c.Bootstrap...)}
	opts = append(opts, extra...)
	return kgo.NewClient(opts...)
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
	cl, err := c.newKgoClient()
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
	cl, err := c.newKgoClient()
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

	cl, err := c.newKgoClient()
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
	cl, err := c.newKgoClient(opts...)
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
	cl, err := c.newKgoClient()
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
