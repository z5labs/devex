package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"dagger/kafka/internal/dagger"

	"gopkg.in/yaml.v3"
)

// RedpandaCluster is the Redpanda counterpart to *Cluster. Redpanda speaks
// the Kafka wire protocol but is a from-scratch C++ implementation with a
// completely different configuration layer (`rpk redpanda start`, a YAML
// config file, PEM cert/key files instead of PKCS#12), so it gets its own
// return type to make the divergence visible at the API surface.
//
// Supports a genuine multi-broker Raft cluster: N brokers (node IDs 0..N-1)
// form a single Raft group over the internal RPC listener, discovering each
// other by deterministic hostname (redpanda-<n>-<suffix>) via the engine's
// session-wide DNS. Redpanda runs broker and Raft duties in the SAME process,
// so there is no separate controller container.
type RedpandaCluster struct {
	// +private
	ClusterID string
	// +private
	BrokerSvcs []*dagger.Service
	// +private
	BrokerHosts []string
	// +private
	ClientSecurityMode string // PLAINTEXT | TLS
}

// RedpandaServerSecurity carries the external-listener security profile for
// a Redpanda cluster. Same shape as *ServerSecurity (PKCS#12 CA + password)
// so callers don't have to convert; the constructor extracts PEM from the
// issued leaf internally for redpanda.yaml. Separate type from
// *ServerSecurity so a caller can't accidentally hand an Apache profile
// (e.g. MtlsServerSecurity, not supported here yet) to RedpandaCluster.
type RedpandaServerSecurity struct {
	// +private
	Mode string // PLAINTEXT | TLS
	// +private
	CaKeyStore *dagger.File
	// +private
	CaKeyStorePassword *dagger.Secret
}

// RedpandaPlaintextServerSecurity returns a RedpandaServerSecurity profile
// configured for unencrypted, unauthenticated traffic on the external Kafka
// listener.
func (k *Kafka) RedpandaPlaintextServerSecurity() *RedpandaServerSecurity {
	return &RedpandaServerSecurity{Mode: "PLAINTEXT"}
}

// RedpandaTlsServerSecurity returns a RedpandaServerSecurity profile that
// terminates TLS on the external Kafka listener. caKeyStore is a PKCS#12
// archive of the CA cert + private key used to mint the per-node server
// leaves — same shape as Kafka.TlsServerSecurity, so callers don't have to
// convert between formats even though Redpanda itself reads PEM internally.
// Each node's leaf carries that node's stable hostname as a DNS SAN so
// franz-go clients dialing any broker in the bootstrap list can verify the
// cert against the matching truststore.
func (k *Kafka) RedpandaTlsServerSecurity(
	caKeyStore *dagger.File,
	caKeyStorePassword *dagger.Secret,
) *RedpandaServerSecurity {
	return &RedpandaServerSecurity{
		Mode:               "TLS",
		CaKeyStore:         caKeyStore,
		CaKeyStorePassword: caKeyStorePassword,
	}
}

