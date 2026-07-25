package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"dagger/tests/internal/dagger"
)

// ----------------------------------------------------------- remote backend

// RemoteBackendRoundTripsState asserts the backend path end to end: an apply
// against a real S3 backend hands back no terraform.tfstate, the backend holds
// the state instead, a second Config reads it back with nothing but the
// backend settings to go on, and a destroy empties it.
//
// The file-carried tests can only prove that WithBackendConfig reaches
// `tofu init`; this one proves the state actually goes somewhere else.
func (t *Tests) RemoteBackendRoundTripsState(ctx context.Context) error {
	b, err := newBackend(ctx, "round-trip")
	if err != nil {
		return err
	}
	defer b.stop(ctx)

	applied, err := pin(ctx, b.withFlags(b.config("remote-state")).Apply())
	if err != nil {
		return fmt.Errorf("Apply against the remote backend: %w", err)
	}
	entries, err := applied.Entries(ctx)
	if err != nil {
		return fmt.Errorf("read the apply directory: %w", err)
	}
	if slices.Contains(entries, stateFileName) {
		return fmt.Errorf("expected no %s from a backend-backed apply, got %v", stateFileName, entries)
	}
	for _, want := range []string{outputsName, applyLogName} {
		if !slices.Contains(entries, want) {
			return fmt.Errorf("expected %s in the apply directory, got %v", want, entries)
		}
	}

	objects, err := b.objects(ctx)
	if err != nil {
		return err
	}
	if !slices.Contains(objects, stateKey) {
		return fmt.Errorf("expected the backend to hold %s, got %v", stateKey, objects)
	}
	state, err := b.state(ctx, stateKey)
	if err != nil {
		return err
	}
	want := []string{"random_integer.port", "random_pet.name"}
	if got := state.addresses(); !slices.Equal(got, want) {
		return fmt.Errorf("expected the backend's state to track %v, got %v", want, got)
	}

	// Nothing but the backend settings travels into this Config — no state
	// file, no artifact from the apply above. Reading the applied outputs back
	// out of it is what proves the backend, not the container, is carrying the
	// state between runs.
	raw, err := b.withFlags(b.config("remote-state")).Outputs(ctx)
	if err != nil {
		return fmt.Errorf("Outputs from the remote backend: %w", err)
	}
	var outputs map[string]outputValue
	if err := json.Unmarshal([]byte(raw), &outputs); err != nil {
		return fmt.Errorf("parse the outputs as JSON (%q): %w", raw, err)
	}
	for _, name := range []string{"name", "port"} {
		if _, ok := outputs[name]; !ok {
			return fmt.Errorf("expected a %s output read back from the backend, got %v", name, outputs)
		}
	}

	destroyed, err := pin(ctx, b.withFlags(b.config("remote-state")).Destroy())
	if err != nil {
		return fmt.Errorf("Destroy against the remote backend: %w", err)
	}
	entries, err = destroyed.Entries(ctx)
	if err != nil {
		return fmt.Errorf("read the destroy directory: %w", err)
	}
	if slices.Contains(entries, stateFileName) {
		return fmt.Errorf("expected no %s from a backend-backed destroy, got %v", stateFileName, entries)
	}

	state, err = b.state(ctx, stateKey)
	if err != nil {
		return err
	}
	if got := state.addresses(); len(got) != 0 {
		return fmt.Errorf("expected the backend's state to be emptied, still tracking %v", got)
	}
	return nil
}

// BackendConfigFileMatchesBackendConfig asserts the file form of the backend
// settings selects the same backend as the individual calls: state written
// through one is found by the other.
//
// A Plan reporting no changes is the assertion, because the only way this
// Config can know there is nothing to do is by having read the state the
// flag-configured apply left behind.
func (t *Tests) BackendConfigFileMatchesBackendConfig(ctx context.Context) error {
	b, err := newBackend(ctx, "config-file")
	if err != nil {
		return err
	}
	defer b.stop(ctx)

	if _, err := pin(ctx, b.withFlags(b.config("remote-state")).Apply()); err != nil {
		return fmt.Errorf("Apply with individual backend settings: %w", err)
	}

	plan, err := pin(ctx, b.withFile(b.config("remote-state")).Plan())
	if err != nil {
		return fmt.Errorf("Plan with a backend settings file: %w", err)
	}
	changes, err := plan.File(changesName).Contents(ctx)
	if err != nil {
		return fmt.Errorf("read %s: %w", changesName, err)
	}
	if changes != "none" {
		text, _ := plan.File(planTextName).Contents(ctx)
		return fmt.Errorf(
			"expected the settings file to select the same backend as the individual settings, got changes=%q:\n%s",
			changes, text)
	}
	return nil
}

