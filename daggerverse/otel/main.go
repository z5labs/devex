// Package main is the otel Dagger module: spins up the OpenTelemetry
// Collector as a service for local development and testing, with a
// component/pipeline builder API for composing receivers, processors,
// and exporters without writing YAML by hand.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"

	"dagger/otel/internal/dagger"

	"gopkg.in/yaml.v3"
)

const (
	coreImagePath    = "otel/opentelemetry-collector"
	contribImagePath = "otel/opentelemetry-collector-contrib"
	defaultRegistry  = "docker.io"
	defaultTag       = "0.130.1"
	otlpGrpcPort     = 4317
	otlpHttpPort     = 4318
	configMountPath  = "/etc/otelcol/config.yaml"

	// TLS material mount paths. Receiver-side (collector-level) server
	// cert/key and the mTLS client CA land at fixed paths; exporter-side
	// material is namespaced per component id under exporterTlsDirPath so
	// distinct exporters never collide.
	tlsBaseDir         = "/etc/otelcol/tls"
	serverCertPath     = tlsBaseDir + "/server-cert.pem"
	serverKeyPath      = tlsBaseDir + "/server-key.pem"
	clientCaPath       = tlsBaseDir + "/client-ca.pem"
	exporterTlsDirPath = tlsBaseDir + "/exporters"
)

// exporterCertDir returns the container directory an exporter's TLS
// material is mounted under. Derived from the component id (kind/name),
// which the collector already guarantees unique, so two exporters never
// share a mount path.
func exporterCertDir(kind, name string) string {
	return fmt.Sprintf("%s/%s_%s", exporterTlsDirPath, kind, name)
}

func coreImage(registry, tag string) string {
	return fmt.Sprintf("%s/%s:%s", registry, coreImagePath, tag)
}

func contribImage(registry, tag string) string {
	return fmt.Sprintf("%s/%s:%s", registry, contribImagePath, tag)
}

func resolveConfigFile(override *dagger.File, pipelines []*Pipeline, tlsEnabled, mtlsEnabled bool) (*dagger.File, error) {
	if override != nil {
		return override, nil
	}
	if len(pipelines) == 0 {
		return nil, nil
	}
	body, err := renderCollectorYAML(pipelines, tlsEnabled, mtlsEnabled)
	if err != nil {
		return nil, err
	}
	return writeWorkdirFile("config.yaml", body)
}

// buildService composes the running collector container. When the
// resolved config is nil (no override and no pipelines), the
// container launches with no --config flag and the collector binary
// exits non-zero — exposed verbatim so callers can detect the
// misconfiguration via service-binding probes.
func buildService(image string, override *dagger.File, pipelines []*Pipeline, hosts []string, svcs []*dagger.Service, serverCert *dagger.File, serverKey *dagger.Secret, clientCa *dagger.File) (*dagger.Service, error) {
	if clientCa != nil && serverCert == nil {
		return nil, fmt.Errorf("WithMtls requires WithTls: a client CA was set without a server certificate")
	}
	cfg, err := resolveConfigFile(override, pipelines, serverCert != nil, clientCa != nil)
	if err != nil {
		return nil, err
	}
	// Strip the image's default args (the upstream image ships
	// a sample config at /etc/otelcol/config.yaml plus a default
	// CMD of ["--config", "/etc/otelcol/config.yaml"]); we
	// re-supply --config explicitly when we have one, and pass
	// nothing otherwise so the binary fails fast on startup.
	ctr := dag.Container().From(image).
		WithUser("0:0").
		WithoutDefaultArgs().
		WithExposedPort(otlpGrpcPort).
		WithExposedPort(otlpHttpPort)
	for i, host := range hosts {
		ctr = ctr.WithServiceBinding(host, svcs[i])
	}
	// Receiver-side (collector-level) TLS material at fixed paths, then
	// exporter-side material namespaced per component id.
	if serverCert != nil {
		ctr = ctr.WithMountedFile(serverCertPath, serverCert)
	}
	if serverKey != nil {
		ctr = ctr.WithMountedSecret(serverKeyPath, serverKey)
	}
	if clientCa != nil {
		ctr = ctr.WithMountedFile(clientCaPath, clientCa)
	}
	ctr = mountExporterTls(ctr, pipelines)
	var args []string
	if cfg != nil {
		ctr = ctr.WithMountedFile(configMountPath, cfg)
		args = []string{"--config=" + configMountPath}
	}
	return ctr.AsService(dagger.ContainerAsServiceOpts{
		UseEntrypoint: true,
		Args:          args,
	}), nil
}