// RedpandaCluster spins up a Redpanda cluster of `brokers` nodes using the
// `redpandadata/redpanda` image. Redpanda runs broker and Raft duties in the
// same process, so there is no separate controller container — every node is
// a full broker that also participates in the Raft group.
//
// Topology: node hostnames are deterministic (redpanda-<n>-<suffix>, node IDs
// 0..N-1, suffix derived from clusterId), so the seed list is computed before
// any container is built and pinned onto every node via WithHostname +
// session-wide DNS. For brokers > 1 the cluster uses Redpanda's seed-driven
// bootstrap: every node shares the identical seed_servers list (all N nodes)
// and empty_seed_starts_cluster=false, so the nodes deterministically form one
// Raft group over the internal RPC listener (:33145) with NO node-to-node
// WithServiceBinding — they are started concurrently and discover each other
// by hostname over session-wide DNS. A single-broker cluster keeps the legacy
// empty_seed_starts_cluster=true bootstrap (empty seed list).
//
// controllers must be 1: Redpanda has no separate controller role, so a
// controller count is not a meaningful concept and any other value is
// rejected (see the constructor's error). Size the cluster with `brokers`.
//
// Inter-node RPC security: the internal RPC listener (:33145) that carries
// Raft traffic is PLAINTEXT and unauthenticated even when the external Kafka
// listener is TLS. Redpanda's RPC-listener TLS would need its own internal
// CA + per-node leaves + mutual trust — a whole parallel PKI — for traffic
// that never leaves the Dagger engine's isolated per-session network. This
// deliberately differs from the Apache path (which always mTLS-encrypts its
// internal + controller listeners) because Redpanda has no equivalent PKCS#12
// env-var contract to reuse; TLS here is scoped to the client-facing Kafka
// listener + bundled Schema Registry REST endpoint, which is what external
// clients actually verify.
//
// The wire protocol matches Kafka, so RedpandaCluster.Client() returns the
// same *Client type the Apache constructors return.
//
// +cache="session"
func (k *Kafka) RedpandaCluster(
	ctx context.Context,
	clusterId string,
	// +default=1
	controllers int,
	// +default=1
	brokers int,
	// +default="docker.io"
	registry string,
	// +default="v26.1.7"
	tag string,
	clientListenerSecurity *RedpandaServerSecurity,
) (*RedpandaCluster, error) {
	image := fmt.Sprintf("%s/redpandadata/redpanda:%s", registry, tag)
	return buildRedpandaCluster(ctx, clusterId, controllers, brokers, image, clientListenerSecurity)
}

