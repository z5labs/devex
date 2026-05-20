package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
	"github.com/twmb/franz-go/pkg/sr"
	"software.sslmate.com/src/go-pkcs12"
)

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
// value already encoded into the requested string representation. KeySchemaID
// and ValueSchemaID are populated when Consume runs with schemaRegistryAware
// and the corresponding record bytes carry the Confluent wire-format header
// (`0x00 || uint32be(schemaID) || payload`); otherwise they are 0 and the
// key/value bytes pass through untouched.
type ConsumedRecord struct {
	Key           string
	Value         string
	KeySchemaID   int
	ValueSchemaID int
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

// canonicalJSON parses b as a JSON value and returns the encoding/json
// canonical re-marshal of it (compact, alphabetically-keyed objects). field
// is "key" or "value" so the error message identifies which side failed.
// Note: bare scalars (null, 42, "foo", true) are accepted — the contract is
// "is this valid JSON?", not "is this a JSON object?".
//
// UseNumber preserves arbitrary-precision numeric tokens as json.Number
// instead of float64, so integers above 2^53 and high-precision decimals
// round-trip byte-for-byte rather than being silently coerced. Encoder
// SetEscapeHTML(false) keeps "<", ">", and "&" in string values
// verbatim rather than rewriting them as < / > / &, which
// would otherwise mutate caller-supplied payloads.
func canonicalJSON(b []byte, field string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", field, err)
	}
	// json.Decoder accepts a stream of values — reject trailing tokens
	// so the caller can't smuggle extra documents through the validator.
	if dec.More() {
		return nil, fmt.Errorf("%s has trailing data after JSON value", field)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("re-marshal %s as canonical JSON: %w", field, err)
	}
	// json.Encoder appends a trailing newline; strip it so the canonical
	// form matches what json.Marshal would have produced.
	out := buf.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out, nil
}

// applySerializeAs runs the named producer-side serializer. "" is
// pass-through; "JSON" canonicalises via canonicalJSON; anything else is
// rejected so a typo never silently degrades to pass-through.
func applySerializeAs(b []byte, mode, field string) ([]byte, error) {
	switch mode {
	case "":
		return b, nil
	case "JSON":
		return canonicalJSON(b, field)
	default:
		return nil, fmt.Errorf("unsupported %sSerializeAs %q (want \"\"|\"JSON\")", field, mode)
	}
}