// mountExporterTls mounts every TLS-carrying exporter's cert/key material
// into the collector at the per-component paths already referenced by the
// rendered config. Mounts are deduped by directory so a single exporter
// wired into many pipelines is mounted once; the key is a secret mount.
func mountExporterTls(ctr *dagger.Container, pipelines []*Pipeline) *dagger.Container {
	seen := map[string]bool{}
	for _, p := range pipelines {
		for _, e := range p.Exporters {
			if e.CaCert == nil && e.ClientCert == nil && e.ClientKey == nil {
				continue
			}
			dir := exporterCertDir(e.Kind, e.Name)
			if seen[dir] {
				continue
			}
			seen[dir] = true
			if e.CaCert != nil {
				ctr = ctr.WithMountedFile(dir+"/ca.pem", e.CaCert)
			}
			if e.ClientCert != nil {
				ctr = ctr.WithMountedFile(dir+"/cert.pem", e.ClientCert)
			}
			if e.ClientKey != nil {
				ctr = ctr.WithMountedSecret(dir+"/key.pem", e.ClientKey)
			}
		}
	}
	return ctr
}

type Otel struct{}

// Receiver is a single OpenTelemetry Collector receiver component.
// Body is the YAML body for this component, spliced under
// `receivers.<kind>/<name>` at render time.
type Receiver struct {
	Kind string
	Name string
	Body string
}

// Processor is a single OpenTelemetry Collector processor component.
type Processor struct {
	Kind string
	Name string
	Body string
}

// Exporter is a single OpenTelemetry Collector exporter component. When
// TLS options are supplied to the factory, the cert/key material is
// carried here so the collector can mount it at the paths already baked
// into Body (see exporterCertDir).
type Exporter struct {
	Kind string
	Name string
	Body string
	// +private
	CaCert *dagger.File
	// +private
	ClientCert *dagger.File
	// +private
	ClientKey *dagger.Secret
}

// nameRe matches the names (and Custom* kinds) the collector permits as
// component IDs without YAML-quoting hazards.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func validateName(field, name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("%s %q: must match [A-Za-z0-9_-]+", field, name)
	}
	return nil
}

func marshalBody(v map[string]any) (string, error) {
	b, err := yaml.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal component body: %w", err)
	}
	return string(b), nil
}

// OtlpReceiver builds the standard OTLP receiver listening on gRPC :4317
// and HTTP :4318.
func (o *Otel) OtlpReceiver(name string) (*Receiver, error) {
	if err := validateName("name", name); err != nil {
		return nil, err
	}
	body, err := marshalBody(map[string]any{
		"protocols": map[string]any{
			"grpc": map[string]any{"endpoint": "0.0.0.0:4317"},
			"http": map[string]any{"endpoint": "0.0.0.0:4318"},
		},
	})
	if err != nil {
		return nil, err
	}
	return &Receiver{Kind: "otlp", Name: name, Body: body}, nil
}