// RemoteWorkspacesIsolateState asserts WithWorkspace partitions a remote
// backend: an apply in one workspace is invisible to a plan in another, and
// the two states are distinct objects in the bucket.
//
// The local backend makes this awkward to see — each workspace's state comes
// back as the same terraform.tfstate file in a different directory — which is
// why the isolation is pinned here rather than alongside the file-carried
// workspace test.
func (t *Tests) RemoteWorkspacesIsolateState(ctx context.Context) error {
	b, err := newBackend(ctx, "workspaces")
	if err != nil {
		return err
	}
	defer b.stop(ctx)

	const (
		staging    = "staging"
		production = "production"
	)

	if _, err := pin(ctx, b.withFlags(b.config("remote-state")).WithWorkspace(staging).Apply()); err != nil {
		return fmt.Errorf("Apply on workspace %q: %w", staging, err)
	}

	stagingChanges, err := b.planChanges(ctx, staging)
	if err != nil {
		return err
	}
	if stagingChanges != "none" {
		return fmt.Errorf("expected workspace %q to see its own apply, got changes=%q", staging, stagingChanges)
	}

	productionChanges, err := b.planChanges(ctx, production)
	if err != nil {
		return err
	}
	if productionChanges != "changes" {
		return fmt.Errorf(
			"expected workspace %q to start from an empty state, got changes=%q — the workspaces share state",
			production, productionChanges)
	}

	// The plan verdicts above are read through tofu, which would agree with
	// itself even if both workspaces resolved to one object. The bucket is the
	// independent witness.
	objects, err := b.objects(ctx)
	if err != nil {
		return err
	}
	want := workspaceStateKey(staging)
	if !slices.Contains(objects, want) {
		return fmt.Errorf("expected the backend to hold %s for workspace %q, got %v", want, staging, objects)
	}
	if slices.Contains(objects, stateKey) {
		return fmt.Errorf("expected no default-workspace state at %s, got %v", stateKey, objects)
	}

	state, err := b.state(ctx, want)
	if err != nil {
		return err
	}
	wantAddresses := []string{"random_integer.port", "random_pet.name"}
	if got := state.addresses(); !slices.Equal(got, wantAddresses) {
		return fmt.Errorf("expected workspace %q to track %v, got %v", staging, wantAddresses, got)
	}
	return nil
}

// ConcurrentAppliesDoNotCorruptState asserts two applies racing for the same
// remote state either serialise or fail on the lock — never both go through as
// if the other had not happened.
//
// The two accepted outcomes are asserted separately because which one occurs
// is a matter of timing, not of correctness:
//
//   - one apply fails with tofu's state-lock diagnostic, or
//   - both succeed, and exactly one of them created the resources while the
//     other found them already there.
//
// The second half is what makes this more than a smoke test. A backend without
// working locking also lets both applies succeed — but then both start from an
// empty state, both report resources added, and the loser's work is silently
// dropped when the winner writes its state.
func (t *Tests) ConcurrentAppliesDoNotCorruptState(ctx context.Context) error {
	b, err := newBackend(ctx, "locking")
	if err != nil {
		return err
	}
	defer b.stop(ctx)

	type outcome struct {
		added int
		err   error
	}
	results := make(chan outcome, 2)
	for _, attempt := range []string{"a", "b"} {
		go func() {
			added, err := b.raceApply(ctx, attempt)
			results <- outcome{added: added, err: err}
		}()
	}
	first, second := <-results, <-results

	switch {
	case first.err != nil && second.err != nil:
		return fmt.Errorf("expected at most one apply to fail, both did:\n%v\n%v", first.err, second.err)
	case first.err != nil:
		return expectLockError(first.err)
	case second.err != nil:
		return expectLockError(second.err)
	}

	// Both got through, so they were serialised: the second one to hold the
	// lock must have read the first one's state and found its work already
	// done.
	created := 0
	for _, o := range []outcome{first, second} {
		if o.added > 0 {
			created++
		}
	}
	if created != 1 {
		return fmt.Errorf(
			"expected exactly one of two serialised applies to create the resources, got %d added and %d added — "+
				"both started from an empty state, so one apply's work was overwritten by the other",
			first.added, second.added)
	}

	state, err := b.state(ctx, stateKey)
	if err != nil {
		return err
	}
	want := []string{"random_pet.name", "terraform_data.delay"}
	if got := state.addresses(); !slices.Equal(got, want) {
		return fmt.Errorf("expected the surviving state to track %v, got %v", want, got)
	}
	return nil
}

