// Command kafka-consumer is a reference Kafka consumer that ties together the
// z5labs devex daggerverse modules end to end:
//
//   - it consumes Avro records from a topic with a franz-go consumer-group
//     client (github.com/twmb/franz-go),
//   - it resolves each record's writer schema from a Confluent-compatible
//     Schema Registry *by id* (the Confluent wire format: a magic byte, a
//     4-byte big-endian schema id, then the Avro binary body) and decodes it
//     with github.com/z5labs/avro-go,
//   - every hop is TLS-encrypted — the broker dial and the Schema Registry
//     HTTPS calls both verify against a CA truststore, and supplying a client
//     keystore upgrades both hops to mTLS; there is no plaintext code path, and
//   - it emits OpenTelemetry traces, metrics, and logs via the franz-go kotel
//     plugin plus the OTel Go SDK, exported over OTLP/gRPC to
//     OTEL_EXPORTER_OTLP_ENDPOINT.
//
// The app is intentionally a single package main driven by flags/env — the
// "production-ready" bar in this example is about how it integrates (TLS,
// Schema Registry, OpenTelemetry) and how it is built and exercised via Dagger,
// not about application-code architecture.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sr"
	"github.com/twmb/franz-go/plugin/kotel"
	"github.com/z5labs/avro-go"
	"github.com/z5labs/avro-go/generic"
	otellog "go.opentelemetry.io/otel/log"
	logglobal "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "kafka-consumer:", err)
		os.Exit(1)
	}
}

// config is the flag/env surface. Endpoints and cert paths come from flags that
// default to their matching env var, so the same binary is driven identically
// by `-flag value` locally and by env vars from the Dagger harness. Secrets
// (the keystore/truststore passwords) are read from the environment only, never
// from a flag, so they don't leak into `ps`.
type config struct {
	brokers       []string
	topic         string
	group         string
	registryURL   string
	truststore    string
	truststorePwd string
	keystore      string // when set, both hops use mTLS
	keystorePwd   string
	maxRecords    int
	timeout       time.Duration
}

func loadConfig(args []string) (config, error) {
	fs := flag.NewFlagSet("kafka-consumer", flag.ContinueOnError)
	brokers := fs.String("brokers", os.Getenv("BROKERS"), "comma-separated host:port Kafka bootstrap brokers")
	topic := fs.String("topic", os.Getenv("TOPIC"), "topic to consume")
	group := fs.String("group", envOr("GROUP", "kafka-consumer"), "consumer group id")
	registryURL := fs.String("registry-url", os.Getenv("REGISTRY_URL"), "Schema Registry base URL (https://host:port)")
	truststore := fs.String("truststore", os.Getenv("TRUSTSTORE"), "path to the PKCS#12 CA truststore (TLS is mandatory)")
	keystore := fs.String("keystore", os.Getenv("KEYSTORE"), "path to a PKCS#12 client keystore; when set, both hops use mTLS")
	maxRecords := fs.Int("max-records", envInt("MAX_RECORDS", 1), "consume this many records, flush telemetry, then exit 0")
	timeout := fs.String("timeout", envOr("TIMEOUT", "30s"), "overall consume deadline; exit non-zero if max-records is not reached")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	d, err := time.ParseDuration(*timeout)
	if err != nil {
		return config{}, fmt.Errorf("parse -timeout %q: %w", *timeout, err)
	}

	cfg := config{
		brokers:       splitAndTrim(*brokers),
		topic:         *topic,
		group:         *group,
		registryURL:   strings.TrimRight(*registryURL, "/"),
		truststore:    *truststore,
		truststorePwd: os.Getenv("TRUSTSTORE_PASSWORD"),
		keystore:      *keystore,
		keystorePwd:   os.Getenv("KEYSTORE_PASSWORD"),
		maxRecords:    *maxRecords,
		timeout:       d,
	}

	switch {
	case len(cfg.brokers) == 0:
		return config{}, errors.New("no brokers configured (set -brokers or BROKERS)")
	case cfg.topic == "":
		return config{}, errors.New("no topic configured (set -topic or TOPIC)")
	case cfg.registryURL == "":
		return config{}, errors.New("no schema registry configured (set -registry-url or REGISTRY_URL)")
	case !strings.HasPrefix(cfg.registryURL, "https://"):
		// TLS everywhere: the registry must be reached over HTTPS.
		return config{}, fmt.Errorf("registry URL must be https:// (got %q) — this example has no plaintext path", cfg.registryURL)
	case cfg.truststore == "" || cfg.truststorePwd == "":
		return config{}, errors.New("a CA truststore is mandatory (set -truststore/TRUSTSTORE and TRUSTSTORE_PASSWORD)")
	case cfg.keystore != "" && cfg.keystorePwd == "":
		return config{}, errors.New("KEYSTORE_PASSWORD is required when a client keystore is set")
	case cfg.maxRecords <= 0:
		return config{}, fmt.Errorf("-max-records must be > 0, got %d", cfg.maxRecords)
	}
	return cfg, nil
}

