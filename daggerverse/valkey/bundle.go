package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"dagger/valkey/internal/dagger"
)

// bundleEntrypoint is the wrapper script the `valkey/valkey-bundle` image
// ships INSTEAD of the stock `docker-entrypoint.sh`. Booting a bundle
// node through the stock script is the whole trap this constructor
// exists to close: the modules ship as `.so` files under
// /usr/lib/valkey, and nothing loads them unless something composes the
// `--loadmodule` flags. `bundle-docker-entrypoint.sh` is what does that
// — it globs the module directory and appends one flag per module — so a
// node built the stock way comes up perfectly healthy with an empty
// module list, and the first `JSON.SET` fails as an unknown command.
//
// The bundle script is otherwise a superset of the stock one: it drops to
// the `valkey` user the same way and honours a `.conf` positional
// argument, skipping the modules that file already loads.
const bundleEntrypoint = "bundle-docker-entrypoint.sh"

// bundleModules is the set of module names a bundle node must report in
// MODULE LIST before readiness will call it up. These are Valkey's own
// spellings, not the file names: `libjson.so` registers as `json`,
// `libvalkey_bloom.so` as `bf` (matching its `BF.*` command prefix), and
// `libsearch.so` as `search`.
//
// The image also carries `ldap` (an authentication backend, not a data
// type) and every node reports the built-in `lua`; neither is a module
// ecosystem a caller reaches through commands, so neither is required
// here. The list is a floor, not an exact match — a future bundle
// release adding a module must not fail this check.
var bundleModules = []string{"bf", "json", "search"}

// BundleServer spins up a single-node Valkey server from the
// `valkey/valkey-bundle` image: the upstream build that carries the
// module ecosystem — JSON (`JSON.SET` / `JSON.GET`), Bloom (`BF.*`), and
// Search (`FT.*`) — preinstalled.
//
// Image: `<registry>/valkey/valkey-bundle:<tag>`. Everything else — the
// listener modes, `requirepass` auth, the hostname derivation, the
// session-cache semantics of `name` — is identical to Valkey.Server, and
// the returned *Server is the same type with the same method set.
//
// This is a separate constructor rather than a `bundle bool` parameter on
// Valkey.Server so the image choice stays legible at the call site and so
// readiness can assert something extra: a bundle node is not ready
// merely because it answers PING, it is ready when MODULE LIST reports
// the JSON, Bloom, and Search modules. A missing module therefore fails
// the boot, naming what is missing, rather than surfacing much later as
// an unknown-command error from the first `JSON.SET`.
//
// The module commands themselves need no new API: `Client.Do(["JSON.SET",
// ...])` already reaches them, and their replies come back JSON-encoded
// like any other. Typed sugar for JSON / Bloom / Search is deliberately
// not part of this surface.
//
// The `valkey-server` configuration passthrough Valkey.Server offers
// (config file, ACL file, append-only, max-memory, extra args) is not
// wired up here yet — a bundle node is provisioned for what its modules
// can do, and the passthrough is a follow-up.
//
// Rejected inputs are exactly Valkey.Server's, for the same reasons: a
// nil `password`, a nil `clientListenerSecurity`, an incomplete TLS /
// MTLS profile, and an empty `name` for a TLS / MTLS node.
//
// Session-cached on the same terms as Valkey.Server: `name` folds into
// the cache key, so parallel test suites should pass a unique value per
// test and a single test should reuse the returned handle.
//
// +cache="session"
func (v *Valkey) BundleServer(
	ctx context.Context,
	// +default=""
	name string,
	// +default="docker.io"
	registry string,
	// +default="9.1"
	tag string,
	password *dagger.Secret,
	clientListenerSecurity *ServerSecurity,
) (*Server, error) {
	if err := validateServerInputs(name, password, clientListenerSecurity); err != nil {
		return nil, err
	}

	server := buildServer(name, bundleImage(registry, tag), bundleEntrypoint, password, clientListenerSecurity, nil, nil, nil, nil)
	server.RequiredModules = bundleModules
	return server, nil
}

// bundleImage renders the bundle image reference a node boots from. As
// with valkeyImage the repository portion is fixed; only the registry
// and tag are caller-overridable.
func bundleImage(registry, tag string) string {
	return fmt.Sprintf("%s/valkey/valkey-bundle:%s", registry, tag)
}

// requireModules asserts the node has loaded every module it was built to
// carry. It runs once, after the first successful PING, rather than
// inside the readiness retry loop: valkey-server loads its modules before
// it binds the client port, so a node that answers PING has already
// decided which modules it has and retrying could only burn the deadline.
//
// A node with no RequiredModules — every node built by Valkey.Server,
// Valkey.Replication, and Valkey.Cluster — skips the round trip entirely.
func (s *Server) requireModules(ctx context.Context, probe *Client) error {
	if len(s.RequiredModules) == 0 {
		return nil
	}
	loaded, err := probe.loadedModules(ctx)
	if err != nil {
		return fmt.Errorf("valkey %s module list: %w", s.Host, err)
	}
	var missing []string
	for _, want := range s.RequiredModules {
		if _, ok := loaded[want]; !ok {
			missing = append(missing, want)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	present := make([]string, 0, len(loaded))
	for name := range loaded {
		present = append(present, name)
	}
	sort.Strings(present)
	return fmt.Errorf(
		"valkey %s booted without the expected module(s) %s; MODULE LIST reports [%s]",
		s.Host, strings.Join(missing, ", "), strings.Join(present, ", "),
	)
}

// loadedModules returns the set of module names the node reports through
// MODULE LIST. Each element of the reply is a map (RESP3) or a flat
// key/value array (RESP2) describing one module; AsStrMap reads both, and
// `name` is the field that identifies the module to its commands.
func (c *Client) loadedModules(ctx context.Context) (map[string]struct{}, error) {
	client, cleanup, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	entries, err := client.Do(ctx, client.B().Arbitrary("MODULE", "LIST").Build()).ToArray()
	if err != nil {
		return nil, err
	}
	names := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		fields, err := entry.AsStrMap()
		if err != nil {
			return nil, err
		}
		if name := fields["name"]; name != "" {
			names[name] = struct{}{}
		}
	}
	return names, nil
}