// ------------------------------------------------------------------ fixture

const (
	// minioImage is the S3-compatible server the backend tests write to. It is
	// the last community MinIO release, pinned so a suite that passes today
	// passes tomorrow.
	minioImage = "minio/minio:RELEASE.2025-09-07T16-13-09Z"

	// mcImage is MinIO's own client. It creates the bucket, and it reads the
	// bucket back from outside tofu — the only way to assert the state landed
	// in the backend rather than merely that tofu did not complain.
	mcImage = "minio/mc:RELEASE.2025-08-13T08-35-41Z"

	minioPort = 9000

	// backendRegion is arbitrary. MinIO ignores it and the s3 backend insists
	// on having one.
	backendRegion = "us-east-1"

	// stateKey is where the remote-state fixtures keep the default workspace's
	// state inside the bucket.
	stateKey = "state/terraform.tfstate"

	// workspaceKeyPrefix is the s3 backend's default prefix for every
	// non-default workspace: its state lands at <prefix>/<workspace>/<key>.
	workspaceKeyPrefix = "env:"
)

// backend is a throwaway S3 backend: a MinIO service, a bucket inside it, and
// the credentials tofu needs to reach both.
//
// Everything a second fixture could collide on — the binding alias, the
// bucket, the credential — is derived from fresh randomness, so the backend
// tests run in parallel with each other under `all`.
type backend struct {
	Svc *dagger.Service
	// Host is the alias the service is bound under, not a hostname the
	// service itself claims; see the note in newBackend.
	Host      string
	Bucket    string
	AccessKey *dagger.Secret
	SecretKey *dagger.Secret
}

// newBackend starts a MinIO service with a randomly generated root credential
// and creates the bucket the fixtures write their state into.
func newBackend(ctx context.Context, label string) (*backend, error) {
	id, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, fmt.Errorf("name the %s backend fixture: %w", label, err)
	}
	suffix := id[:12]

	// The root credential is minted the same way every other secret in this
	// suite is, and travels the same way: as a *dagger.Secret, never as a
	// literal. A throwaway MinIO is no reason to put a password in git.
	_, accessKey, err := testSecret(ctx, label+"-access-key")
	if err != nil {
		return nil, err
	}
	_, secretKey, err := testSecret(ctx, label+"-secret-key")
	if err != nil {
		return nil, err
	}

	b := &backend{
		Host:      "minio-" + suffix,
		Bucket:    "tofu-state-" + suffix,
		AccessKey: accessKey,
		SecretKey: secretKey,
	}
	b.Svc = dag.Container().
		From(minioImage).
		WithSecretVariable("MINIO_ROOT_USER", accessKey).
		WithSecretVariable("MINIO_ROOT_PASSWORD", secretKey).
		WithExposedPort(minioPort).
		// A server never exits, so it belongs in AsService's args rather than
		// a WithExec, which would wait for it forever.
		AsService(dagger.ContainerAsServiceOpts{
			Args: []string{"minio", "server", "/data", "--address", fmt.Sprintf(":%d", minioPort)},
		})
	// Deliberately no WithHostname: a custom hostname is registered in the DNS
	// domain of the client that starts the service, and the container that has
	// to reach this one is assembled inside the opentofu module — a different
	// client, which cannot resolve it. The generated hostname is
	// session-visible, and Host is only ever the alias a binding maps onto it.

	// Started here rather than left to the first WithServiceBinding: a binding
	// holds the service only for the exec that declares it, and this MinIO
	// keeps its data in a scratch container filesystem. A restart between the
	// apply and the plan that is supposed to observe it would take the state
	// with it.
	if _, err := b.Svc.Start(ctx); err != nil {
		return nil, fmt.Errorf("start the MinIO backend: %w", err)
	}
	if _, err := b.mc(ctx, "mb", "--ignore-existing", "local/"+b.Bucket); err != nil {
		return nil, err
	}
	return b, nil
}

