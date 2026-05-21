package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dgraph-io/dgo/v240"
	"github.com/dgraph-io/dgo/v240/protos/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client is a dgo-backed Dgraph client. Each method opens a fresh
// gRPC connection so the function call is stateless from Dagger's
// perspective.
type Client struct {
	// +private
	GrpcEndpoints []string
	// +private
	SecurityMode string
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
	}
	return c
}

// dial opens one gRPC ClientConn per endpoint, wraps them in a
// *dgo.Dgraph, and returns a cleanup func that closes every conn.
// The dgo client load-balances among the supplied stubs.
func (c *Client) dial() (*dgo.Dgraph, func(), error) {
	if c.SecurityMode != "PLAINTEXT" {
		return nil, nil, fmt.Errorf(
			"only PLAINTEXT client security is supported in this story, got %q; TLS / mTLS land in a follow-up",
			c.SecurityMode,
		)
	}
	if len(c.GrpcEndpoints) == 0 {
		return nil, nil, fmt.Errorf("client has no gRPC endpoints configured")
	}
	conns := make([]*grpc.ClientConn, 0, len(c.GrpcEndpoints))
	stubs := make([]api.DgraphClient, 0, len(c.GrpcEndpoints))
	for _, ep := range c.GrpcEndpoints {
		conn, err := grpc.NewClient(ep,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
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

// DropAll wipes every predicate, type, and triple from the cluster.
//
// +cache="never"
func (c *Client) DropAll(ctx context.Context) error {
	dg, cleanup, err := c.dial()
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
	dg, cleanup, err := c.dial()
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
	dg, cleanup, err := c.dial()
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
	dg, cleanup, err := c.dial()
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

	dg, cleanup, err := c.dial()
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