// OtlpExporter builds an OTLP gRPC exporter pointing at endpoint
// (host:port, no scheme). With no TLS options the exporter is plaintext
// (tls.insecure=true). Supplying caCert pins the server CA; supplying
// clientCert + clientKey (which must be given together) presents an mTLS
// identity.
func (o *Otel) OtlpExporter(
	name, endpoint string,
	// PEM-encoded CA certificate to verify the receiver against. When set
	// the exporter speaks TLS instead of plaintext.
	// +optional
	caCert *dagger.File,
	// PEM-encoded client certificate presented for mTLS. Must be paired
	// with clientKey.
	// +optional
	clientCert *dagger.File,
	// PEM-encoded PKCS#8 client private key for mTLS. Must be paired with
	// clientCert.
	// +optional
	clientKey *dagger.Secret,
) (*Exporter, error) {
	return newOtlpExporter("otlp", name, endpoint, caCert, clientCert, clientKey)
}

// OtlpHttpExporter builds an OTLP/HTTP exporter pointing at endpoint
// (URL with scheme, e.g. http://loki:3100/otlp). TLS options behave as
// on OtlpExporter; point endpoint at an https:// URL when supplying them.
func (o *Otel) OtlpHttpExporter(
	name, endpoint string,
	// +optional
	caCert *dagger.File,
	// +optional
	clientCert *dagger.File,
	// +optional
	clientKey *dagger.Secret,
) (*Exporter, error) {
	return newOtlpExporter("otlphttp", name, endpoint, caCert, clientCert, clientKey)
}

// newOtlpExporter is the shared constructor behind OtlpExporter and
// OtlpHttpExporter: it validates the name and TLS pairing, renders the
// exporter body (baking in the per-component TLS mount paths when TLS is
// requested), and stashes the cert/key material for the collector to
// mount.
func newOtlpExporter(kind, name, endpoint string, caCert, clientCert *dagger.File, clientKey *dagger.Secret) (*Exporter, error) {
	if err := validateName("name", name); err != nil {
		return nil, err
	}
	if (clientCert == nil) != (clientKey == nil) {
		return nil, fmt.Errorf("exporter %q: clientCert and clientKey must be provided together", name)
	}
	body, err := marshalBody(map[string]any{
		"endpoint": endpoint,
		"tls":      exporterTlsBlock(kind, name, caCert, clientCert, clientKey),
	})
	if err != nil {
		return nil, err
	}
	return &Exporter{
		Kind:       kind,
		Name:       name,
		Body:       body,
		CaCert:     caCert,
		ClientCert: clientCert,
		ClientKey:  clientKey,
	}, nil
}

// exporterTlsBlock renders the exporter's `tls:` map. With no material it
// is the plaintext {insecure: true}; otherwise it references the
// per-component mount paths (see exporterCertDir) that buildService fills.
func exporterTlsBlock(kind, name string, caCert, clientCert *dagger.File, clientKey *dagger.Secret) map[string]any {
	if caCert == nil && clientCert == nil && clientKey == nil {
		return map[string]any{"insecure": true}
	}
	dir := exporterCertDir(kind, name)
	tls := map[string]any{}
	if caCert != nil {
		tls["ca_file"] = dir + "/ca.pem"
	}
	if clientCert != nil {
		tls["cert_file"] = dir + "/cert.pem"
	}
	if clientKey != nil {
		tls["key_file"] = dir + "/key.pem"
	}
	return tls
}

// DebugExporter builds the stdout `debug` exporter at verbosity=detailed.
func (o *Otel) DebugExporter(name string) (*Exporter, error) {
	if err := validateName("name", name); err != nil {
		return nil, err
	}
	body, err := marshalBody(map[string]any{"verbosity": "detailed"})
	if err != nil {
		return nil, err
	}
	return &Exporter{Kind: "debug", Name: name, Body: body}, nil
}

// BatchProcessor builds a batch processor with collector defaults.
func (o *Otel) BatchProcessor(name string) (*Processor, error) {
	if err := validateName("name", name); err != nil {
		return nil, err
	}
	return &Processor{Kind: "batch", Name: name, Body: "{}\n"}, nil
}