// stop tears the service down. Under `all` several backends are alive at once,
// and each is useless the moment its test returns.
func (b *backend) stop(ctx context.Context) {
	_, _ = b.Svc.Stop(ctx)
}

func (b *backend) endpoint() string {
	return fmt.Sprintf("http://%s:%d", b.Host, minioPort)
}

// config binds a fixture to the toolchain with everything the backend needs
// except the backend settings themselves, which withFlags and withFile supply
// in their two different ways.
func (b *backend) config(name string) *dagger.OpentofuConfig {
	return opentofu().
		Config(fixture(name)).
		// Without this the backend is simply unreachable: tofu runs in its own
		// container, and MinIO is a service in the session, not on the
		// internet.
		WithServiceBinding(b.Host, b.Svc).
		WithSecretVariable("AWS_ACCESS_KEY_ID", b.AccessKey).
		WithSecretVariable("AWS_SECRET_ACCESS_KEY", b.SecretKey).
		// The endpoint override is the one setting that cannot travel as a
		// `-backend-config=name=value` scalar — it lives under the s3 backend's
		// nested `endpoints` attribute. Routing it through the AWS SDK's own
		// environment override instead is what lets the flag form and the file
		// form below carry an identical list of settings.
		WithEnvVariable("AWS_ENDPOINT_URL_S3", b.endpoint()).
		WithEnvVariable("AWS_REGION", backendRegion)
}

// settings is the backend configuration both forms render, so the equivalence
// test compares two spellings of one list rather than two lists.
func (b *backend) settings() [][2]string {
	return [][2]string{
		{"bucket", b.Bucket},
		{"key", stateKey},
		{"region", backendRegion},
		// MinIO serves one host, not a bucket-per-subdomain wildcard.
		{"use_path_style", "true"},
		// Locking through a conditional-write lock object in the bucket
		// itself. The alternative, a DynamoDB table, has no counterpart here.
		{"use_lockfile", "true"},
		// The credential is a MinIO root user: there is no STS to validate it
		// against, no account ID to request, and no instance metadata service
		// behind the endpoint.
		{"skip_credentials_validation", "true"},
		{"skip_requesting_account_id", "true"},
		{"skip_metadata_api_check", "true"},
		{"skip_region_validation", "true"},
	}
}

// withFlags renders the settings as individual WithBackendConfig calls.
func (b *backend) withFlags(cfg *dagger.OpentofuConfig) *dagger.OpentofuConfig {
	for _, setting := range b.settings() {
		cfg = cfg.WithBackendConfig(setting[0], setting[1])
	}
	return cfg
}

// withFile renders the same settings as a single backend settings file.
func (b *backend) withFile(cfg *dagger.OpentofuConfig) *dagger.OpentofuConfig {
	var hcl strings.Builder
	for _, setting := range b.settings() {
		fmt.Fprintf(&hcl, "%s = %q\n", setting[0], setting[1])
	}
	return cfg.WithBackendConfigFile(newFile("backend.hcl", hcl.String()))
}

// planChanges plans the remote-state fixture on the named workspace and
// returns the one-word verdict.
func (b *backend) planChanges(ctx context.Context, workspace string) (string, error) {
	plan, err := pin(ctx, b.withFlags(b.config("remote-state")).WithWorkspace(workspace).Plan())
	if err != nil {
		return "", fmt.Errorf("Plan on workspace %q: %w", workspace, err)
	}
	changes, err := plan.File(changesName).Contents(ctx)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", changesName, err)
	}
	return changes, nil
}

// raceApply is one of the two concurrent applies. It returns how many
// resources the apply reported creating.
//
// The plugin cache is off on purpose: it is shared with LOCKED sharing, which
// would serialise the two inits before either reached the backend and leave
// the lock untested.
//
// attempt only distinguishes the two calls from each other. Two byte-identical
// concurrent calls would be one call as far as the engine is concerned, and
// the race would have a single participant.
func (b *backend) raceApply(ctx context.Context, attempt string) (int, error) {
	result, err := pin(ctx, b.withFlags(b.config("remote-state-slow")).
		WithoutPluginCache().
		WithEnvVariable("TOFU_TEST_ATTEMPT", attempt).
		Apply())
	if err != nil {
		return 0, err
	}
	log, err := result.File(applyLogName).Contents(ctx)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", applyLogName, err)
	}
	return addedResources(log)
}