// applyDeserializeAs runs the named consumer-side deserializer against
// payload bytes that have already had any Confluent frame stripped. "" is
// pass-through; "JSON" validates with json.Valid. The bytes are returned
// unchanged on success — consumers control rendering via keyEncoding /
// valueEncoding, not via the deserialize step.
func applyDeserializeAs(b []byte, mode, field string) ([]byte, error) {
	switch mode {
	case "":
		return b, nil
	case "JSON":
		if !json.Valid(b) {
			return nil, fmt.Errorf("%s is not valid JSON", field)
		}
		return b, nil
	default:
		return nil, fmt.Errorf("unsupported %sDeserializeAs %q (want \"\"|\"JSON\")", field, mode)
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
// keySchemaID / valueSchemaID, when positive, prepend the Confluent
// Schema Registry wire-format header to the corresponding field:
// `0x00 || uint32be(schemaID) || payload`. The header is laid down via
// franz-go's sr.ConfluentHeader so the byte layout matches what
// Schema-Registry-aware consumers expect. Default 0 means no framing
// (the field is sent verbatim). Negative IDs are rejected.
//
// keySerializeAs / valueSerializeAs, when set to "JSON", parse the
// corresponding decoded bytes with encoding/json and re-marshal to the
// canonical form before any framing is applied. The pipeline order is
// decode → serialize → frame, so a single call can both canonicalise a
// JSON payload and prepend the Confluent header. Invalid JSON is
// rejected before any broker I/O. Default "" is pass-through; "JSON"
// is the only non-empty value accepted in this story.
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
	// +default=0
	keySchemaID int,
	// +default=0
	valueSchemaID int,
	// +default=""
	keySerializeAs string,
	// +default=""
	valueSerializeAs string,
) error {
	if keySchemaID < 0 {
		return fmt.Errorf("keySchemaID must be >= 0, got %d", keySchemaID)
	}
	if valueSchemaID < 0 {
		return fmt.Errorf("valueSchemaID must be >= 0, got %d", valueSchemaID)
	}

	keyBytes, err := decodeString(key, keyEncoding)
	if err != nil {
		return fmt.Errorf("decode key: %w", err)
	}
	valBytes, err := decodeString(value, valueEncoding)
	if err != nil {
		return fmt.Errorf("decode value: %w", err)
	}

	keyBytes, err = applySerializeAs(keyBytes, keySerializeAs, "key")
	if err != nil {
		return fmt.Errorf("serialize key: %w", err)
	}
	valBytes, err = applySerializeAs(valBytes, valueSerializeAs, "value")
	if err != nil {
		return fmt.Errorf("serialize value: %w", err)
	}

	var hdr sr.ConfluentHeader
	if keySchemaID > 0 {
		framed, err := hdr.AppendEncode(nil, keySchemaID, nil)
		if err != nil {
			return fmt.Errorf("frame key with schema id %d: %w", keySchemaID, err)
		}
		keyBytes = append(framed, keyBytes...)
	}
	if valueSchemaID > 0 {
		framed, err := hdr.AppendEncode(nil, valueSchemaID, nil)
		if err != nil {
			return fmt.Errorf("frame value with schema id %d: %w", valueSchemaID, err)
		}
		valBytes = append(framed, valBytes...)
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
// When schemaRegistryAware is true, each record's key and value are
// inspected for the Confluent Schema Registry wire-format header
// (`0x00 || uint32be(schemaID) || payload`). When present, the 5-byte
// header is stripped before encoding the payload and the extracted
// schema ID is surfaced on ConsumedRecord.KeySchemaID /
// ConsumedRecord.ValueSchemaID. Unframed fields pass through with a
// zero schema ID. When false (the default), bytes are returned verbatim
// and the schema ID fields are always zero.
//
// keyDeserializeAs / valueDeserializeAs, when set to "JSON", validate
// each consumed record's post-frame-strip payload bytes via
// encoding/json's json.Valid. The pipeline order is unframe →
// deserialize → encode, so SchemaRegistryAware and a JSON deserializer
// compose: framed bytes are stripped first and validation runs on the
// payload alone. Records whose payloads fail to parse cause Consume to
// error out and abandon the remaining poll. Default "" is
// pass-through; "JSON" is the only non-empty value accepted in this
// story.
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
	// +default=false
	schemaRegistryAware bool,
	// +default=""
	keyDeserializeAs string,
	// +default=""
	valueDeserializeAs string,
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
			keyRaw, valRaw := r.Key, r.Value
			var keyID, valID int
			if schemaRegistryAware {
				var hdr sr.ConfluentHeader
				if id, payload, err := hdr.DecodeID(keyRaw); err == nil {
					keyID, keyRaw = id, payload
				}
				if id, payload, err := hdr.DecodeID(valRaw); err == nil {
					valID, valRaw = id, payload
				}
			}
			var err error
			keyRaw, err = applyDeserializeAs(keyRaw, keyDeserializeAs, "key")
			if err != nil {
				return nil, fmt.Errorf("deserialize key: %w", err)
			}
			valRaw, err = applyDeserializeAs(valRaw, valueDeserializeAs, "value")
			if err != nil {
				return nil, fmt.Errorf("deserialize value: %w", err)
			}
			keyStr, err := encodeBytes(keyRaw, keyEncoding)
			if err != nil {
				return nil, fmt.Errorf("encode key: %w", err)
			}
			valStr, err := encodeBytes(valRaw, valueEncoding)
			if err != nil {
				return nil, fmt.Errorf("encode value: %w", err)
			}
			out = append(out, ConsumedRecord{
				Key:           keyStr,
				Value:         valStr,
				KeySchemaID:   keyID,
				ValueSchemaID: valID,
			})
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
