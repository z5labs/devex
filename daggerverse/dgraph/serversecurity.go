package main

import (
	"strings"

	"dagger/dgraph/internal/dagger"
)

// Fixed in-container paths for the caller-supplied TLS material. Both the
// Zero and every Alpha mount the material at these paths and reference
// them through the shared `--tls` superflag, so a single server leaf
// covers every listener in the cluster.
const (
	serverCertPath = "/etc/dgraph-tls/server.crt"
	serverKeyPath  = "/etc/dgraph-tls/server.key"
	clientCaPath   = "/etc/dgraph-tls/client-ca.crt"
)

// applyServerSecurity mounts the listener's TLS material onto a Zero or
// Alpha container and returns the extra `--tls` superflag args for the
// mode. PLAINTEXT is a no-op (nil args ⇒ unencrypted listeners). TLS
// mounts the PEM server cert (world-readable) and the PEM key (a secret),
// then enables one-way TLS with the default VERIFYIFGIVEN client-auth so
// clients need not present a certificate. MTLS additionally mounts the
// client CA and switches client-auth to REQUIREANDVERIFY, so connecting
// clients must present a cert signed by that CA.
func applyServerSecurity(ctr *dagger.Container, s *ServerSecurity) (*dagger.Container, []string) {
	if s.Mode == "PLAINTEXT" {
		return ctr, nil
	}

	ctr = ctr.
		WithFile(serverCertPath, s.ServerCert, dagger.ContainerWithFileOpts{
			Permissions: 0o644,
		}).
		WithMountedSecret(serverKeyPath, s.ServerKey, dagger.ContainerWithMountedSecretOpts{
			Mode: 0o400,
		})

	fields := []string{
		"server-cert=" + serverCertPath,
		"server-key=" + serverKeyPath,
	}

	if s.Mode == "MTLS" {
		ctr = ctr.WithFile(clientCaPath, s.ClientCa, dagger.ContainerWithFileOpts{
			Permissions: 0o644,
		})
		fields = append(fields,
			"ca-cert="+clientCaPath,
			"client-auth-type=REQUIREANDVERIFY",
		)
	}

	// Dgraph's `--tls` superflag is a single `key=value;`-delimited
	// argument shared by every listener (Alpha client-facing HTTP/gRPC,
	// Zero admin HTTP). `internal-port=false` (the default) keeps
	// inter-node traffic plaintext, so Alpha↔Zero need no peer certs.
	return ctr, []string{"--tls", strings.Join(fields, "; ") + ";"}
}