func buildRedpandaCluster(
	ctx context.Context,
	clusterId string,
	controllers int,
	brokers int,
	image string,
	clientListenerSecurity *RedpandaServerSecurity,
) (*RedpandaCluster, error) {
	if controllers != 1 {
		return nil, fmt.Errorf(
			"Kafka.RedpandaCluster: Redpanda has no separate controller role — broker and Raft duties run in the same process, so a controller count is not a meaningful concept (got controllers=%d, want 1); size the cluster with `brokers` instead",
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
	case "PLAINTEXT", "TLS":
	default:
		return nil, fmt.Errorf("unknown clientListenerSecurity mode %q (want PLAINTEXT|TLS)", mode)
	}
	if mode == "TLS" && (clientListenerSecurity.CaKeyStore == nil || clientListenerSecurity.CaKeyStorePassword == nil) {
		return nil, fmt.Errorf("clientListenerSecurity mode TLS requires a CA keystore and password")
	}
	if raw, err := base64.RawURLEncoding.DecodeString(clusterId); err != nil || len(raw) != 16 {
		return nil, fmt.Errorf(
			"clusterId must be 16 bytes encoded as 22 unpadded base64-url chars, got %q",
			clusterId,
		)
	}

	// Deterministic per-cluster hostnames (redpanda-<n>-<suffix>, node IDs
	// 0..N-1) let the whole seed list be assembled here — before any
	// container exists — and pinned identically onto every node via
	// WithHostname. At runtime each node resolves its peers by hostname over
	// the engine's session-wide DNS, so a real Raft group forms with no
	// node-to-node WithServiceBinding and thus no unresolvable build cycle.
	suffix := clusterHostSuffix(clusterId)
	brokerHosts := make([]string, brokers)
	seedHosts := make([]string, brokers)
	for i := range brokerHosts {
		brokerHosts[i] = fmt.Sprintf("redpanda-%d-%s", i+1, suffix)
		seedHosts[i] = brokerHosts[i]
	}
	multiNode := brokers > 1

	// TLS: mint a server leaf per node (each SAN'd to its own hostname) and a
	// per-node redpanda.yaml; the CA cert is shared across nodes as the
	// truststore for each node's bundled Schema Registry / pandaproxy client.
	var tlsAssets []redpandaNodeTlsAssets
	if mode == "TLS" {
		var err error
		tlsAssets, err = mintRedpandaTlsAssets(ctx, clientListenerSecurity, brokerHosts, seedHosts, multiNode)
		if err != nil {
			return nil, fmt.Errorf("mint redpanda tls assets: %w", err)
		}
	}

	// Build each node's pre-service container + its `rpk redpanda start` args.
	nodeCtrs := make([]*dagger.Container, brokers)
	nodeArgs := make([][]string, brokers)
	for i := 0; i < brokers; i++ {
		host := brokerHosts[i]
		ctr := dag.Container().From(image).
			WithExposedPort(9092).
			WithExposedPort(33145).
			WithExposedPort(schemaRegistryPort)

		// rpk redpanda start args. --mode dev-container bundles
		// --overprovisioned, --reserve-memory 0M, --check=false, and
		// --unsafe-bypass-fsync so we don't repeat those flags.
		args := []string{
			"redpanda", "start",
			"--mode", "dev-container",
			"--smp", "1",
			"--memory", "1G",
		}

		if mode == "TLS" {
			a := tlsAssets[i]
			// Owned by redpanda:redpanda because the image's entrypoint
			// drops to that uid; rpk's start path chowns the live config
			// file to the same user and fails with EPERM if it's still
			// owned by root.
			ctr = ctr.
				WithFile("/etc/redpanda/certs/server.crt", a.ServerCert, dagger.ContainerWithFileOpts{Permissions: 0o644, Owner: "redpanda:redpanda"}).
				WithMountedSecret("/etc/redpanda/certs/server.key", a.ServerKey, dagger.ContainerWithMountedSecretOpts{Owner: "redpanda:redpanda", Mode: 0o400}).
				WithFile("/etc/redpanda/certs/ca.crt", a.CaCert, dagger.ContainerWithFileOpts{Permissions: 0o644, Owner: "redpanda:redpanda"}).
				WithFile("/etc/redpanda/redpanda.yaml", a.ConfigFile, dagger.ContainerWithFileOpts{Permissions: 0o644, Owner: "redpanda:redpanda"})
		} else {
			// PLAINTEXT: no YAML needed — pass listener flags directly.
			args = append(args,
				"--node-id", fmt.Sprintf("%d", i),
				"--kafka-addr", "PLAINTEXT://0.0.0.0:9092",
				"--advertise-kafka-addr", "PLAINTEXT://"+host+":9092",
				"--rpc-addr", "0.0.0.0:33145",
				"--advertise-rpc-addr", host+":33145",
				"--schema-registry-addr", fmt.Sprintf("0.0.0.0:%d", schemaRegistryPort),
			)
			if multiNode {
				// Seed-driven bootstrap: every node shares the identical
				// seed list (all N nodes) and does NOT self-start an empty
				// cluster, so the nodes deterministically form one Raft group.
				seeds := make([]string, brokers)
				for j, h := range seedHosts {
					seeds[j] = h + ":33145"
				}
				args = append(args,
					"--seeds", strings.Join(seeds, ","),
					"--set", "redpanda.empty_seed_starts_cluster=false",
				)
			} else {
				args = append(args,
					"--set", "redpanda.empty_seed_starts_cluster=true",
				)
			}
		}

		nodeCtrs[i] = ctr
		nodeArgs[i] = args
	}

	// Every node is an independent service pinned to its deterministic
	// hostname; there is NO node-to-node WithServiceBinding. A symmetric Raft
	// peer mesh can't use bindings: binding node A→B forces Dagger to fully
	// ready B (all exposed ports up) before it even boots A, but B can't open
	// its Kafka/Schema-Registry ports until the cluster forms, which needs A —
	// a readiness deadlock. Instead the nodes are started concurrently (see
	// startAll, used by Client and SchemaRegistry) so they boot together and
	// discover each other by hostname over session-wide DNS, forming the Raft
	// group over the internal RPC listener.
	svcs := make([]*dagger.Service, brokers)
	for i := 0; i < brokers; i++ {
		svcs[i] = nodeCtrs[i].
			AsService(dagger.ContainerAsServiceOpts{
				Args:          nodeArgs[i],
				UseEntrypoint: true,
			}).
			WithHostname(brokerHosts[i])
	}

	return &RedpandaCluster{
		ClusterID:          clusterId,
		BrokerSvcs:         svcs,
		BrokerHosts:        brokerHosts,
		ClientSecurityMode: mode,
	}, nil
}

// redpandaNodeTlsAssets bundles one node's PEM material + rendered
// redpanda.yaml for a Redpanda cluster with TLS termination on the external
// Kafka listener. The CA cert is shared across nodes (same *dagger.File on
// every node); the server leaf + config are per-node.
type redpandaNodeTlsAssets struct {
	ServerCert *dagger.File   // PEM, mounted at /etc/redpanda/certs/server.crt
	ServerKey  *dagger.Secret // PEM PKCS#8, mounted at /etc/redpanda/certs/server.key
	CaCert     *dagger.File   // CA PEM, mounted at /etc/redpanda/certs/ca.crt — truststore for the bundled SR's broker client
	ConfigFile *dagger.File   // rendered redpanda.yaml, mounted at /etc/redpanda/redpanda.yaml
}

// mintRedpandaTlsAssets loads the caller-supplied CA once, then for each node
// signs a server leaf carrying that node's stable hostname as a DNS SAN and
// renders the matching per-node redpanda.yaml. Mirrors mintExternalLeaves'
// content-addressed staging via writeWorkdirBytes for the public-cert + YAML
// files; each node's private key crosses the container boundary as a
// *dagger.Secret so its plaintext never lands on disk in the module's workdir.
func mintRedpandaTlsAssets(
	ctx context.Context,
	sec *RedpandaServerSecurity,
	brokerHosts []string,
	seedHosts []string,
	multiNode bool,
) ([]redpandaNodeTlsAssets, error) {
	ca := dag.CertificateManagement().LoadCertificateAuthority(sec.CaKeyStore, sec.CaKeyStorePassword)

	// The bundled Schema Registry / pandaproxy talk to the broker over the
	// Kafka wire protocol; with a TLS-only external listener they must dial
	// over TLS and trust the cluster CA, so the CA cert is mounted as their
	// truststore. Materialized once and shared across nodes.
	caCertBytes, err := dagFileBytes(ctx, ca.CertPemFile())
	if err != nil {
		return nil, fmt.Errorf("materialize redpanda ca cert: %w", err)
	}
	caCertFile, err := writeWorkdirBytes("redpanda-ca-cert", "ca.crt", caCertBytes)
	if err != nil {
		return nil, fmt.Errorf("stage redpanda ca cert: %w", err)
	}

	assets := make([]redpandaNodeTlsAssets, len(brokerHosts))
	for i, brokerHost := range brokerHosts {
		leafKeyPem, err := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).Pem().Contents(ctx)
		if err != nil {
			return nil, fmt.Errorf("generate redpanda leaf key for %q: %w", brokerHost, err)
		}
		leafKey := dag.SetSecret("redpanda-leaf-key-"+randSuffix(), leafKeyPem)

		leafPwdHex, err := dag.Random().Sha256(ctx)
		if err != nil {
			return nil, fmt.Errorf("generate redpanda leaf password for %q: %w", brokerHost, err)
		}
		leafPwd := dag.SetSecret("redpanda-leaf-pwd-"+randSuffix(), leafPwdHex)

		leafSerial, err := dag.Random().Serial(ctx)
		if err != nil {
			return nil, fmt.Errorf("generate redpanda leaf serial for %q: %w", brokerHost, err)
		}
		nb := time.Now().UTC().Format(time.RFC3339)

		issued := ca.IssueServerCertificate(brokerHost, nb, leafSerial, leafPwd, leafKey,
			dagger.CertificateManagementCertificateAuthorityIssueServerCertificateOpts{
				DNSSans:      []string{brokerHost, "localhost"},
				IPSans:       []string{"127.0.0.1"},
				ValidityDays: 365,
			})

		certBytes, err := dagFileBytes(ctx, issued.CertPemFile())
		if err != nil {
			return nil, fmt.Errorf("materialize redpanda server cert for %q: %w", brokerHost, err)
		}
		certFile, err := writeWorkdirBytes("redpanda-server-cert-"+brokerHost, "server.crt", certBytes)
		if err != nil {
			return nil, fmt.Errorf("stage redpanda server cert for %q: %w", brokerHost, err)
		}

		yamlBytes, err := renderRedpandaYaml(i, brokerHost, brokerHosts, seedHosts, multiNode, true)
		if err != nil {
			return nil, fmt.Errorf("render redpanda.yaml for %q: %w", brokerHost, err)
		}
		yamlFile, err := writeWorkdirBytes("redpanda-config-"+brokerHost, "redpanda.yaml", yamlBytes)
		if err != nil {
			return nil, fmt.Errorf("stage redpanda.yaml for %q: %w", brokerHost, err)
		}

		assets[i] = redpandaNodeTlsAssets{
			ServerCert: certFile,
			ServerKey:  issued.PrivateKeyPem(),
			CaCert:     caCertFile,
			ConfigFile: yamlFile,
		}
	}
	return assets, nil
}