// ------------------------------------------------------------- mc helpers

// mc runs a MinIO client command against the fixture and returns its stdout.
//
// The alias is assembled from secret environment variables inside the
// container rather than passed to `mc alias set`, so the credential never
// enters argv. The run carries a nonce because two identical mc invocations
// would otherwise be one content-addressed exec, and a listing taken before an
// apply would be handed back as the listing after it.
func (b *backend) mc(ctx context.Context, args ...string) (string, error) {
	nonce, err := dag.Random().UUIDV4(ctx)
	if err != nil {
		return "", fmt.Errorf("name an mc run: %w", err)
	}
	script := `set -e
export MC_HOST_local="http://$MC_ACCESS_KEY:$MC_SECRET_KEY@$MC_ADDR"
ready=
for _ in $(seq 1 90); do
  if mc --config-dir /tmp/mc ls local >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
[ -n "$ready" ] || { echo "MinIO at $MC_ADDR never became ready" >&2; exit 1; }
exec mc --config-dir /tmp/mc "$@"`

	out, err := dag.Container().
		From(mcImage).
		WithServiceBinding(b.Host, b.Svc).
		WithSecretVariable("MC_ACCESS_KEY", b.AccessKey).
		WithSecretVariable("MC_SECRET_KEY", b.SecretKey).
		WithEnvVariable("MC_ADDR", fmt.Sprintf("%s:%d", b.Host, minioPort)).
		WithEnvVariable("MC_RUN_NONCE", nonce).
		WithExec(append([]string{"sh", "-c", script, "mc"}, args...)).
		Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("mc %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// objects lists everything in the bucket, sorted, so an assertion does not
// depend on the order MinIO happens to return.
func (b *backend) objects(ctx context.Context) ([]string, error) {
	out, err := b.mc(ctx, "ls", "--recursive", "--json", "local/"+b.Bucket)
	if err != nil {
		return nil, err
	}
	var keys []string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("parse an mc listing entry (%q): %w", line, err)
		}
		if entry.Key != "" {
			keys = append(keys, entry.Key)
		}
	}
	slices.Sort(keys)
	return keys, nil
}

// state reads one state object out of the bucket and decodes it.
func (b *backend) state(ctx context.Context, key string) (stateDocument, error) {
	raw, err := b.mc(ctx, "cat", "local/"+b.Bucket+"/"+key)
	if err != nil {
		return stateDocument{}, err
	}
	return parseState(raw)
}

// ------------------------------------------------------------------ helpers

// workspaceStateKey is where the s3 backend keeps a named workspace's state,
// given this suite never overrides workspace_key_prefix.
func workspaceStateKey(workspace string) string {
	return workspaceKeyPrefix + "/" + workspace + "/" + stateKey
}

// addedResources reads the resource count out of an apply log's closing line,
// which reads `Apply complete! Resources: 2 added, 0 changed, 0 destroyed.`
func addedResources(log string) (int, error) {
	for line := range strings.SplitSeq(log, "\n") {
		_, after, found := strings.Cut(line, "Resources:")
		if !found {
			continue
		}
		count, rest, ok := strings.Cut(strings.TrimSpace(after), " added")
		if !ok || rest == "" {
			continue
		}
		var added int
		if _, err := fmt.Sscanf(count, "%d", &added); err != nil {
			return 0, fmt.Errorf("parse the resource count from %q: %w", line, err)
		}
		return added, nil
	}
	return 0, fmt.Errorf("expected an apply log ending in a resource summary, got:\n%s", log)
}

// expectLockError asserts a failed apply failed for the one reason this test
// accepts: it could not take the state lock. Any other failure — a broken
// backend, a provider error — is a genuine failure of the test.
func expectLockError(err error) error {
	if strings.Contains(err.Error(), "state lock") || strings.Contains(err.Error(), "Lock Info") {
		return nil
	}
	return fmt.Errorf("expected the losing apply to fail on the state lock, got: %v", err)
}
