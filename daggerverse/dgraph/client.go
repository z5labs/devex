package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dgraph-io/dgo/v240"
	"github.com/dgraph-io/dgo/v240/protos/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"dagger/dgraph/internal/dagger"
)

// Client is a dgo-backed Dgraph client. Each method opens a fresh
// gRPC connection so the function call is stateless from Dagger's
// perspective.
type Client struct {
	// +private
	GrpcEndpoints []string
	// +private
	SecurityMode string
	// +private
	ServerCa *dagger.File // TLS + MTLS: PEM root used to verify each Alpha.
	// +private
	ClientCert *dagger.File // MTLS: PEM leaf client certificate.
	// +private
	ClientKey *dagger.Secret // MTLS: PEM client private key.
}

// Client constructs a dgo-backed Dgraph client that targets the given
// gRPC endpoints (each of the form `host:9080`). No I/O happens at
// construction time. Works against the local Cluster() topology or any
// reachable remote cluster — Dgraph Cloud, an existing self-hosted
// cluster, anything that speaks the Dgraph gRPC API.
//
// +cache="session"
func (d *Dgraph) Client(grpcEndpoints []string, security *ClientSecurity) *Client {
	return clientFrom(grpcEndpoints, security)
}

func clientFrom(endpoints []string, security *ClientSecurity) *Client {
	c := &Client{
		GrpcEndpoints: endpoints,
		SecurityMode:  "PLAINTEXT",
	}
	if security != nil {
		c.SecurityMode = security.Mode
		c.ServerCa = security.ServerCa
		c.ClientCert = security.ClientCert
		c.ClientKey = security.ClientKey
	}
	return c
}

// dial opens one gRPC ClientConn per endpoint, wraps them in a
// *dgo.Dgraph, and returns a cleanup func that closes every conn.
// The dgo client load-balances among the supplied stubs. For TLS / MTLS
// it materialises a *tls.Config from the client's PEM material first and
// dials each endpoint with a per-host ServerName so certificate
// verification checks the dialed Alpha's SAN.
func (c *Client) dial(ctx context.Context) (*dgo.Dgraph, func(), error) {
	if len(c.GrpcEndpoints) == 0 {
		return nil, nil, fmt.Errorf("client has no gRPC endpoints configured")
	}
	baseTLS, err := c.buildTLSConfig(ctx)
	if err != nil {
		return nil, nil, err
	}
	conns := make([]*grpc.ClientConn, 0, len(c.GrpcEndpoints))
	stubs := make([]api.DgraphClient, 0, len(c.GrpcEndpoints))
	for _, ep := range c.GrpcEndpoints {
		creds := insecure.NewCredentials()
		if baseTLS != nil {
			cfg := baseTLS.Clone()
			cfg.ServerName = hostOf(ep)
			creds = credentials.NewTLS(cfg)
		}
		conn, err := grpc.NewClient(ep, grpc.WithTransportCredentials(creds))
		if err != nil {
			for _, cc := range conns {
				_ = cc.Close()
			}
			return nil, nil, fmt.Errorf("dial %s: %w", ep, err)
		}
		conns = append(conns, conn)
		stubs = append(stubs, api.NewDgraphClient(conn))
	}
	cleanup := func() {
		for _, cc := range conns {
			_ = cc.Close()
		}
	}
	return dgo.NewDgraphClient(stubs...), cleanup, nil
}

// hostOf strips the `:port` suffix from a `host:port` endpoint, yielding
// the ServerName a TLS handshake verifies against the Alpha's cert SAN.
func hostOf(endpoint string) string {
	host, _, found := strings.Cut(endpoint, ":")
	if !found {
		return endpoint
	}
	return host
}

