package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/valkey-io/valkey-go"

	"dagger/valkey/internal/dagger"
)

// defaultUser is the ACL user `requirepass` sets the password of. Valkey
// ships exactly one user out of the box and `requirepass` is shorthand
// for setting its password, so every client in this story authenticates
// as `default`.
const defaultUser = "default"

// scanCount is the COUNT hint handed to each SCAN call in Keys. It is a
// hint, not a page size — the server may return more or fewer — so Keys
// always walks the cursor to exhaustion regardless.
const scanCount = 1000

// Client is a valkey-go backed Valkey client. Each method opens a fresh
// connection so the function call is stateless from Dagger's
// perspective; ApplyFile is the exception — it runs every command on one
// connection.
type Client struct {
	// +private
	Host string
	// +private
	Port int
	// +private
	UserName string
	// +private
	Pass *dagger.Secret
	// +private
	Db int
	// +private
	SecurityMode string
	// +private
	ServerCa *dagger.File // TLS + MTLS: PEM root used to verify the server.
	// +private
	ClientCert *dagger.File // MTLS: PEM leaf client certificate.
	// +private
	ClientKey *dagger.Secret // MTLS: PEM PKCS#8 client private key.
}

// Client constructs a valkey-go backed client targeting host:port with
// the given user, password, and logical database. No I/O happens at
// construction time. Works against the local Server() topology or any
// reachable remote Valkey — ElastiCache Serverless, MemoryDB, an
// existing self-hosted node, anything that speaks the Valkey/Redis wire
// protocol with password auth.
//
// +cache="session"
func (v *Valkey) Client(
	host string,
	// +default=6379
	port int,
	// +default="default"
	user string,
	password *dagger.Secret,
	// +default=0
	db int,
	security *ClientSecurity,
) *Client {
	return clientFrom(host, port, user, password, db, security)
}

func clientFrom(host string, port int, user string, password *dagger.Secret, db int, security *ClientSecurity) *Client {
	c := &Client{
		Host:         host,
		Port:         port,
		UserName:     user,
		Pass:         password,
		Db:           db,
		SecurityMode: "PLAINTEXT",
	}
	if security != nil {
		c.SecurityMode = security.Mode
		c.ServerCa = security.ServerCa
		c.ClientCert = security.ClientCert
		c.ClientKey = security.ClientKey
	}
	return c
}

// dial opens one valkey-go client using the stored credentials and
// returns a cleanup func that closes it. Callers must defer the cleanup.
// valkey-go connects eagerly, so a dial failure (refused, WRONGPASS,
// LOADING) surfaces here rather than on first command.
//
// ForceSingleClient skips the CLUSTER SLOTS probe valkey-go would
// otherwise run to auto-detect a cluster; this story only ever targets a
// standalone node. DisableCache turns off client-side caching, which
// would otherwise let a second read of the same key answer from a local
// tracking cache — exactly the stale read `+cache="never"` exists to
// prevent.
func (c *Client) dial(ctx context.Context) (valkey.Client, func(), error) {
	if c.Pass == nil {
		return nil, nil, fmt.Errorf("client has no password configured")
	}
	password, err := c.Pass.Plaintext(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read password: %w", err)
	}
	tlsCfg, err := c.buildTLSConfig(ctx)
	if err != nil {
		return nil, nil, err
	}
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress:       []string{addr},
		Username:          c.UserName,
		Password:          password,
		SelectDB:          c.Db,
		TLSConfig:         tlsCfg,
		DisableCache:      true,
		ForceSingleClient: true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("connect %s: %w", addr, err)
	}
	return client, client.Close, nil
}