// MemoryLimiterProcessor builds a memory_limiter processor with
// conservative defaults (check_interval: 1s, limit_mib: 512). Callers
// needing different thresholds should reach for CustomProcessor.
func (o *Otel) MemoryLimiterProcessor(name string) (*Processor, error) {
	if err := validateName("name", name); err != nil {
		return nil, err
	}
	body, err := marshalBody(map[string]any{
		"check_interval": "1s",
		"limit_mib":      512,
	})
	if err != nil {
		return nil, err
	}
	return &Processor{Kind: "memory_limiter", Name: name, Body: body}, nil
}

// ResourceProcessor builds a no-op resource processor (empty
// attributes list). Callers needing actual attribute upserts should
// reach for CustomProcessor.
func (o *Otel) ResourceProcessor(name string) (*Processor, error) {
	if err := validateName("name", name); err != nil {
		return nil, err
	}
	body, err := marshalBody(map[string]any{
		"attributes": []any{},
	})
	if err != nil {
		return nil, err
	}
	return &Processor{Kind: "resource", Name: name, Body: body}, nil
}

func validateCustom(kind, name, yamlBody string) error {
	if err := validateName("kind", kind); err != nil {
		return err
	}
	if err := validateName("name", name); err != nil {
		return err
	}
	var v any
	if err := yaml.Unmarshal([]byte(yamlBody), &v); err != nil {
		return fmt.Errorf("custom %s/%s: invalid YAML body: %w", kind, name, err)
	}
	return nil
}

// CustomReceiver builds a receiver of arbitrary kind whose body is the
// caller-supplied YAML, spliced verbatim under `receivers.<kind>/<name>`.
func (o *Otel) CustomReceiver(kind, name, yamlBody string) (*Receiver, error) {
	if err := validateCustom(kind, name, yamlBody); err != nil {
		return nil, err
	}
	return &Receiver{Kind: kind, Name: name, Body: yamlBody}, nil
}

// CustomProcessor — see CustomReceiver.
func (o *Otel) CustomProcessor(kind, name, yamlBody string) (*Processor, error) {
	if err := validateCustom(kind, name, yamlBody); err != nil {
		return nil, err
	}
	return &Processor{Kind: kind, Name: name, Body: yamlBody}, nil
}

// CustomExporter — see CustomReceiver.
func (o *Otel) CustomExporter(kind, name, yamlBody string) (*Exporter, error) {
	if err := validateCustom(kind, name, yamlBody); err != nil {
		return nil, err
	}
	return &Exporter{Kind: kind, Name: name, Body: yamlBody}, nil
}

// Pipeline is a single OpenTelemetry Collector pipeline binding a
// signal kind to an ordered set of receivers, processors, and
// exporters. Components are held by reference; the collector
// deduplicates shared components into one top-level entry per
// kind/name when the YAML is rendered.
type Pipeline struct {
	Signal     string
	Name       string
	Receivers  []*Receiver
	Processors []*Processor
	Exporters  []*Exporter
}

func validateSignal(signal string) error {
	switch signal {
	case "logs", "traces", "metrics":
		return nil
	}
	return fmt.Errorf("signal %q: must be one of logs, traces, metrics", signal)
}

// Pipeline builds an empty pipeline for signal (logs|traces|metrics)
// keyed at <signal>/<name> in the rendered config.
func (o *Otel) Pipeline(signal, name string) (*Pipeline, error) {
	if err := validateSignal(signal); err != nil {
		return nil, err
	}
	if err := validateName("name", name); err != nil {
		return nil, err
	}
	return &Pipeline{Signal: signal, Name: name}, nil
}