func run(ctx context.Context) error {
	cfg, err := loadConfig(os.Args[1:])
	if err != nil {
		return err
	}

	// One TLS config serves both hops: the same CA roots verify the broker and
	// the registry, and the same optional client leaf authenticates to both.
	tlsCfg, err := buildTLSConfig(cfg)
	if err != nil {
		return fmt.Errorf("build tls config: %w", err)
	}

	// OpenTelemetry: traces + metrics + logs over OTLP/gRPC. The exporters read
	// OTEL_EXPORTER_OTLP_ENDPOINT / _INSECURE and the resource reads
	// OTEL_SERVICE_NAME from the environment.
	shutdown, tp, mp, err := setupOTel(ctx)
	if err != nil {
		return fmt.Errorf("setup opentelemetry: %w", err)
	}
	// Flush-before-exit: this bounded run would otherwise drop its telemetry
	// when the process exits right after the last record.
	defer func() { _ = shutdown(context.Background()) }()

	logger := logglobal.GetLoggerProvider().Logger("kafka-consumer")

	// kotel wires franz-go's client into OTel: client metrics + a fetch span
	// per poll. tracer is kept for the per-record process span below.
	kt, tracer := newKotel(tp, mp)

	cl, err := newClient(cfg, tlsCfg, kt)
	if err != nil {
		return fmt.Errorf("new kafka client: %w", err)
	}
	defer cl.Close()

	schemas := newSchemaResolver(cfg.registryURL, tlsCfg)

	deadlineCtx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()

	consumed := 0
	for consumed < cfg.maxRecords {
		fetches := cl.PollFetches(deadlineCtx)
		if err := fetchErr(fetches); err != nil {
			return fmt.Errorf("consumed %d/%d records before error: %w", consumed, cfg.maxRecords, err)
		}
		iter := fetches.RecordIter()
		for !iter.Done() && consumed < cfg.maxRecords {
			if err := processRecord(deadlineCtx, iter.Next(), schemas, tracer, logger); err != nil {
				return err
			}
			consumed++
		}
	}
	return nil
}

// processRecord opens a per-record consume span, resolves the record's writer
// schema by the id in its Confluent wire header, Avro-decodes the body, prints
// one JSON line to stdout, and emits an OTel log record for the same event.
func processRecord(ctx context.Context, rec *kgo.Record, schemas *schemaResolver, tracer *kotel.Tracer, logger otellog.Logger) error {
	// WithProcessSpan parents the span from the record's propagated trace
	// context; re-attach it to our deadline-bound ctx so downstream calls
	// (schema fetch, log emit) are both traced and bounded by -timeout.
	_, span := tracer.WithProcessSpan(rec)
	defer span.End()
	ctx = trace.ContextWithSpan(ctx, span)

	var hdr sr.ConfluentHeader
	id, body, err := hdr.DecodeID(rec.Value)
	if err != nil {
		return fmt.Errorf("decode confluent header (offset %d): %w", rec.Offset, err)
	}

	value, err := schemas.decode(ctx, id, body)
	if err != nil {
		return fmt.Errorf("decode record (offset %d, schema %d): %w", rec.Offset, id, err)
	}

	line, err := json.Marshal(map[string]any{
		"topic":     rec.Topic,
		"partition": rec.Partition,
		"offset":    rec.Offset,
		"schemaId":  id,
		"value":     value,
	})
	if err != nil {
		return fmt.Errorf("marshal decoded record: %w", err)
	}
	fmt.Println(string(line))
	emitLog(ctx, logger, string(line))
	return nil
}

// fetchErr collapses a fetch batch's errors into one, ignoring the benign
// deadline/cancel that ends the bounded poll loop.
func fetchErr(fetches kgo.Fetches) error {
	var errs []error
	for _, e := range fetches.Errors() {
		if errors.Is(e.Err, context.DeadlineExceeded) || errors.Is(e.Err, context.Canceled) {
			continue
		}
		errs = append(errs, fmt.Errorf("topic %q partition %d: %w", e.Topic, e.Partition, e.Err))
	}
	return errors.Join(errs...)
}

// newClient opens a franz-go consumer-group client that dials brokers over TLS
// (kgo.DialTLSConfig) and reports telemetry through the kotel hooks.
func newClient(cfg config, tlsCfg *tls.Config, kt *kotel.Kotel) (*kgo.Client, error) {
	return kgo.NewClient(
		kgo.SeedBrokers(cfg.brokers...),
		kgo.DialTLSConfig(tlsCfg),
		kgo.ConsumeTopics(cfg.topic),
		kgo.ConsumerGroup(cfg.group),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		// The example re-reads from the start on every run; committing offsets
		// would defeat that and isn't needed to demonstrate the integration.
		kgo.DisableAutoCommit(),
		kgo.WithHooks(kt.Hooks()...),
	)
}

// newKotel builds the kotel plugin (client metrics + fetch spans) and returns
// its tracer so callers can open per-record process spans.
func newKotel(tp trace.TracerProvider, mp metric.MeterProvider) (*kotel.Kotel, *kotel.Tracer) {
	tracer := kotel.NewTracer(kotel.TracerProvider(tp))
	meter := kotel.NewMeter(kotel.MeterProvider(mp))
	return kotel.NewKotel(kotel.WithTracer(tracer), kotel.WithMeter(meter)), tracer
}

