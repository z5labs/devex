package main

import (
	"bytes"
	"context"
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
)

// schemaRegistryPort is the HTTP port the Confluent Schema Registry REST
// API listens on, both inside the container and as advertised to callers.
const schemaRegistryPort = 8081

// srContentType is the versioned media type the Schema Registry REST API
// expects on requests and returns on responses.
const srContentType = "application/vnd.schemaregistry.v1+json"

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
}

// SchemaRegistryClient is a pure-Go net/http client for a Schema Registry's
// admin REST API. Each method opens a fresh request so the function call is
// stateless from Dagger's perspective.
type SchemaRegistryClient struct {
	// +private
	Svc *dagger.Service
	// +private
	BaseURL string
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
) (*SchemaRegistry, error) {
	if cluster == nil {
		return nil, fmt.Errorf("Kafka.ConfluentSchemaRegistry: cluster must not be nil")
	}
	if len(cluster.BrokerSvcs) == 0 {
		return nil, fmt.Errorf("Kafka.ConfluentSchemaRegistry: cluster has no brokers")
	}
	if cluster.ClientSecurityMode != "PLAINTEXT" {
		return nil, fmt.Errorf(
			"Kafka.ConfluentSchemaRegistry: cluster client listener is %q; only PLAINTEXT "+
				"is supported in this story. TLS / mTLS Schema Registry "+
				"(SCHEMA_REGISTRY_KAFKASTORE_SECURITY_PROTOCOL=SSL plus keystore mounts) "+
				"is a follow-up story.",
			cluster.ClientSecurityMode,
		)
	}

	image := fmt.Sprintf("%s/confluentinc/cp-schema-registry:%s", registry, tag)
	// `csr-` (confluent schema registry) keeps the hostname short — a
	// longer `schema-registry-<suffix>` alias trips runc's sethostname on
	// startup — and gives sibling registry implementations room to pick
	// their own distinct prefix on the same cluster.
	srHost := "csr-" + clusterHostSuffix(cluster.ClusterID)

	// cp-schema-registry wants its bootstrap servers scheme-prefixed;
	// Cluster.BootstrapServers reports bare host:port.
	bootstrap := cluster.BootstrapServers()
	kafkastore := make([]string, len(bootstrap))
	for i, b := range bootstrap {
		kafkastore[i] = "PLAINTEXT://" + b
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

	ctr := dag.Container().
		From(image).
		WithEnvVariable("SCHEMA_REGISTRY_HOST_NAME", srHost).
		WithEnvVariable("SCHEMA_REGISTRY_LISTENERS", fmt.Sprintf("http://0.0.0.0:%d", schemaRegistryPort)).
		WithEnvVariable("SCHEMA_REGISTRY_KAFKASTORE_BOOTSTRAP_SERVERS", strings.Join(kafkastore, ",")).
		WithEnvVariable("SCHEMA_REGISTRY_KAFKASTORE_TOPIC_REPLICATION_FACTOR", strconv.Itoa(rf)).
		WithExposedPort(schemaRegistryPort)
	ctr = cluster.BindBrokers(ctr)

	srSvc := ctr.
		AsService(dagger.ContainerAsServiceOpts{UseEntrypoint: true}).
		WithHostname(srHost)

	return &SchemaRegistry{
		SchemaRegistrySvc: srSvc,
		AdvertisedHost:    srHost,
		AdvertisedPort:    schemaRegistryPort,
	}, nil
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
// Only PLAINTEXT clusters are supported in this story: the constructor
// rejects TLS / mTLS clusters and points callers at the TLS follow-up.
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
) (*SchemaRegistry, error) {
	if cluster == nil {
		return nil, fmt.Errorf("Kafka.ApicurioSchemaRegistry: cluster must not be nil")
	}
	if len(cluster.BrokerSvcs) == 0 {
		return nil, fmt.Errorf("Kafka.ApicurioSchemaRegistry: cluster has no brokers")
	}
	if cluster.ClientSecurityMode != "PLAINTEXT" {
		return nil, fmt.Errorf(
			"Kafka.ApicurioSchemaRegistry: cluster client listener is %q; only PLAINTEXT "+
				"is supported in this story. TLS / mTLS Schema Registry "+
				"(KAFKA_SSL_* / REGISTRY_KAFKASQL_SECURITY_PROTOCOL plus keystore mounts) "+
				"is a follow-up story.",
			cluster.ClientSecurityMode,
		)
	}

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
		// Apicurio is a Quarkus app listening on 8080 by default; pin it to
		// schemaRegistryPort so the advertised port matches cp-schema-registry.
		WithEnvVariable("QUARKUS_HTTP_PORT", strconv.Itoa(schemaRegistryPort)).
		WithExposedPort(schemaRegistryPort)
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
		BasePath: "/apis/ccompat/v7",
	}, nil
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
// Only PLAINTEXT clusters are supported in this story: the constructor
// rejects TLS / mTLS clusters and points callers at the TLS follow-up.
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
) (*SchemaRegistry, error) {
	if cluster == nil {
		return nil, fmt.Errorf("Kafka.KarapaceSchemaRegistry: cluster must not be nil")
	}
	if len(cluster.BrokerSvcs) == 0 {
		return nil, fmt.Errorf("Kafka.KarapaceSchemaRegistry: cluster has no brokers")
	}
	if cluster.ClientSecurityMode != "PLAINTEXT" {
		return nil, fmt.Errorf(
			"Kafka.KarapaceSchemaRegistry: cluster client listener is %q; only PLAINTEXT "+
				"is supported in this story. TLS / mTLS Schema Registry "+
				"(KARAPACE_SECURITY_PROTOCOL=SSL plus keystore mounts) "+
				"is a follow-up story.",
			cluster.ClientSecurityMode,
		)
	}

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

// Client returns a typed HTTP client targeting this registry's REST API.
// No I/O happens at construction time.
func (s *SchemaRegistry) Client() *SchemaRegistryClient {
	return &SchemaRegistryClient{
		Svc:     s.SchemaRegistrySvc,
		BaseURL: fmt.Sprintf("http://%s:%d%s", s.AdvertisedHost, s.AdvertisedPort, s.BasePath),
	}
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
	if _, err := c.Svc.Start(ctx); err != nil {
		return nil, 0, fmt.Errorf("start schema registry service: %w", err)
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

		resp, err := http.DefaultClient.Do(req)
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