// buildTLSConfig materialises the client-side *tls.Config from the
// client's PEM material. Returns (nil, nil) for PLAINTEXT mode. For TLS
// it pins RootCAs to the supplied server CA and sets ServerName to the
// dialed host (the SAN valkey-go verifies against); MTLS additionally
// loads the client leaf + key so the node's `tls-auth-clients yes` is
// satisfied.
func (c *Client) buildTLSConfig(ctx context.Context) (*tls.Config, error) {
	if c.SecurityMode == "PLAINTEXT" {
		return nil, nil
	}
	if c.ServerCa == nil {
		return nil, fmt.Errorf("%s client security requires a server CA", c.SecurityMode)
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
		ServerName: c.Host,
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

// Ping opens a connection and verifies the node is reachable and
// accepting authenticated commands.
//
// +cache="never"
func (c *Client) Ping(ctx context.Context) error {
	client, cleanup, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	return client.Do(ctx, client.B().Ping().Build()).Error()
}

// Do runs an arbitrary command — `["SET", "k", "v"]`, `["GET", "k"]`,
// `["LPUSH", "l", "a", "b"]` — and returns the reply JSON-encoded, so
// the RESP type survives the round trip: a status reply is `"OK"`, an
// integer is `1`, a bulk string is `"v"`, an array is `["a","b"]`, and a
// nil reply is `null` (never `""`).
//
// This is the escape hatch: every other method on Client is expressible
// through it. The reply comes back as a string rather than a
// *dagger.File because a single command reply is small and a core scalar
// keeps `dagger call do --args=GET,foo` readable.
//
// +cache="never"
func (c *Client) Do(ctx context.Context, args []string) (string, error) {
	cmd, err := arbitraryCommand(args)
	if err != nil {
		return "", err
	}

	client, cleanup, err := c.dial(ctx)
	if err != nil {
		return "", err
	}
	defer cleanup()

	return encodeReply(client.Do(ctx, cmd(client)))
}

// arbitraryCommand validates a caller-supplied argv and returns a
// builder closure for it. valkey-go's Arbitrary builder *panics* on an
// empty command and on the SUBSCRIBE family (whose replies arrive out of
// band and would hang a request/response call), so both are turned into
// errors before they can take down the module runtime.
func arbitraryCommand(args []string) (func(valkey.Client) valkey.Completed, error) {
	if len(args) == 0 || args[0] == "" {
		return nil, fmt.Errorf("args must not be empty; pass at least a command name, e.g. --args=GET,mykey")
	}
	if strings.HasSuffix(strings.ToUpper(args[0]), "SUBSCRIBE") {
		return nil, fmt.Errorf("%s is not supported: its replies arrive out of band, not as a command reply", strings.ToUpper(args[0]))
	}
	return func(client valkey.Client) valkey.Completed {
		return client.B().Arbitrary(args[0]).Args(args[1:]...).Build()
	}, nil
}

// encodeReply renders a command reply as JSON. A nil reply becomes
// `null` rather than an error, so callers can distinguish "no such key"
// from a stored empty string.
func encodeReply(res valkey.ValkeyResult) (string, error) {
	if err := res.Error(); err != nil {
		if valkey.IsValkeyNil(err) {
			return "null", nil
		}
		return "", err
	}
	v, err := res.ToAny()
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal reply: %w", err)
	}
	return string(b), nil
}

// Get returns the string value stored at key. A missing key is an error,
// not an empty string — an empty string is itself a legitimate stored
// value and must stay distinguishable from absence.
//
// +cache="never"
func (c *Client) Get(ctx context.Context, key string) (string, error) {
	client, cleanup, err := c.dial(ctx)
	if err != nil {
		return "", err
	}
	defer cleanup()

	v, err := client.Do(ctx, client.B().Get().Key(key).Build()).ToString()
	if err != nil {
		if valkey.IsValkeyNil(err) {
			return "", fmt.Errorf("key %q does not exist", key)
		}
		return "", err
	}
	return v, nil
}

// Set stores value at key. ttl is a Go duration string (`"250ms"`,
// `"30s"`, `"5m"`); empty means no expiry.
//
// +cache="never"
func (c *Client) Set(
	ctx context.Context,
	key string,
	value string,
	// +optional
	ttl string,
) error {
	var expiry time.Duration
	if ttl != "" {
		d, err := time.ParseDuration(ttl)
		if err != nil {
			return fmt.Errorf("parse ttl %q: %w", ttl, err)
		}
		if d <= 0 {
			return fmt.Errorf("ttl %q must be positive", ttl)
		}
		expiry = d
	}

	client, cleanup, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	// Px carries millisecond precision; Ex would silently floor a
	// sub-second ttl to 0 and make the key expire immediately.
	cmd := client.B().Set().Key(key).Value(value)
	if expiry > 0 {
		return client.Do(ctx, cmd.Px(expiry).Build()).Error()
	}
	return client.Do(ctx, cmd.Build()).Error()
}

// Del removes the given keys and returns how many actually existed.
//
// +cache="never"
func (c *Client) Del(ctx context.Context, keys []string) (int, error) {
	if len(keys) == 0 {
		return 0, fmt.Errorf("keys must not be empty")
	}

	client, cleanup, err := c.dial(ctx)
	if err != nil {
		return 0, err
	}
	defer cleanup()

	n, err := client.Do(ctx, client.B().Del().Key(keys...).Build()).ToInt64()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// Keys returns every key matching a glob pattern (`"*"` for all).
//
// It is SCAN-backed rather than KEYS-backed — KEYS blocks the server for
// the whole sweep — and walks the cursor to exhaustion, so the result is
// the complete match set and not just SCAN's first page.
//
// +cache="never"
func (c *Client) Keys(ctx context.Context, pattern string) ([]string, error) {
	client, cleanup, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	out := make([]string, 0)
	var cursor uint64
	for {
		entry, err := client.Do(ctx,
			client.B().Scan().Cursor(cursor).Match(pattern).Count(scanCount).Build(),
		).AsScanEntry()
		if err != nil {
			return nil, err
		}
		out = append(out, entry.Elements...)
		cursor = entry.Cursor
		if cursor == 0 {
			return out, nil
		}
	}
}

// ApplyFile reads a file of Valkey commands — one per line, in
// valkey-cli syntax — and runs them in order on a single connection.
// This is the fixture-seeding path.
//
// Blank lines and `#` comment lines are skipped. Arguments are split on
// whitespace, with single- and double-quoted runs kept intact so values
// containing spaces survive; `\` escapes inside double quotes. A command
// that fails aborts the run and reports the offending line number.
//
// +cache="never"
func (c *Client) ApplyFile(ctx context.Context, file *dagger.File) error {
	contents, err := file.Contents(ctx)
	if err != nil {
		return fmt.Errorf("read command file: %w", err)
	}

	type lineCmd struct {
		num   int
		build func(valkey.Client) valkey.Completed
	}
	cmds := make([]lineCmd, 0)
	for i, line := range strings.Split(contents, "\n") {
		args, err := splitCommand(line)
		if err != nil {
			return fmt.Errorf("line %d: %w", i+1, err)
		}
		if len(args) == 0 {
			continue
		}
		build, err := arbitraryCommand(args)
		if err != nil {
			return fmt.Errorf("line %d: %w", i+1, err)
		}
		cmds = append(cmds, lineCmd{num: i + 1, build: build})
	}

	client, cleanup, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	for _, cmd := range cmds {
		if err := client.Do(ctx, cmd.build(client)).Error(); err != nil {
			return fmt.Errorf("line %d: %w", cmd.num, err)
		}
	}
	return nil
}

// splitCommand tokenises one line of a command file into argv. It
// returns an empty slice for blank lines and `#` comments. Quoting
// follows valkey-cli: a `'...'` run is literal, a `"..."` run honours
// backslash escapes, and quotes may open mid-token (`k="a b"` is one
// argument).
func splitCommand(line string) ([]string, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return nil, nil
	}

	var (
		args    []string
		cur     strings.Builder
		inTok   bool
		quote   byte // 0, '\'' or '"'
		escaped bool
	)
	flush := func() {
		args = append(args, cur.String())
		cur.Reset()
		inTok = false
	}
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case escaped:
			cur.WriteByte(ch)
			escaped = false
		case quote == '"' && ch == '\\':
			escaped = true
		case quote != 0 && ch == quote:
			quote = 0
		case quote != 0:
			cur.WriteByte(ch)
		case ch == '\'' || ch == '"':
			quote = ch
			inTok = true
		case ch == ' ' || ch == '\t' || ch == '\r':
			if inTok {
				flush()
			}
		default:
			cur.WriteByte(ch)
			inTok = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote", quote)
	}
	if escaped {
		return nil, fmt.Errorf("trailing backslash")
	}
	if inTok {
		flush()
	}
	return args, nil
}

// Info returns the server's INFO output. An empty section returns the
// default set; pass a section name (`"server"`, `"replication"`,
// `"keyspace"`, …) to narrow it.
//
// +cache="never"
func (c *Client) Info(
	ctx context.Context,
	// +default=""
	section string,
) (string, error) {
	client, cleanup, err := c.dial(ctx)
	if err != nil {
		return "", err
	}
	defer cleanup()

	cmd := client.B().Info()
	if section != "" {
		return client.Do(ctx, cmd.Section(section).Build()).ToString()
	}
	return client.Do(ctx, cmd.Build()).ToString()
}

// DbSize returns the number of keys in the client's logical database.
//
// +cache="never"
func (c *Client) DbSize(ctx context.Context) (int, error) {
	client, cleanup, err := c.dial(ctx)
	if err != nil {
		return 0, err
	}
	defer cleanup()

	n, err := client.Do(ctx, client.B().Dbsize().Build()).ToInt64()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// FlushAll removes every key from every logical database on the node.
//
// +cache="never"
func (c *Client) FlushAll(ctx context.Context) error {
	client, cleanup, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	return client.Do(ctx, client.B().Flushall().Build()).Error()
}
