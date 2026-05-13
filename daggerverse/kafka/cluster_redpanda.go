package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"dagger/kafka/internal/dagger"

	"gopkg.in/yaml.v3"
)

// RedpandaCluster is the Redpanda counterpart to *Cluster. Redpanda speaks
// the Kafka wire protocol but is a from-scratch C++ implementation with a
// completely different configuration layer (`rpk redpanda start`, a YAML
// config file, PEM cert/key files instead of PKCS#12), so it gets its own
// return type to make the divergence visible at the API surface. Single
// node only in this story (controllers=1, brokers=1).
type RedpandaCluster struct {
	// +private
	ClusterID string
	// +private
	BrokerSvc *dagger.Service
	// +private
	BrokerHost string
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
// archive of the CA cert + private key used to mint the per-cluster server
// leaf — same shape as Kafka.TlsServerSecurity, so callers don't have to
// convert between formats even though Redpanda itself reads PEM internally.
// The leaf carries the broker's stable hostname as a DNS SAN so franz-go
// clients dialing the bootstrap address can verify the cert against the
// matching truststore.
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

// RedpandaCluster spins up a single-node Redpanda cluster using the
// `redpandadata/redpanda` image. Redpanda runs broker and Raft duties in the
// same process, so there is no separate controller container.
//
// Multi-node (controllers != 1 or brokers != 1) is rejected — multi-broker
// Redpanda needs `--seeds` plumbing + per-node `rpc_server` advertising
// that doesn't fit single-story scope. The wire protocol matches Kafka,
// so RedpandaCluster.Client() returns the same *Client type the Apache
// constructors return.
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
			"Kafka.RedpandaCluster: single-node only in this story (got controllers=%d); multi-node Redpanda is not yet supported",
			controllers,
		)
	}
	if brokers != 1 {
		return nil, fmt.Errorf(
			"Kafka.RedpandaCluster: single-node only in this story (got brokers=%d); multi-broker Redpanda is not yet supported",
			brokers,
		)
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

	brokerHost := "redpanda-1-" + clusterHostSuffix(clusterId)

	ctr := dag.Container().From(image)

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
		assets, err := mintRedpandaTlsAssets(ctx, clientListenerSecurity, brokerHost)
		if err != nil {
			return nil, fmt.Errorf("mint redpanda tls assets: %w", err)
		}
		// Owned by redpanda:redpanda because the image's entrypoint
		// drops to that uid; rpk's start path chowns the live config
		// file to the same user and fails with EPERM if it's still
		// owned by root.
		ctr = ctr.
			WithFile("/etc/redpanda/certs/server.crt", assets.ServerCert, dagger.ContainerWithFileOpts{Permissions: 0o644, Owner: "redpanda:redpanda"}).
			WithMountedSecret("/etc/redpanda/certs/server.key", assets.ServerKey, dagger.ContainerWithMountedSecretOpts{Owner: "redpanda:redpanda", Mode: 0o400}).
			WithFile("/etc/redpanda/redpanda.yaml", assets.ConfigFile, dagger.ContainerWithFileOpts{Permissions: 0o644, Owner: "redpanda:redpanda"})
	} else {
		// PLAINTEXT: no YAML needed — pass listener flags directly.
		args = append(args,
			"--node-id", "0",
			"--kafka-addr", "PLAINTEXT://0.0.0.0:9092",
			"--advertise-kafka-addr", "PLAINTEXT://"+brokerHost+":9092",
			"--rpc-addr", "0.0.0.0:33145",
			"--advertise-rpc-addr", brokerHost+":33145",
			"--set", "redpanda.empty_seed_starts_cluster=true",
		)
	}

	svc := ctr.
		WithExposedPort(9092).
		WithExposedPort(33145).
		AsService(dagger.ContainerAsServiceOpts{
			Args:          args,
			UseEntrypoint: true,
		}).
		WithHostname(brokerHost)

	return &RedpandaCluster{
		ClusterID:          clusterId,
		BrokerSvc:          svc,
		BrokerHost:         brokerHost,
		ClientSecurityMode: mode,
	}, nil
}

// redpandaTlsAssets bundles the PEM material + rendered redpanda.yaml for a
// single-node Redpanda cluster with TLS termination on the external Kafka
// listener.
type redpandaTlsAssets struct {
	ServerCert *dagger.File   // PEM, mounted at /etc/redpanda/certs/server.crt
	ServerKey  *dagger.Secret // PEM PKCS#8, mounted at /etc/redpanda/certs/server.key
	ConfigFile *dagger.File   // rendered redpanda.yaml, mounted at /etc/redpanda/redpanda.yaml
}

