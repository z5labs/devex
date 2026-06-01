package main

// ServerSecurity describes how a Postgres cluster's client-facing
// listener authenticates and encrypts traffic. In this story only
// PLAINTEXT is supported (scram-sha-256 password auth over an
// unencrypted TCP listener); TLS / mTLS variants land in a follow-up.
//
// The empty-but-distinct struct exists so future constructors
// (TlsServerSecurity, MtlsServerSecurity) can land without changing
// Cluster's signature.
type ServerSecurity struct {
	// +private
	Mode string // PLAINTEXT (TLS / MTLS reserved for follow-up)
}

// ClientSecurity describes how a pgx client authenticates to a Postgres
// primary. PLAINTEXT only in this story; TLS / mTLS land in a
// follow-up.
type ClientSecurity struct {
	// +private
	Mode string // PLAINTEXT (TLS / MTLS reserved for follow-up)
}

// PlaintextServerSecurity returns a ServerSecurity profile configured
// for scram-sha-256 password auth over an unencrypted TCP listener.
func (p *Postgres) PlaintextServerSecurity() *ServerSecurity {
	return &ServerSecurity{Mode: "PLAINTEXT"}
}

// PlaintextClientSecurity returns a ClientSecurity profile configured
// for scram-sha-256 password auth over an unencrypted TCP connection.
func (p *Postgres) PlaintextClientSecurity() *ClientSecurity {
	return &ClientSecurity{Mode: "PLAINTEXT"}
}