// DebugPipeline is a pre-wired smoke-test pipeline of
// otlp receiver → batch processor → debug exporter for signal.
// Component names are fixed (`otlp/debug`, `batch/debug`,
// `debug/debug`); the pipeline name is `debug`.
func (o *Otel) DebugPipeline(signal string) (*Pipeline, error) {
	if err := validateSignal(signal); err != nil {
		return nil, err
	}
	r, err := o.OtlpReceiver("debug")
	if err != nil {
		return nil, err
	}
	p, err := o.BatchProcessor("debug")
	if err != nil {
		return nil, err
	}
	e, err := o.DebugExporter("debug")
	if err != nil {
		return nil, err
	}
	return &Pipeline{
		Signal:     signal,
		Name:       "debug",
		Receivers:  []*Receiver{r},
		Processors: []*Processor{p},
		Exporters:  []*Exporter{e},
	}, nil
}

// WithReceiver appends a receiver to the pipeline and returns a new
// pipeline; the receiver is held by reference so it can be deduped
// across pipelines at render time.
func (p *Pipeline) WithReceiver(recv *Receiver) *Pipeline {
	out := *p
	out.Receivers = append(append([]*Receiver{}, p.Receivers...), recv)
	return &out
}

// WithProcessor — see WithReceiver.
func (p *Pipeline) WithProcessor(proc *Processor) *Pipeline {
	out := *p
	out.Processors = append(append([]*Processor{}, p.Processors...), proc)
	return &out
}

// WithExporter — see WithReceiver.
func (p *Pipeline) WithExporter(exp *Exporter) *Pipeline {
	out := *p
	out.Exporters = append(append([]*Exporter{}, p.Exporters...), exp)
	return &out
}

// CoreCollector wraps the otel/opentelemetry-collector image. Its
// public surface is identical to ContribCollector — both share the
// rendering and service-construction helpers below; only the image
// path differs.
type CoreCollector struct {
	Registry     string
	Tag          string
	Override     *dagger.File
	Pipelines    []*Pipeline
	BindingHosts []string
	BindingSvcs  []*dagger.Service
	// +private
	ServerCert *dagger.File
	// +private
	ServerKey *dagger.Secret
	// +private
	ClientCa *dagger.File
}

// Core returns a CoreCollector backed by the
// otel/opentelemetry-collector image at <registry>/<image>:<tag>.
// configFile, when supplied, fully replaces the rendered pipeline
// YAML; the image path is fixed.
func (o *Otel) Core(
	// +default="docker.io"
	registry string,
	// +default="0.130.1"
	tag string,
	// +optional
	configFile *dagger.File,
) *CoreCollector {
	return &CoreCollector{Registry: registry, Tag: tag, Override: configFile}
}

// WithServiceBinding binds a backend service into the collector's
// network so exporter endpoints can reach it by hostname. Repeated
// calls accumulate.
func (c *CoreCollector) WithServiceBinding(host string, svc *dagger.Service) *CoreCollector {
	out := *c
	out.BindingHosts = append(append([]string{}, c.BindingHosts...), host)
	out.BindingSvcs = append(append([]*dagger.Service{}, c.BindingSvcs...), svc)
	return &out
}

// WithPipeline appends a pipeline to the collector. The collector
// dedupes shared components into one top-level entry per kind/name
// at YAML-render time.
func (c *CoreCollector) WithPipeline(p *Pipeline) *CoreCollector {
	out := *c
	out.Pipelines = append(append([]*Pipeline{}, c.Pipelines...), p)
	return &out
}

// WithConfigFile fully replaces the rendered pipeline YAML with the
// supplied file. Pipelines added via WithPipeline are ignored when an
// override is set.
func (c *CoreCollector) WithConfigFile(f *dagger.File) *CoreCollector {
	out := *c
	out.Override = f
	return &out
}

// WithTls enables TLS on both OTLP receivers (gRPC :4317 and HTTP :4318).
// serverCert is the PEM-encoded server certificate and serverKey its
// PEM-encoded PKCS#8 private key; both are mounted into the collector and
// wired into every otlp receiver at render time. After this call
// OtlpHttpEndpoint returns an https:// URL.
func (c *CoreCollector) WithTls(serverCert *dagger.File, serverKey *dagger.Secret) *CoreCollector {
	out := *c
	out.ServerCert = serverCert
	out.ServerKey = serverKey
	return &out
}