// buildTLSConfig materialises the client-side *tls.Config from the
// client's PEM material. Returns (nil, nil) for PLAINTEXT mode (dial then
// uses insecure credentials). For TLS / MTLS it pins the server CA in
// RootCAs; the per-endpoint ServerName is set by dial. MTLS additionally
// loads the client leaf cert + key so the client can satisfy the
// listener's REQUIREANDVERIFY client-auth requirement.
func (c *Client) buildTLSConfig(ctx context.Context) (*tls.Config, error) {
	if c.SecurityMode == "PLAINTEXT" {
		return nil, nil
	}
	if c.ServerCa == nil {
		return nil, fmt.Errorf("%s client security requires a server CA", securityModeLabel(c.SecurityMode))
	}
	caPEM, err := c.ServerCa.Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("read server CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, fmt.Errorf("server CA contains no PEM certificates")
	}
	cfg := &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	}

	if c.SecurityMode == "MTLS" {
		if c.ClientCert == nil || c.ClientKey == nil {
			return nil, fmt.Errorf("MTLS client security requires both clientCert and clientKey")
		}
		certPEM, err := c.ClientCert.Contents(ctx)
		if err != nil {
			return nil, fmt.Errorf("read client cert: %w", err)
		}
		keyPEM, err := c.ClientKey.Plaintext(ctx)
		if err != nil {
			return nil, fmt.Errorf("read client key: %w", err)
		}
		pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
		if err != nil {
			return nil, fmt.Errorf("load client keypair: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, nil
}

// waitReady retries a trivial read-only schema query until the Alphas
// report a Raft leader (Dgraph returns "server is not ready to accept
// requests" until then). It authenticates with the client's own cert
// material, so it doubles as the readiness probe for mTLS listeners that
// reject an unauthenticated HTTP /health check. Not exported to Dagger.
func (c *Client) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(120 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		dg, cleanup, err := c.dial(ctx)
		if err != nil {
			return err
		}
		_, qErr := dg.NewReadOnlyTxn().Query(ctx, "schema {}")
		cleanup()
		if qErr == nil {
			return nil
		}
		lastErr = qErr
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timeout")
	}
	return fmt.Errorf("cluster not ready: %w", lastErr)
}

// DropAll wipes every predicate, type, and triple from the cluster.
//
// +cache="never"
func (c *Client) DropAll(ctx context.Context) error {
	dg, cleanup, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	return dg.Alter(ctx, &api.Operation{DropAll: true})
}

// AlterSchema applies the given DQL schema (predicate definitions,
// types, indexes) to the cluster.
//
// +cache="never"
func (c *Client) AlterSchema(ctx context.Context, schema string) error {
	dg, cleanup, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	return dg.Alter(ctx, &api.Operation{Schema: schema})
}

// Mutate applies a JSON set mutation to the cluster. setJson is the
// JSON-encoded payload (e.g. `{"name":"Alice"}` or `{"set":[...]}` —
// dgo's SetJson field is the raw value array form). If commit is true
// the mutation commits as a single transaction; if false the
// transaction is discarded (dry run) and no triples are persisted.
//
// Returns the assigned-UIDs JSON object (`{"<blank-node>":"<uid>"}`)
// as a string.
//
// +cache="never"
func (c *Client) Mutate(ctx context.Context, setJson string, commit bool) (string, error) {
	dg, cleanup, err := c.dial(ctx)
	if err != nil {
		return "", err
	}
	defer cleanup()

	txn := dg.NewTxn()
	defer txn.Discard(ctx)

	resp, err := txn.Mutate(ctx, &api.Mutation{
		SetJson:   []byte(setJson),
		CommitNow: commit,
	})
	if err != nil {
		return "", err
	}
	uids := resp.GetUids()
	if uids == nil {
		uids = map[string]string{}
	}
	b, err := json.Marshal(uids)
	if err != nil {
		return "", fmt.Errorf("marshal uids: %w", err)
	}
	return string(b), nil
}

// RunQuery runs a DQL read-only query and returns the response JSON
// body verbatim (the `data` object Dgraph returns).
//
// Named RunQuery rather than Query because Dagger Go SDK codegen
// allocates a struct field named after the lowercase method name to
// cache the result, and `query` collides with the always-present
// querybuilder field on every generated object type. RunQuery sidesteps
// the collision (`runQuery` is unique) while preserving the verb-noun
// shape callers expect.
//
// +cache="never"
func (c *Client) RunQuery(ctx context.Context, dql string) (string, error) {
	dg, cleanup, err := c.dial(ctx)
	if err != nil {
		return "", err
	}
	defer cleanup()

	resp, err := dg.NewReadOnlyTxn().Query(ctx, dql)
	if err != nil {
		return "", err
	}
	return string(resp.GetJson()), nil
}

// QueryWithVars runs a parameterised DQL query and returns the
// response JSON body verbatim. varsJson is a JSON-encoded string-to-
// string map of variable bindings (e.g. `{"$name":"Alice"}`); Dagger's
// function signatures don't support Go map parameters so the map is
// passed as JSON across the module boundary.
//
// +cache="never"
func (c *Client) QueryWithVars(ctx context.Context, dql string, varsJson string) (string, error) {
	vars := map[string]string{}
	if varsJson != "" {
		if err := json.Unmarshal([]byte(varsJson), &vars); err != nil {
			return "", fmt.Errorf("parse varsJson: %w", err)
		}
	}

	dg, cleanup, err := c.dial(ctx)
	if err != nil {
		return "", err
	}
	defer cleanup()

	resp, err := dg.NewReadOnlyTxn().QueryWithVars(ctx, dql, vars)
	if err != nil {
		return "", err
	}
	return string(resp.GetJson()), nil
}
