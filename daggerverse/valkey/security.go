package main

import (
	"fmt"
	"strconv"

	"dagger/valkey/internal/dagger"
)

// ServerSecurity describes how a Valkey server's client-facing listener
// authenticates and encrypts traffic. Three modes are planned:
//
//   - PLAINTEXT — `requirepass` auth over an unencrypted TCP listener.
//   - TLS — one-way TLS: the node presents a server certificate on a
//     `--tls-port` listener and clients still authenticate with the
//     password.
//   - MTLS — mutual TLS: connecting clients must additionally present a
//     certificate signed by ClientCa.
//
// Only PLAINTEXT is constructible in this story. The TLS / MTLS fields
// are carried now so the follow-up that adds them slots in without
// changing the Server signature.
//
// Note for that follow-up: Valkey's `tls-auth-clients` defaults to
// `yes`, so a TLS-enabled node demands client certificates unless
// `--tls-auth-clients no` is passed. One-way TLS is therefore the
// opt-*out* here, inverting the postgres posture where mTLS is the
// opt-in.
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

// ClientSecurity describes how a valkey-go client connects to a Valkey
// server. PLAINTEXT connects over an unencrypted TCP listener; TLS pins
// the server CA; MTLS additionally presents a client certificate + key.
//
// Only PLAINTEXT is constructible in this story.
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
// for `requirepass` auth over an unencrypted TCP listener.
func (v *Valkey) PlaintextServerSecurity() *ServerSecurity {
	return &ServerSecurity{Mode: "PLAINTEXT"}
}

// PlaintextClientSecurity returns a ClientSecurity profile configured
// for `requirepass` auth over an unencrypted TCP connection.
func (v *Valkey) PlaintextClientSecurity() *ClientSecurity {
	return &ClientSecurity{Mode: "PLAINTEXT"}
}

// validateServerSecurity rejects the listener modes this story cannot
// yet configure, so a caller who hand-builds a TLS profile gets a clear
// error instead of a node that silently boots plaintext.
func validateServerSecurity(s *ServerSecurity) error {
	switch s.Mode {
	case "PLAINTEXT":
		return nil
	default:
		return fmt.Errorf(
			"clientListenerSecurity mode %s is not supported yet; pass PlaintextServerSecurity()",
			securityModeLabel(s.Mode),
		)
	}
}

// applyServerSecurity renders the container mutations and the
// `valkey-server` arguments that realise a listener mode. PLAINTEXT
// needs no mounts: the whole configuration is two flags. The TLS /
// MTLS follow-up mounts the caller-supplied PEM material here and swaps
// in a `--tls-port 6379 --port 0` pair, which is why this returns the
// container alongside the args.
//
// `"$VALKEY_PASSWORD"` is expanded by the shell wrapper Server builds
// around these args, from the secret environment variable — the
// plaintext never enters the container's argv in the Dagger graph.
func applyServerSecurity(ctr *dagger.Container, s *ServerSecurity) (*dagger.Container, []string) {
	// PLAINTEXT is the only mode validateServerSecurity lets through, so
	// there is exactly one rendering today and `s` carries no material
	// yet; the TLS / MTLS branch reads it here.
	return ctr, []string{
		"--port", strconv.Itoa(valkeyPort),
		"--requirepass", `"$VALKEY_PASSWORD"`,
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