// WithMtls requires client certificates signed by clientCa (PEM-encoded)
// on every incoming OTLP connection. Must be combined with WithTls;
// Service returns an error otherwise.
func (c *CoreCollector) WithMtls(clientCa *dagger.File) *CoreCollector {
	out := *c
	out.ClientCa = clientCa
	return &out
}

// ConfigFile returns the file that will be mounted as the collector's
// --config: either the caller-supplied override or the
// pipeline-rendered YAML. Inspecting it does not launch the service.
func (c *CoreCollector) ConfigFile() (*dagger.File, error) {
	return resolveConfigFile(c.Override, c.Pipelines, c.ServerCert != nil, c.ClientCa != nil)
}

// Service returns the running collector. Listens on :4317 (OTLP gRPC)
// and :4318 (OTLP HTTP). Mounts the resolved config (override or
// rendered) when one exists; otherwise launches with no --config flag,
// matching the collector binary's behavior of refusing to start.
func (c *CoreCollector) Service() (*dagger.Service, error) {
	return buildService(coreImage(c.Registry, c.Tag), c.Override, c.Pipelines, c.BindingHosts, c.BindingSvcs, c.ServerCert, c.ServerKey, c.ClientCa)
}

// OtlpGrpcEndpoint returns the host:port of the running collector's
// OTLP/gRPC listener (no scheme).
//
// +cache="never"
func (c *CoreCollector) OtlpGrpcEndpoint(ctx context.Context) (string, error) {
	svc, err := c.Service()
	if err != nil {
		return "", err
	}
	host, err := svc.Hostname(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%d", host, otlpGrpcPort), nil
}

// OtlpHttpEndpoint returns <scheme>://<host>:4318 for the running
// collector's OTLP/HTTP listener. The scheme is https once WithTls has
// been called, http otherwise.
//
// +cache="never"
func (c *CoreCollector) OtlpHttpEndpoint(ctx context.Context) (string, error) {
	svc, err := c.Service()
	if err != nil {
		return "", err
	}
	host, err := svc.Hostname(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s://%s:%d", httpScheme(c.ServerCert), host, otlpHttpPort), nil
}

// renderCollectorYAML composes the full collector config from the
// supplied pipelines. Components keyed at <kind>/<name> are deduped
// across pipelines; component bodies (including Custom* bodies) are
// parsed via yaml.Unmarshal so they splice in structurally rather
// than as quoted scalars.
func renderCollectorYAML(pipelines []*Pipeline, tlsEnabled, mtlsEnabled bool) ([]byte, error) {
	receivers := map[string]any{}
	processors := map[string]any{}
	exporters := map[string]any{}
	pipelineBlock := map[string]any{}

	for _, p := range pipelines {
		var rNames, prNames, eNames []string
		for _, r := range p.Receivers {
			id := r.Kind + "/" + r.Name
			parsed, err := parseBody("receiver", id, r.Body)
			if err != nil {
				return nil, err
			}
			if tlsEnabled && r.Kind == "otlp" {
				if err := injectReceiverTls(parsed, mtlsEnabled); err != nil {
					return nil, fmt.Errorf("otlp receiver %s: %w", id, err)
				}
			}
			if err := dedupParsed("receiver", id, parsed, receivers); err != nil {
				return nil, err
			}
			rNames = append(rNames, id)
		}
		for _, pr := range p.Processors {
			id := pr.Kind + "/" + pr.Name
			if err := dedupComponent("processor", id, pr.Body, processors); err != nil {
				return nil, err
			}
			prNames = append(prNames, id)
		}
		for _, e := range p.Exporters {
			id := e.Kind + "/" + e.Name
			if err := dedupComponent("exporter", id, e.Body, exporters); err != nil {
				return nil, err
			}
			eNames = append(eNames, id)
		}
		entry := map[string]any{}
		if len(rNames) > 0 {
			entry["receivers"] = rNames
		}
		if len(prNames) > 0 {
			entry["processors"] = prNames
		}
		if len(eNames) > 0 {
			entry["exporters"] = eNames
		}
		pipelineBlock[p.Signal+"/"+p.Name] = entry
	}

	root := map[string]any{}
	if len(receivers) > 0 {
		root["receivers"] = receivers
	}
	if len(processors) > 0 {
		root["processors"] = processors
	}
	if len(exporters) > 0 {
		root["exporters"] = exporters
	}
	root["service"] = map[string]any{"pipelines": pipelineBlock}

	return yaml.Marshal(root)
}

// dedupComponent inserts the parsed body for id into dst when the id
// is new, and returns an error if id already maps to a different
// parsed body — surfacing accidental same-id-different-body collisions
// instead of silently keeping whichever was registered first.
func dedupComponent(kind, id, body string, dst map[string]any) error {
	parsed, err := parseBody(kind, id, body)
	if err != nil {
		return err
	}
	return dedupParsed(kind, id, parsed, dst)
}

// dedupParsed inserts an already-parsed body for id into dst, returning
// an error on same-id-different-body collisions. Split from
// dedupComponent so receivers can be TLS-mutated after parsing but before
// deduplication.
func dedupParsed(kind, id string, parsed any, dst map[string]any) error {
	if existing, ok := dst[id]; ok {
		if !reflect.DeepEqual(existing, parsed) {
			return fmt.Errorf("conflicting %s body for %s: same id wired into the pipeline graph with different configs; reuse a single instance instead", kind, id)
		}
		return nil
	}
	dst[id] = parsed
	return nil
}

// injectReceiverTls adds a `tls:` block referencing the collector-level
// server cert/key (and, when mtls is set, the client CA) into each
// configured protocol of a parsed otlp receiver body. Protocols the
// caller did not configure are left untouched.
func injectReceiverTls(parsed any, mtls bool) error {
	m, ok := parsed.(map[string]any)
	if !ok {
		return fmt.Errorf("receiver body is not a mapping")
	}
	protos, ok := m["protocols"].(map[string]any)
	if !ok {
		return fmt.Errorf("receiver body missing protocols mapping")
	}
	injected := false
	for _, proto := range []string{"grpc", "http"} {
		entry, ok := protos[proto].(map[string]any)
		if !ok {
			continue
		}
		entry["tls"] = receiverTlsBlock(mtls)
		injected = true
	}
	if !injected {
		return fmt.Errorf("receiver has no grpc or http protocol to enable TLS on")
	}
	return nil
}

// receiverTlsBlock renders the collector-side `tls:` map pointing at the
// fixed server cert/key mount paths, plus the client CA path for mTLS.
func receiverTlsBlock(mtls bool) map[string]any {
	tls := map[string]any{
		"cert_file": serverCertPath,
		"key_file":  serverKeyPath,
	}
	if mtls {
		tls["client_ca_file"] = clientCaPath
	}
	return tls
}

func parseBody(kind, id, body string) (any, error) {
	var v any
	if err := yaml.Unmarshal([]byte(body), &v); err != nil {
		return nil, fmt.Errorf("parse %s %s body: %w", kind, id, err)
	}
	if v == nil {
		return map[string]any{}, nil
	}
	return v, nil
}

// writeWorkdirFile writes content to a content-addressed subdir of
// the module's scratch workdir and returns it as a *dagger.File. The
// subdir name is derived from a hash of the content so distinct
// outputs land at distinct WorkdirFile paths and identical outputs
// are idempotent across re-entry.
func writeWorkdirFile(name string, content []byte) (*dagger.File, error) {
	sum := sha256.Sum256(content)
	dir := "out-" + hex.EncodeToString(sum[:])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name)

	tmp, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return nil, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return nil, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return nil, err
	}
	return dag.CurrentModule().WorkdirFile(path), nil
}

