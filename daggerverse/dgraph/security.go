package main

// ServerSecurity describes how a Dgraph cluster's listeners authenticate
// and encrypt traffic. In this story only PLAINTEXT is supported on
// every listener (client-facing and internal); TLS / mTLS variants land
// in a follow-up.
//
// The empty-but-distinct struct exists so future constructors
// (TlsServerSecurity, MtlsServerSecurity) can land without changing
// Cluster's signature.
type ServerSecurity struct {
	// +private
	Mode string // PLAINTEXT (TLS / MTLS reserved for follow-up)
}

// ClientSecurity describes how a dgo client authenticates to a Dgraph
// Alpha. PLAINTEXT only in this story; TLS / mTLS land in a follow-up.
type ClientSecurity struct {
	// +private
	Mode string // PLAINTEXT (TLS / MTLS reserved for follow-up)
}

// PlaintextServerSecurity returns a ServerSecurity profile configured
// for unencrypted, unauthenticated traffic on every cluster listener.
func (d *Dgraph) PlaintextServerSecurity() *ServerSecurity {
	return &ServerSecurity{Mode: "PLAINTEXT"}
}

// PlaintextClientSecurity returns a ClientSecurity profile configured
// for unencrypted, unauthenticated traffic to the cluster's Alphas.
func (d *Dgraph) PlaintextClientSecurity() *ClientSecurity {
	return &ClientSecurity{Mode: "PLAINTEXT"}
}