// renderRedpandaYaml emits the full /etc/redpanda/redpanda.yaml for one node
// (nodeID, advertised as brokerHost) of a Redpanda cluster. For a multi-node
// cluster seedHosts lists every node and seed-driven bootstrap is used
// (empty_seed_starts_cluster=false); for a single node the seed list is empty
// and the node self-starts its cluster (empty_seed_starts_cluster=true).
// Rendered via yaml.v3 (not fmt.Fprintf) so any hostname containing YAML
// metacharacters is properly quoted.
func renderRedpandaYaml(nodeID int, brokerHost string, brokerHosts []string, seedHosts []string, multiNode bool, withTls bool) ([]byte, error) {
	type addr struct {
		Address string `yaml:"address"`
		Port    int    `yaml:"port"`
		Name    string `yaml:"name,omitempty"`
	}
	type seedHost struct {
		Address string `yaml:"address"`
		Port    int    `yaml:"port"`
	}
	type seedServer struct {
		Host seedHost `yaml:"host"`
	}
	type tlsEntry struct {
		Name              string `yaml:"name"`
		Enabled           bool   `yaml:"enabled"`
		CertFile          string `yaml:"cert_file"`
		KeyFile           string `yaml:"key_file"`
		TruststoreFile    string `yaml:"truststore_file,omitempty"`
		RequireClientAuth bool   `yaml:"require_client_auth"`
	}
	// brokerTLS / kafkaClientCfg configure the internal Kafka clients the
	// bundled Schema Registry and pandaproxy use to reach the broker. On a
	// TLS cluster the only Kafka listener is TLS, so these clients must dial
	// it over TLS and trust the cluster CA.
	type brokerTLS struct {
		Enabled        bool   `yaml:"enabled"`
		TruststoreFile string `yaml:"truststore_file"`
	}
	type kafkaClientCfg struct {
		Brokers   []addr    `yaml:"brokers"`
		BrokerTLS brokerTLS `yaml:"broker_tls"`
	}

	// Seed-driven bootstrap for a multi-node cluster: every node carries the
	// identical seed_servers list (all N nodes) and empty_seed_starts_cluster
	// is false so no single node self-elects a one-node cluster. A single-node
	// cluster keeps the legacy empty-seed self-bootstrap.
	seedServers := []any{}
	if multiNode {
		for _, h := range seedHosts {
			seedServers = append(seedServers, seedServer{Host: seedHost{Address: h, Port: 33145}})
		}
	}

	rpCfg := map[string]any{
		"data_directory":            "/var/lib/redpanda/data",
		"node_id":                   nodeID,
		"empty_seed_starts_cluster": !multiNode,
		"seed_servers":              seedServers,
		// The internal RPC listener carries Raft traffic between nodes. It is
		// PLAINTEXT/unauthenticated even on a TLS cluster (see the
		// RedpandaCluster doc comment) — it never leaves the engine session.
		"rpc_server":         addr{Address: "0.0.0.0", Port: 33145},
		"advertised_rpc_api": addr{Address: brokerHost, Port: 33145},
		"kafka_api": []addr{
			{Address: "0.0.0.0", Port: 9092, Name: "external"},
		},
		"advertised_kafka_api": []addr{
			{Address: brokerHost, Port: 9092, Name: "external"},
		},
		"admin": []addr{
			{Address: "0.0.0.0", Port: 9644},
		},
		"developer_mode": true,
	}
	if withTls {
		rpCfg["kafka_api_tls"] = []tlsEntry{{
			Name:              "external",
			Enabled:           true,
			CertFile:          "/etc/redpanda/certs/server.crt",
			KeyFile:           "/etc/redpanda/certs/server.key",
			RequireClientAuth: false,
		}}
	}
	// Bundle the Schema Registry REST listener. On a TLS cluster it reuses
	// the broker's own server leaf (already mounted at /etc/redpanda/certs)
	// to terminate HTTPS — the leaf's DNS SAN is brokerHost, which is also
	// the registry's advertised host — so a TLS Redpanda cluster serves its
	// bundled Schema Registry over HTTPS. (SR-REST mTLS is not reachable:
	// Redpanda has no cluster-level mTLS mode.)
	srBlock := map[string]any{
		"schema_registry_api": []addr{
			{Address: "0.0.0.0", Port: schemaRegistryPort, Name: "external"},
		},
	}
	if withTls {
		srBlock["schema_registry_api_tls"] = []tlsEntry{{
			Name:              "external",
			Enabled:           true,
			CertFile:          "/etc/redpanda/certs/server.crt",
			KeyFile:           "/etc/redpanda/certs/server.key",
			RequireClientAuth: false,
		}}
	}
	full := map[string]any{
		"redpanda":        rpCfg,
		"pandaproxy":      map[string]any{},
		"schema_registry": srBlock,
		"rpk": map[string]any{
			"coredump_dir": "/var/lib/redpanda/coredump",
		},
	}
	if withTls {
		// The bundled SR / pandaproxy Kafka clients bootstrap against every
		// broker over TLS, trusting the cluster CA. Listing all brokers keeps
		// the client reachable even if the local node is not the leader for
		// the internal _schemas topic.
		brokerAddrs := make([]addr, len(brokerHosts))
		for i, h := range brokerHosts {
			brokerAddrs[i] = addr{Address: h, Port: 9092}
		}
		clientCfg := kafkaClientCfg{
			Brokers: brokerAddrs,
			BrokerTLS: brokerTLS{
				Enabled:        true,
				TruststoreFile: "/etc/redpanda/certs/ca.crt",
			},
		}
		full["schema_registry_client"] = clientCfg
		full["pandaproxy_client"] = clientCfg
	}
	return yaml.Marshal(full)
}