// httpScheme returns "https" when a server certificate is configured
// (WithTls was called), "http" otherwise.
func httpScheme(serverCert *dagger.File) string {
	if serverCert != nil {
		return "https"
	}
	return "http"
}

// ContribCollector wraps the otel/opentelemetry-collector-contrib
// image. Method set is identical to CoreCollector; only the image
// path differs, so the rendering and service helpers are shared.
type ContribCollector struct {
	Registry     string
	Tag          string
	Override     *dagger.File
	Pipelines    []*Pipeline
	BindingHosts []string
	BindingSvcs  []*dagger.Service
	// +private
	ServerCert *dagger.File
	// +private
	ServerKey *dagger.Secret
	// +private
	ClientCa *dagger.File
}

// Contrib returns a ContribCollector backed by the
// otel/opentelemetry-collector-contrib image. See Core.
func (o *Otel) Contrib(
	// +default="docker.io"
	registry string,
	// +default="0.130.1"
	tag string,
	// +optional
	configFile *dagger.File,
) *ContribCollector {
	return &ContribCollector{Registry: registry, Tag: tag, Override: configFile}
}

// WithServiceBinding — see CoreCollector.WithServiceBinding.
func (c *ContribCollector) WithServiceBinding(host string, svc *dagger.Service) *ContribCollector {
	out := *c
	out.BindingHosts = append(append([]string{}, c.BindingHosts...), host)
	out.BindingSvcs = append(append([]*dagger.Service{}, c.BindingSvcs...), svc)
	return &out
}

