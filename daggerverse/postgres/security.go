package main

import "dagger/postgres/internal/dagger"

// ServerSecurity describes how a Postgres cluster's client-facing
// listener authenticates and encrypts traffic. Three modes are
// supported:
//
//   - PLAINTEXT — scram-sha-256 password auth over an unencrypted TCP
//     listener.
//   - TLS — one-way TLS: the primary presents a server certificate and
//     clients still authenticate with scram-sha-256. Plaintext TCP is
//     refused.
//   - MTLS — mutual TLS: connecting clients must additionally present a
//     certificate signed by ClientCa (clientcert=verify-full) on top of
//     the password.
//
// The cert material is caller-supplied PEM: PostgreSQL reads it natively
// via ssl_cert_file / ssl_key_file / ssl_ca_file.
type ServerSecurity struct {
	// +private
	Mode string // PLAINTEXT | TLS | MTLS
	// +private
	ServerCert *dagger.File // TLS + MTLS: PEM-encoded leaf server certificate.
	// +private
	ServerKey *dagger.Secret // TLS + MTLS: PEM-encoded PKCS#8 server private key.
	// +private
	ClientCa *dagger.File // MTLS only: PEM-encoded CA that signs accepted client certs.
}

// ClientSecurity describes how a pgx client authenticates to a Postgres
// primary. PLAINTEXT connects over an unencrypted TCP listener; TLS pins
// the server CA (sslmode=verify-full); MTLS additionally presents a
// client certificate + key. The client builds a *tls.Config from this
// PEM material and hands it to pgx via pgconn.Config.TLSConfig.
type ClientSecurity struct {
	// +private
	Mode string // PLAINTEXT | TLS | MTLS
	// +private
	ServerCa *dagger.File // TLS + MTLS: PEM-encoded root used to verify the server.
	// +private
	ClientCert *dagger.File // MTLS only: PEM-encoded leaf client certificate.
	// +private
	ClientKey *dagger.Secret // MTLS only: PEM-encoded PKCS#8 client private key.
}

// PlaintextServerSecurity returns a ServerSecurity profile configured
// for scram-sha-256 password auth over an unencrypted TCP listener.
func (p *Postgres) PlaintextServerSecurity() *ServerSecurity {
	return &ServerSecurity{Mode: "PLAINTEXT"}
}

// TlsServerSecurity returns a ServerSecurity profile that terminates
// one-way TLS on the primary's :5432 listener. serverCert is the PEM
// leaf certificate (its SAN must cover the cluster hostname the client
// dials) and serverKey is the matching PEM PKCS#8 private key. The
// primary starts with `ssl=on` and a pg_hba.conf that accepts only
// `hostssl … scram-sha-256` — plaintext TCP is refused.
func (p *Postgres) TlsServerSecurity(serverCert *dagger.File, serverKey *dagger.Secret) *ServerSecurity {
	return &ServerSecurity{
		Mode:       "TLS",
		ServerCert: serverCert,
		ServerKey:  serverKey,
	}
}

// MtlsServerSecurity returns a ServerSecurity profile that terminates
// mutual TLS. In addition to the server leaf (serverCert and serverKey),
// clientCa is mounted as ssl_ca_file and the pg_hba.conf line carries
// `clientcert=verify-full`, so connecting clients must present a cert
// signed by clientCa AND the correct password.
func (p *Postgres) MtlsServerSecurity(serverCert *dagger.File, serverKey *dagger.Secret, clientCa *dagger.File) *ServerSecurity {
	return &ServerSecurity{
		Mode:       "MTLS",
		ServerCert: serverCert,
		ServerKey:  serverKey,
		ClientCa:   clientCa,
	}
}

// PlaintextClientSecurity returns a ClientSecurity profile configured
// for scram-sha-256 password auth over an unencrypted TCP connection.
func (p *Postgres) PlaintextClientSecurity() *ClientSecurity {
	return &ClientSecurity{Mode: "PLAINTEXT"}
}

// TlsClientSecurity returns a ClientSecurity profile that opens a
// one-way TLS connection and verifies the server against serverCa
// (sslmode=verify-full with the supplied root).
func (p *Postgres) TlsClientSecurity(serverCa *dagger.File) *ClientSecurity {
	return &ClientSecurity{
		Mode:     "TLS",
		ServerCa: serverCa,
	}
}

// MtlsClientSecurity returns a ClientSecurity profile that opens a
// mutual-TLS connection: the server is verified against serverCa and the
// client presents clientCert + clientKey to satisfy the primary's
// clientcert=verify-full requirement.
func (p *Postgres) MtlsClientSecurity(serverCa *dagger.File, clientCert *dagger.File, clientKey *dagger.Secret) *ClientSecurity {
	return &ClientSecurity{
		Mode:       "MTLS",
		ServerCa:   serverCa,
		ClientCert: clientCert,
		ClientKey:  clientKey,
	}
}