// startAll boots every node service concurrently. Concurrency is required for
// a multi-node cluster: the nodes are symmetric Raft peers with no service
// bindings between them, so a node's ports (Kafka 9092, Schema Registry 8081)
// only open once the cluster forms — which needs all peers up. Starting them
// one at a time would block on the first node forever (it waits for its own
// health check, which waits for peers that haven't been started yet); starting
// them together lets the Raft group form and every node's health check pass.
func startAll(ctx context.Context, svcs []*dagger.Service) error {
	var wg sync.WaitGroup
	errs := make([]error, len(svcs))
	for i, svc := range svcs {
		if svc == nil {
			continue
		}
		wg.Add(1)
		go func(i int, svc *dagger.Service) {
			defer wg.Done()
			if _, err := svc.Start(ctx); err != nil {
				errs[i] = fmt.Errorf("start redpanda broker %d: %w", i, err)
			}
		}(i, svc)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// BootstrapServers returns the host:port bootstrap addresses for every broker
// in this Redpanda cluster.
//
// +cache="never"
func (r *RedpandaCluster) BootstrapServers() []string {
	out := make([]string, len(r.BrokerHosts))
	for i, h := range r.BrokerHosts {
		out[i] = h + ":9092"
	}
	return out
}

// SchemaRegistry exposes Redpanda's bundled Schema Registry as the same
// *SchemaRegistry type Kafka.ConfluentSchemaRegistry returns, so callers can
// treat the bundled and separate-container registries uniformly.
//
// `rpk redpanda start` runs a Schema Registry inside every broker process on
// :8081 — no extra container. The returned *SchemaRegistry points at the first
// broker (node 0); because the registry client only starts that one service,
// this method brings the whole cluster online (startAll) before returning, so
// the registry is backed by a formed Raft group. Redpanda's SR speaks the
// Confluent Schema Registry REST API, so the *SchemaRegistryClient from
// Client() works unchanged.
//
// security must match the cluster's mode (PLAINTEXT or TLS — Redpanda has no
// mTLS): on a TLS cluster the bundled SR REST endpoint terminates HTTPS
// reusing the broker's server leaf (configured at cluster-build time in
// renderRedpandaYaml), so the caller must pass a TLS profile to get an HTTPS
// client. The profile's CA keystore is unused here (the leaf is already
// minted); it is required only for API uniformity with the other registries.
//
// The returned registry is Bundled: its service is a broker itself, so Stop is
// a no-op on it — call cluster.Stop to tear the registry down with the
// cluster. A caller that uniformly `defer sr.Stop(ctx)` over the shared
// *SchemaRegistry type therefore can't accidentally kill the cluster.
//
// +cache="never"
func (r *RedpandaCluster) SchemaRegistry(ctx context.Context, security *SchemaRegistrySecurity) (*SchemaRegistry, error) {
	if r == nil || len(r.BrokerSvcs) == 0 {
		return nil, fmt.Errorf("RedpandaCluster.SchemaRegistry: cluster has no broker service")
	}
	if err := validateRegistrySecurity("RedpandaCluster.SchemaRegistry", security, r.ClientSecurityMode); err != nil {
		return nil, err
	}
	// The bundled registry's service is node 0, but node 0 alone can't form a
	// multi-node cluster — the registry client (do()) only starts that one
	// service. Bring the whole cluster online here so the returned registry is
	// backed by a formed Raft group. For a single node this just starts it.
	if err := startAll(ctx, r.BrokerSvcs); err != nil {
		return nil, err
	}
	return &SchemaRegistry{
		SchemaRegistrySvc: r.BrokerSvcs[0],
		AdvertisedHost:    r.BrokerHosts[0],
		AdvertisedPort:    schemaRegistryPort,
		Bundled:           true,
		SecurityMode:      security.Mode,
	}, nil
}

// BindBrokers binds every Redpanda broker service into the given container so
// the container can reach them by the same hostnames BootstrapServers reports.
//
// +cache="never"
func (r *RedpandaCluster) BindBrokers(ctr *dagger.Container) *dagger.Container {
	for i, svc := range r.BrokerSvcs {
		ctr = ctr.WithServiceBinding(r.BrokerHosts[i], svc)
	}
	return ctr
}

// Client starts every Redpanda broker service — bringing the whole Raft group
// online — and returns a franz-go-backed *Client targeting them. The Kafka
// wire protocol matches Apache Kafka, so the existing *Client + *ClientSecurity
// (PKCS#12) are reused unchanged.
//
// +cache="never"
func (r *RedpandaCluster) Client(ctx context.Context, security *ClientSecurity) (*Client, error) {
	if r == nil || len(r.BrokerSvcs) == 0 {
		return nil, fmt.Errorf("RedpandaCluster.Client: cluster has no broker service")
	}
	if err := startAll(ctx, r.BrokerSvcs); err != nil {
		return nil, err
	}
	return clientFrom(r.BootstrapServers(), security), nil
}

// Stop tears down every broker container backing this Redpanda cluster.
// Tests should call this in a defer so each broker `Container.asService`
// span closes when the test work is done. Kill is set so Service.Stop
// skips graceful shutdown — see Cluster.Stop for the rationale.
//
// +cache="never"
func (r *RedpandaCluster) Stop(ctx context.Context) error {
	if r == nil {
		return nil
	}
	opts := dagger.ServiceStopOpts{Kill: true}
	var errs []error
	for i, svc := range r.BrokerSvcs {
		if svc == nil {
			continue
		}
		if _, err := svc.Stop(ctx, opts); err != nil {
			errs = append(errs, fmt.Errorf("stop redpanda broker %d: %w", i, err))
		}
	}
	return errors.Join(errs...)
}
