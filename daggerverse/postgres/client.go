package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"

	"dagger/postgres/internal/dagger"
)

// Client is a pgx-backed PostgreSQL client. Each method opens a fresh
// connection so the function call is stateless from Dagger's
// perspective; ApplyFile is the exception — it runs every statement on
// one connection.
type Client struct {
	// +private
	Host string
	// +private
	Port int
	// +private
	UserName string
	// +private
	DbName string
	// +private
	Pass *dagger.Secret
	// +private
	SecurityMode string
	// +private
	ServerCa *dagger.File // TLS + MTLS: PEM root used to verify the server.
	// +private
	ClientCert *dagger.File // MTLS: PEM leaf client certificate.
	// +private
	ClientKey *dagger.Secret // MTLS: PEM PKCS#8 client private key.
}

// Client constructs a pgx-backed PostgreSQL client targeting host:port
// with the given role, database, and password. No I/O happens at
// construction time. Works against the local Cluster() topology or any
// reachable remote PostgreSQL — AWS RDS, Cloud SQL, an existing
// self-hosted primary, anything that speaks the PostgreSQL wire
// protocol with scram-sha-256 password auth.
//
// +cache="session"
func (p *Postgres) Client(
	host string,
	// +default=5432
	port int,
	user string,
	db string,
	password *dagger.Secret,
	security *ClientSecurity,
) *Client {
	return clientFrom(host, port, user, db, password, security)
}

func clientFrom(host string, port int, user, db string, password *dagger.Secret, security *ClientSecurity) *Client {
	c := &Client{
		Host:         host,
		Port:         port,
		UserName:     user,
		DbName:       db,
		Pass:         password,
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

// dial opens one pgx connection using the client's stored credentials
// and returns a cleanup func that closes it. Callers must defer the
// cleanup. For TLS / MTLS clients it materialises a *tls.Config from the
// client's PEM material first.
func (c *Client) dial(ctx context.Context) (*pgx.Conn, func(), error) {
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
	conn, err := pgConnect(ctx, c.Host, c.Port, c.UserName, c.DbName, password, tlsCfg)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = conn.Close(context.Background()) }
	return conn, cleanup, nil
}

// buildTLSConfig materialises the client-side *tls.Config from the
// client's PEM material. Returns (nil, nil) for PLAINTEXT mode (pgConnect
// then uses sslmode=disable). For TLS / MTLS it pins the server CA in
// RootCAs and sets ServerName to the dialed host — yielding
// sslmode=verify-full semantics. MTLS additionally loads the client
// leaf cert + key so the client can satisfy clientcert=verify-full.
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

// pgConnect opens a single pgx connection. With a nil tlsCfg it dials a
// plaintext TCP listener (sslmode=disable) — scram-sha-256 password auth
// happens independently of transport encryption. With a tlsCfg it dials
// over TLS by handing the config to pgconn via pgconn.Config.TLSConfig
// and clearing the fallback chain so there is no silent downgrade to
// plaintext.
func pgConnect(ctx context.Context, host string, port int, user, db, password string, tlsCfg *tls.Config) (*pgx.Conn, error) {
	if tlsCfg == nil {
		dsn := fmt.Sprintf(
			"host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
			dsnEscape(host), port, dsnEscape(user), dsnEscape(password), dsnEscape(db),
		)
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			return nil, fmt.Errorf("connect %s:%d: %w", host, port, err)
		}
		return conn, nil
	}

	dsn := fmt.Sprintf(
		"host=%s port=%d user=%s dbname=%s sslmode=verify-full",
		dsnEscape(host), port, dsnEscape(user), dsnEscape(db),
	)
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse config %s:%d: %w", host, port, err)
	}
	cfg.Password = password
	cfg.TLSConfig = tlsCfg
	cfg.Fallbacks = nil
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect %s:%d: %w", host, port, err)
	}
	return conn, nil
}

// dsnEscape renders a value for a libpq keyword/value DSN. Values
// containing a space, quote, backslash, or `=` must be single-quoted
// with `'` and `\` backslash-escaped; everything else passes through.
// This keeps caller-supplied passwords with special characters from
// corrupting the connection string.
func dsnEscape(v string) string {
	if v == "" {
		return "''"
	}
	if !strings.ContainsAny(v, " '\\=") {
		return v
	}
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `'`, `\'`)
	return "'" + v + "'"
}

// Ping opens a connection and verifies the server is reachable and
// accepting authenticated queries.
//
// +cache="never"
func (c *Client) Ping(ctx context.Context) error {
	conn, cleanup, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	return conn.Ping(ctx)
}

// Exec runs a SQL statement and returns the affected-row count
// (INSERT/UPDATE/DELETE rows, or 0 for DDL).
//
// +cache="never"
func (c *Client) Exec(ctx context.Context, sql string) (int64, error) {
	conn, cleanup, err := c.dial(ctx)
	if err != nil {
		return 0, err
	}
	defer cleanup()
	tag, err := conn.Exec(ctx, sql)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// Scalar runs a query and returns the first column of the first row as
// a string. Errors if the query returns zero rows, or if that first
// column is SQL NULL (rather than silently returning the string
// "<nil>").
//
// +cache="never"
func (c *Client) Scalar(ctx context.Context, sql string) (string, error) {
	conn, cleanup, err := c.dial(ctx)
	if err != nil {
		return "", err
	}
	defer cleanup()

	rows, err := conn.Query(ctx, sql)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("scalar query returned zero rows")
	}
	vals, err := rows.Values()
	if err != nil {
		return "", err
	}
	if len(vals) == 0 {
		return "", fmt.Errorf("scalar query returned a row with no columns")
	}
	if vals[0] == nil {
		return "", fmt.Errorf("scalar query returned SQL NULL in the first column")
	}
	return fmt.Sprint(vals[0]), nil
}

// ApplyFile reads a `.sql` file and runs its statements on a single
// connection, in order. Statements are split on `;` outside of single-
// and double-quoted strings, line (`--`) and block (`/* */`) comments,
// and dollar-quoted strings (`$$ ... $$` / `$tag$ ... $tag$`).
//
// +cache="never"
func (c *Client) ApplyFile(ctx context.Context, file *dagger.File) error {
	contents, err := file.Contents(ctx)
	if err != nil {
		return fmt.Errorf("read sql file: %w", err)
	}

	conn, cleanup, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	for i, stmt := range splitSQL(contents) {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("statement %d: %w", i+1, err)
		}
	}
	return nil
}

// QueryJSON runs a query and returns the result set as a *dagger.File
// containing a JSON array of objects — one per row, keyed by column
// name.
//
// +cache="never"
func (c *Client) QueryJSON(ctx context.Context, sql string) (*dagger.File, error) {
	conn, cleanup, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	rows, err := conn.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols := rows.FieldDescriptions()
	out := make([]map[string]any, 0)
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, fd := range cols {
			row[string(fd.Name)] = vals[i]
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal rows: %w", err)
	}
	return writeWorkdirFile("query.json", b)
}

// writeWorkdirFile writes content to a content-addressed subdir of the
// module's scratch workdir and returns it as a *dagger.File. The subdir
// name is derived from a hash of the content, so distinct outputs land
// at distinct WorkdirFile paths and identical outputs are idempotent.
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
	if err := tmp.Chmod(0o600); err != nil {
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