// mintRedpandaTlsAssets loads the caller-supplied CA, signs a single server
// leaf with the broker's stable hostname as a DNS SAN, and renders the
// matching redpanda.yaml. Mirrors mintExternalLeaves' content-addressed
// staging via writeWorkdirBytes for the public-cert + YAML files; the
// private key crosses the container boundary as a *dagger.Secret so its
// plaintext never lands on disk in the module's workdir.
func mintRedpandaTlsAssets(
	ctx context.Context,
	sec *RedpandaServerSecurity,
	brokerHost string,
) (*redpandaTlsAssets, error) {
	ca := dag.CertificateManagement().LoadCertificateAuthority(sec.CaKeyStore, sec.CaKeyStorePassword)

	leafKeyPem, err := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).Pem().Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate redpanda leaf key: %w", err)
	}
	leafKey := dag.SetSecret("redpanda-leaf-key-"+randSuffix(), leafKeyPem)

	leafPwdHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate redpanda leaf password: %w", err)
	}
	leafPwd := dag.SetSecret("redpanda-leaf-pwd-"+randSuffix(), leafPwdHex)

	leafSerial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate redpanda leaf serial: %w", err)
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
		return nil, fmt.Errorf("materialize redpanda server cert: %w", err)
	}
	certFile, err := writeWorkdirBytes("redpanda-server-cert-"+brokerHost, "server.crt", certBytes)
	if err != nil {
		return nil, fmt.Errorf("stage redpanda server cert: %w", err)
	}
	yamlBytes, err := renderRedpandaYaml(brokerHost, true)
	if err != nil {
		return nil, fmt.Errorf("render redpanda.yaml: %w", err)
	}
	yamlFile, err := writeWorkdirBytes("redpanda-config-"+brokerHost, "redpanda.yaml", yamlBytes)
	if err != nil {
		return nil, fmt.Errorf("stage redpanda.yaml: %w", err)
	}

	return &redpandaTlsAssets{
		ServerCert: certFile,
		ServerKey:  issued.PrivateKeyPem(),
		ConfigFile: yamlFile,
	}, nil
}

// renderRedpandaYaml emits the full /etc/redpanda/redpanda.yaml for a
// single-node cluster. Rendered via yaml.v3 (not fmt.Fprintf) so any
// hostname containing YAML metacharacters is properly quoted.
func renderRedpandaYaml(brokerHost string, withTls bool) ([]byte, error) {
	type addr struct {
		Address string `yaml:"address"`
		Port    int    `yaml:"port"`
		Name    string `yaml:"name,omitempty"`
	}
	type tlsEntry struct {
		Name              string `yaml:"name"`
		Enabled           bool   `yaml:"enabled"`
		CertFile          string `yaml:"cert_file"`
		KeyFile           string `yaml:"key_file"`
		TruststoreFile    string `yaml:"truststore_file,omitempty"`
		RequireClientAuth bool   `yaml:"require_client_auth"`
	}
	rpCfg := map[string]any{
		"data_directory":            "/var/lib/redpanda/data",
		"node_id":                   0,
		"empty_seed_starts_cluster": true,
		"seed_servers":              []any{},
		"rpc_server":                addr{Address: "0.0.0.0", Port: 33145},
		"advertised_rpc_api":        addr{Address: brokerHost, Port: 33145},
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
	full := map[string]any{
		"redpanda":        rpCfg,
		"pandaproxy":      map[string]any{},
		"schema_registry": map[string]any{},
		"rpk": map[string]any{
			"coredump_dir": "/var/lib/redpanda/coredump",
		},
	}
	return yaml.Marshal(full)
}

// BootstrapServers returns the bootstrap address (single broker:9092) for
// this Redpanda cluster.
//
// +cache="never"
func (r *RedpandaCluster) BootstrapServers() []string {
	return []string{r.BrokerHost + ":9092"}
}

// BindBrokers binds the single Redpanda broker service into the given
// container so the container can reach it by hostname.
//
// +cache="never"
func (r *RedpandaCluster) BindBrokers(ctr *dagger.Container) *dagger.Container {
	return ctr.WithServiceBinding(r.BrokerHost, r.BrokerSvc)
}

// Client starts the Redpanda broker service and returns a franz-go-backed
// *Client targeting it. The Kafka wire protocol matches Apache Kafka, so
// the existing *Client + *ClientSecurity (PKCS#12) are reused unchanged.
//
// +cache="never"
func (r *RedpandaCluster) Client(ctx context.Context, security *ClientSecurity) (*Client, error) {
	if r == nil || r.BrokerSvc == nil {
		return nil, fmt.Errorf("RedpandaCluster.Client: cluster has no broker service")
	}
	if _, err := r.BrokerSvc.Start(ctx); err != nil {
		return nil, fmt.Errorf("start redpanda broker: %w", err)
	}
	return clientFrom(r.BootstrapServers(), security), nil
}

// Stop tears down the broker container backing this Redpanda cluster.
// Tests should call this in a defer so the broker `Container.asService`
// span closes when the test work is done. Kill is set so Service.Stop
// skips graceful shutdown — see Cluster.Stop for the rationale.
//
// +cache="never"
func (r *RedpandaCluster) Stop(ctx context.Context) error {
	if r == nil || r.BrokerSvc == nil {
		return nil
	}
	if _, err := r.BrokerSvc.Stop(ctx, dagger.ServiceStopOpts{Kill: true}); err != nil {
		return fmt.Errorf("stop redpanda broker: %w", err)
	}
	return nil
}
