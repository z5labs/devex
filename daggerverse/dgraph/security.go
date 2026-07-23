package main

import (
	"fmt"

	"dagger/dgraph/internal/dagger"
)

// ServerSecurity describes how a Dgraph cluster's listeners authenticate
// and encrypt traffic. Three modes are supported, applied uniformly to
// every Alpha (client-facing HTTP :8080 / gRPC :9080) and Zero (admin
// HTTP :6080) listener via Dgraph's unified `--tls` superflag:
//
//   - PLAINTEXT — unencrypted, unauthenticated traffic on every listener.
//   - TLS — one-way TLS: each node presents a server certificate and
//     clients verify it against the CA they hold. Dgraph's
//     `client-auth-type=VERIFYIFGIVEN` (the default) means clients are
//     not required to present a certificate.
//   - MTLS — mutual TLS: connecting clients must additionally present a
//     certificate signed by ClientCa
//     (`client-auth-type=REQUIREANDVERIFY`).
//
// The cert material is caller-supplied PEM: Dgraph reads it natively via
// the `--tls "server-cert=…; server-key=…; ca-cert=…"` superflag.
type ServerSecurity struct {
	// +private
	Mode string // PLAINTEXT | TLS | MTLS
	// +private
	ServerCert *dagger.File // TLS + MTLS: PEM-encoded leaf server certificate.
	// +private
	ServerKey *dagger.Secret // TLS + MTLS: PEM-encoded server private key.
	// +private
	ClientCa *dagger.File // MTLS only: PEM CA that signs accepted client certs.
}

// ClientSecurity describes how a dgo client authenticates to a Dgraph
// Alpha. PLAINTEXT dials an unencrypted gRPC listener; TLS pins the
// server CA and verifies the Alpha's certificate; MTLS additionally
// presents a client certificate + key. The client builds a *tls.Config
// from this PEM material and hands it to dgo via
// grpc.WithTransportCredentials(credentials.NewTLS(cfg)).
type ClientSecurity struct {
	// +private
	Mode string // PLAINTEXT | TLS | MTLS
	// +private
	ServerCa *dagger.File // TLS + MTLS: PEM root used to verify the Alpha.
	// +private
	ClientCert *dagger.File // MTLS only: PEM-encoded leaf client certificate.
	// +private
	ClientKey *dagger.Secret // MTLS only: PEM-encoded client private key.
}

// PlaintextServerSecurity returns a ServerSecurity profile configured
// for unencrypted, unauthenticated traffic on every cluster listener.
func (d *Dgraph) PlaintextServerSecurity() *ServerSecurity {
	return &ServerSecurity{Mode: "PLAINTEXT"}
}

// TlsServerSecurity returns a ServerSecurity profile that terminates
// one-way TLS on every Dgraph listener. serverCert is the PEM leaf
// certificate (its SAN must cover the Alpha / Zero hostnames the client
// dials) and serverKey is the matching PEM private key. Dgraph starts
// each node with `--tls "server-cert=…; server-key=…"`; the default
// `client-auth-type=VERIFYIFGIVEN` means clients need not present a
// certificate, so a plain TlsClientSecurity connects cleanly.
func (d *Dgraph) TlsServerSecurity(serverCert *dagger.File, serverKey *dagger.Secret) *ServerSecurity {
	return &ServerSecurity{
		Mode:       "TLS",
		ServerCert: serverCert,
		ServerKey:  serverKey,
	}
}

// MtlsServerSecurity returns a ServerSecurity profile that terminates
// mutual TLS. In addition to the server leaf (serverCert and serverKey),
// clientCa is passed as the `--tls "ca-cert=…"` value and
// `client-auth-type=REQUIREANDVERIFY`, so connecting clients must present
// a certificate signed by clientCa.
func (d *Dgraph) MtlsServerSecurity(serverCert *dagger.File, serverKey *dagger.Secret, clientCa *dagger.File) *ServerSecurity {
	return &ServerSecurity{
		Mode:       "MTLS",
		ServerCert: serverCert,
		ServerKey:  serverKey,
		ClientCa:   clientCa,
	}
}

// PlaintextClientSecurity returns a ClientSecurity profile configured
// for unencrypted, unauthenticated traffic to the cluster's Alphas.
func (d *Dgraph) PlaintextClientSecurity() *ClientSecurity {
	return &ClientSecurity{Mode: "PLAINTEXT"}
}

// TlsClientSecurity returns a ClientSecurity profile that opens a
// one-way TLS connection and verifies the Alpha against serverCa.
func (d *Dgraph) TlsClientSecurity(serverCa *dagger.File) *ClientSecurity {
	return &ClientSecurity{
		Mode:     "TLS",
		ServerCa: serverCa,
	}
}

// MtlsClientSecurity returns a ClientSecurity profile that opens a
// mutual-TLS connection: the Alpha is verified against serverCa and the
// client presents clientCert + clientKey to satisfy the listener's
// REQUIREANDVERIFY client-auth requirement.
func (d *Dgraph) MtlsClientSecurity(serverCa *dagger.File, clientCert *dagger.File, clientKey *dagger.Secret) *ClientSecurity {
	return &ClientSecurity{
		Mode:       "MTLS",
		ServerCa:   serverCa,
		ClientCert: clientCert,
		ClientKey:  clientKey,
	}
}

// validateServerSecurity rejects an incomplete profile before a
// half-configured cluster boots: TLS needs a server cert + key, MTLS
// additionally needs the client CA.
func validateServerSecurity(s *ServerSecurity) error {
	switch s.Mode {
	case "PLAINTEXT":
		return nil
	case "TLS":
		if s.ServerCert == nil || s.ServerKey == nil {
			return fmt.Errorf("TlsServerSecurity requires both serverCert and serverKey")
		}
		return nil
	case "MTLS":
		if s.ServerCert == nil || s.ServerKey == nil {
			return fmt.Errorf("MtlsServerSecurity requires serverCert and serverKey")
		}
		if s.ClientCa == nil {
			return fmt.Errorf("MtlsServerSecurity requires clientCa")
		}
		return nil
	default:
		return fmt.Errorf("unsupported server security mode %q", s.Mode)
	}
}

// securityModeLabel renders a mode constant as the spelling used in
// user-facing error messages.
func securityModeLabel(mode string) string {
	switch mode {
	case "PLAINTEXT":
		return "plaintext"
	case "TLS":
		return "TLS"
	case "MTLS":
		return "mTLS"
	default:
		return mode
	}
}
