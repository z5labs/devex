package main

import (
	"fmt"
	"strconv"

	"dagger/valkey/internal/dagger"
)

// ServerSecurity describes how a Valkey server's client-facing listener
// authenticates and encrypts traffic. Three modes are supported:
//
//   - PLAINTEXT — `requirepass` auth over an unencrypted TCP listener.
//   - TLS — one-way TLS: the node presents a server certificate on a
//     `--tls-port` listener and clients still authenticate with the
//     password.
//   - MTLS — mutual TLS: connecting clients must additionally present a
//     certificate signed by ClientCa.
//
// The cert material is caller-supplied PEM: valkey-server reads it
// natively via `--tls-cert-file` / `--tls-key-file` / `--tls-ca-cert-file`.
//
// The trap this module has to get right: Valkey's `tls-auth-clients`
// defaults to `yes`, so a TLS-enabled node demands client certificates
// unless `--tls-auth-clients no` is passed. One-way TLS is therefore the
// opt-*out* here, inverting the postgres posture where mTLS is the
// opt-in — see applyServerSecurity.
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
// the server CA and sets ServerName to the dialed host; MTLS
// additionally presents a client certificate + key. The client builds a
// *tls.Config from this PEM material and hands it to valkey-go via
// ClientOption.TLSConfig.
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

// TlsServerSecurity returns a ServerSecurity profile that terminates
// one-way TLS on the node's :6379 listener. serverCert is the PEM leaf
// certificate (its SAN must cover the hostname the client dials) and
// serverKey is the matching PEM PKCS#8 private key. The node starts with
// `--tls-port 6379 --port 0` (encrypted listener only, plaintext off)
// and `--tls-auth-clients no`, so a client authenticates with the
// password and the server certificate but presents no client cert.
func (v *Valkey) TlsServerSecurity(serverCert *dagger.File, serverKey *dagger.Secret) *ServerSecurity {
	return &ServerSecurity{
		Mode:       "TLS",
		ServerCert: serverCert,
		ServerKey:  serverKey,
	}
}

// MtlsServerSecurity returns a ServerSecurity profile that terminates
// mutual TLS. In addition to the server leaf (serverCert and serverKey),
// clientCa is mounted as `--tls-ca-cert-file` and `tls-auth-clients` is
// left at its default (`yes`), so connecting clients must present a cert
// signed by clientCa AND the correct password.
func (v *Valkey) MtlsServerSecurity(serverCert *dagger.File, serverKey *dagger.Secret, clientCa *dagger.File) *ServerSecurity {
	return &ServerSecurity{
		Mode:       "MTLS",
		ServerCert: serverCert,
		ServerKey:  serverKey,
		ClientCa:   clientCa,
	}
}

// PlaintextClientSecurity returns a ClientSecurity profile configured
// for `requirepass` auth over an unencrypted TCP connection.
func (v *Valkey) PlaintextClientSecurity() *ClientSecurity {
	return &ClientSecurity{Mode: "PLAINTEXT"}
}

// TlsClientSecurity returns a ClientSecurity profile that opens a
// one-way TLS connection and verifies the server against serverCa, with
// ServerName pinned to the dialed host.
func (v *Valkey) TlsClientSecurity(serverCa *dagger.File) *ClientSecurity {
	return &ClientSecurity{
		Mode:     "TLS",
		ServerCa: serverCa,
	}
}

// MtlsClientSecurity returns a ClientSecurity profile that opens a
// mutual-TLS connection: the server is verified against serverCa and the
// client presents clientCert + clientKey to satisfy the node's
// `tls-auth-clients yes` requirement.
func (v *Valkey) MtlsClientSecurity(serverCa *dagger.File, clientCert *dagger.File, clientKey *dagger.Secret) *ClientSecurity {
	return &ClientSecurity{
		Mode:       "MTLS",
		ServerCa:   serverCa,
		ClientCert: clientCert,
		ClientKey:  clientKey,
	}
}

// Fixed in-container paths for the caller-supplied TLS material. The key
// rides in as a mounted secret so its plaintext never lands in the image
// layers or the Dagger graph; the certs are ordinary world-readable
// files. All are owned by the valkey user the entrypoint drops to, so
// valkey-server can read them after `gosu valkey`.
const (
	serverCertPath = "/etc/valkey-tls/server.crt"
	serverKeyPath  = "/etc/valkey-tls/server.key"
	clientCaPath   = "/etc/valkey-tls/client-ca.crt"
)

// validateServerSecurity rejects an incomplete profile before a
// half-configured node boots: TLS needs a server cert + key, MTLS
// additionally needs the client CA. A caller who hand-builds a profile
// (rather than using the constructors) gets a clear error instead of a
// node that silently boots wrong.
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

// applyServerSecurity renders the container mutations and the
// `valkey-server` arguments that realise a listener mode, returning the
// (possibly mutated) container alongside the args.
//
// PLAINTEXT needs no mounts: the whole configuration is two flags.
//
// TLS / MTLS mount the caller-supplied PEM material and terminate TLS on
// a SECOND listener: `--tls-port 6379 --port 0`. Valkey does not upgrade
// the existing listener in place — it opens the encrypted one on
// `tls-port` and leaves the plaintext `port` alive alongside it unless
// that port is explicitly set to 0. `--port 0` is what makes the
// plaintext listener genuinely off.
//
// The tls-auth-clients trap: it defaults to `yes`, so a TLS node demands
// a client certificate unless told otherwise. TLS (one-way) is the
// opt-*out* and therefore passes `--tls-auth-clients no`; MTLS adds
// `--tls-ca-cert-file` and leaves the flag at its default so a client
// cert is required. Forgetting the flag in TLS mode would silently
// produce mTLS that rejects every well-formed one-way client.
//
// `"$VALKEY_PASSWORD"` is expanded by the shell wrapper Server builds
// around these args, from the secret environment variable — the
// plaintext never enters the container's argv in the Dagger graph.
func applyServerSecurity(ctr *dagger.Container, s *ServerSecurity) (*dagger.Container, []string) {
	if s.Mode == "PLAINTEXT" {
		return ctr, []string{
			"--port", strconv.Itoa(valkeyPort),
			"--requirepass", `"$VALKEY_PASSWORD"`,
		}
	}

	ctr = ctr.
		WithFile(serverCertPath, s.ServerCert, dagger.ContainerWithFileOpts{
			Permissions: 0o644,
			Owner:       "valkey:valkey",
		}).
		WithMountedSecret(serverKeyPath, s.ServerKey, dagger.ContainerWithMountedSecretOpts{
			Mode:  0o600,
			Owner: "valkey:valkey",
		})

	args := []string{
		"--tls-port", strconv.Itoa(valkeyPort),
		"--port", "0",
		"--tls-cert-file", serverCertPath,
		"--tls-key-file", serverKeyPath,
		"--requirepass", `"$VALKEY_PASSWORD"`,
	}

	switch s.Mode {
	case "TLS":
		args = append(args, "--tls-auth-clients", "no")
	case "MTLS":
		ctr = ctr.WithFile(clientCaPath, s.ClientCa, dagger.ContainerWithFileOpts{
			Permissions: 0o644,
			Owner:       "valkey:valkey",
		})
		args = append(args, "--tls-ca-cert-file", clientCaPath)
	}

	return ctr, args
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