// WithPipeline — see CoreCollector.WithPipeline.
func (c *ContribCollector) WithPipeline(p *Pipeline) *ContribCollector {
	out := *c
	out.Pipelines = append(append([]*Pipeline{}, c.Pipelines...), p)
	return &out
}

// WithConfigFile — see CoreCollector.WithConfigFile.
func (c *ContribCollector) WithConfigFile(f *dagger.File) *ContribCollector {
	out := *c
	out.Override = f
	return &out
}

// WithTls — see CoreCollector.WithTls.
func (c *ContribCollector) WithTls(serverCert *dagger.File, serverKey *dagger.Secret) *ContribCollector {
	out := *c
	out.ServerCert = serverCert
	out.ServerKey = serverKey
	return &out
}

// WithMtls — see CoreCollector.WithMtls.
func (c *ContribCollector) WithMtls(clientCa *dagger.File) *ContribCollector {
	out := *c
	out.ClientCa = clientCa
	return &out
}

// ConfigFile — see CoreCollector.ConfigFile.
func (c *ContribCollector) ConfigFile() (*dagger.File, error) {
	return resolveConfigFile(c.Override, c.Pipelines, c.ServerCert != nil, c.ClientCa != nil)
}

// Service — see CoreCollector.Service.
func (c *ContribCollector) Service() (*dagger.Service, error) {
	return buildService(contribImage(c.Registry, c.Tag), c.Override, c.Pipelines, c.BindingHosts, c.BindingSvcs, c.ServerCert, c.ServerKey, c.ClientCa)
}

// OtlpGrpcEndpoint — see CoreCollector.OtlpGrpcEndpoint.
//
// +cache="never"
func (c *ContribCollector) OtlpGrpcEndpoint(ctx context.Context) (string, error) {
	svc, err := c.Service()
	if err != nil {
		return "", err
	}
	host, err := svc.Hostname(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%d", host, otlpGrpcPort), nil
}

// OtlpHttpEndpoint — see CoreCollector.OtlpHttpEndpoint.
//
// +cache="never"
func (c *ContribCollector) OtlpHttpEndpoint(ctx context.Context) (string, error) {
	svc, err := c.Service()
	if err != nil {
		return "", err
	}
	host, err := svc.Hostname(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s://%s:%d", httpScheme(c.ServerCert), host, otlpHttpPort), nil
}