// emitLog records the consumed event as an OTel log so it reaches the collector
// (and, in the Dagger integration test, Loki) alongside the traces and metrics.
func emitLog(ctx context.Context, logger otellog.Logger, body string) {
	var rec otellog.Record
	rec.SetTimestamp(time.Now())
	rec.SetSeverity(otellog.SeverityInfo)
	rec.SetSeverityText("INFO")
	rec.SetBody(otellog.StringValue(body))
	logger.Emit(ctx, rec)
}

// buildTLSConfig materializes the client *tls.Config from the PKCS#12 CA
// truststore (mandatory) and, when a client keystore is supplied, the client
// leaf for mTLS. It mirrors the kafka module's own client (daggerverse/kafka/
// client.go). Adapting to PEM material means swapping the two pkcs12.Decode*
// calls for x509.AppendCertsFromPEM / tls.LoadX509KeyPair.
func buildTLSConfig(cfg config) (*tls.Config, error) {
	tsBytes, err := os.ReadFile(cfg.truststore)
	if err != nil {
		return nil, fmt.Errorf("read truststore: %w", err)
	}
	roots, err := pkcs12.DecodeTrustStore(tsBytes, cfg.truststorePwd)
	if err != nil {
		return nil, fmt.Errorf("decode truststore: %w", err)
	}
	pool := x509.NewCertPool()
	for _, ca := range roots {
		pool.AddCert(ca)
	}
	out := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	if cfg.keystore != "" {
		ksBytes, err := os.ReadFile(cfg.keystore)
		if err != nil {
			return nil, fmt.Errorf("read keystore: %w", err)
		}
		priv, leaf, chain, err := pkcs12.DecodeChain(ksBytes, cfg.keystorePwd)
		if err != nil {
			return nil, fmt.Errorf("decode keystore: %w", err)
		}
		certBytes := [][]byte{leaf.Raw}
		for _, link := range chain {
			certBytes = append(certBytes, link.Raw)
		}
		out.Certificates = []tls.Certificate{{
			Certificate: certBytes,
			PrivateKey:  priv,
			Leaf:        leaf,
		}}
	}
	return out, nil
}

// schemaResolver resolves and memoizes Avro schemas by Schema Registry id over
// HTTPS, so a topic's records don't each pay a registry round-trip.
type schemaResolver struct {
	baseURL string
	http    *http.Client

	mu    sync.Mutex
	cache map[int]*resolvedSchema
}

type resolvedSchema struct {
	schema  avro.Schema
	names   *nameTable
	decoder *generic.Decoder
}

func newSchemaResolver(baseURL string, tlsCfg *tls.Config) *schemaResolver {
	return &schemaResolver{
		baseURL: baseURL,
		http:    &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}, Timeout: 15 * time.Second},
		cache:   make(map[int]*resolvedSchema),
	}
}

// decode resolves the schema for id (fetching + compiling it on first use) and
// returns the Avro-decoded body as a json.Marshal-ready Go value.
func (s *schemaResolver) decode(ctx context.Context, id int, body []byte) (any, error) {
	rs, err := s.schemaFor(ctx, id)
	if err != nil {
		return nil, err
	}
	val, err := rs.decoder.Decode(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("avro-decode: %w", err)
	}
	return jsonFromValue(val, rs.schema, rs.names, "")
}

func (s *schemaResolver) schemaFor(ctx context.Context, id int) (*resolvedSchema, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rs, ok := s.cache[id]; ok {
		return rs, nil
	}
	text, err := s.fetchSchema(ctx, id)
	if err != nil {
		return nil, err
	}
	schema, err := avro.ParseJSON([]byte(text))
	if err != nil {
		return nil, fmt.Errorf("parse schema id %d: %w", id, err)
	}
	dec, err := generic.NewDecoder(schema)
	if err != nil {
		return nil, fmt.Errorf("compile decoder for schema id %d: %w", id, err)
	}
	rs := &resolvedSchema{schema: schema, names: buildNameTable(schema), decoder: dec}
	s.cache[id] = rs
	return rs, nil
}

// fetchSchema GETs the writer schema text by id from the Schema Registry REST
// API over HTTPS (Confluent-compatible: /schemas/ids/{id} → {"schema": "..."}).
func (s *schemaResolver) fetchSchema(ctx context.Context, id int) (string, error) {
	url := fmt.Sprintf("%s/schemas/ids/%d", s.baseURL, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("get schema id %d: %w", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry returned %s for schema id %d", resp.Status, id)
	}
	var payload struct {
		Schema string `json:"schema"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode registry response for schema id %d: %w", id, err)
	}
	if payload.Schema == "" {
		return "", fmt.Errorf("registry returned an empty schema for id %d", id)
	}
	return payload.Schema, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func splitAndTrim(csv string) []string {
	var out []string
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
