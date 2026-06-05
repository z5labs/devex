package main

import (
	"fmt"

	"dagger/postgres/internal/dagger"
)

// Fixed in-container paths for the caller-supplied TLS material and the
// rendered host-based-auth file. The directory is owned by the postgres
// user so the server can read its key without tripping PostgreSQL's
// "private key file has group or world access" startup check.
const (
	serverCertPath = "/etc/postgres-tls/server.crt"
	serverKeyPath  = "/etc/postgres-tls/server.key"
	clientCaPath   = "/etc/postgres-tls/client-ca.crt"
	hbaPath        = "/etc/postgres-tls/pg_hba.conf"
)

// validateServerSecurity rejects an incomplete profile before a
// half-configured primary boots: TLS needs a server cert + key, MTLS
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

// applyServerSecurity mounts the listener's TLS material onto the
// container and returns the postgres startup args for the mode. For
// PLAINTEXT it is a no-op (nil args ⇒ the image's default `postgres`
// CMD, scram-sha-256 over a plaintext TCP listener). For TLS / MTLS it
// mounts the PEM cert (world-readable), the PEM key (a secret, 0600 and
// owned by postgres), the optional client CA, and a custom pg_hba.conf,
// then returns `postgres -c ssl=on …` args pointing at them.
func applyServerSecurity(ctr *dagger.Container, s *ServerSecurity) (*dagger.Container, []string) {
	if s.Mode == "PLAINTEXT" {
		return ctr, nil
	}

	ctr = ctr.
		WithFile(serverCertPath, s.ServerCert, dagger.ContainerWithFileOpts{
			Permissions: 0o644,
			Owner:       "postgres:postgres",
		}).
		WithMountedSecret(serverKeyPath, s.ServerKey, dagger.ContainerWithMountedSecretOpts{
			Mode:  0o600,
			Owner: "postgres:postgres",
		}).
		WithNewFile(hbaPath, pgHbaConf(s.Mode), dagger.ContainerWithNewFileOpts{
			Permissions: 0o644,
			Owner:       "postgres:postgres",
		})

	args := []string{
		"postgres",
		"-c", "ssl=on",
		"-c", "ssl_cert_file=" + serverCertPath,
		"-c", "ssl_key_file=" + serverKeyPath,
		"-c", "hba_file=" + hbaPath,
	}

	if s.Mode == "MTLS" {
		ctr = ctr.WithFile(clientCaPath, s.ClientCa, dagger.ContainerWithFileOpts{
			Permissions: 0o644,
			Owner:       "postgres:postgres",
		})
		args = append(args, "-c", "ssl_ca_file="+clientCaPath)
	}

	return ctr, args
}

// pgHbaConf renders the host-based-auth file for a TLS / MTLS listener.
// The `local … trust` line is unix-socket-only and exists solely so the
// image entrypoint's bootstrap server (which connects over the socket
// during initdb) can run its setup. There is no plaintext `host` line,
// so plaintext TCP is refused — only `hostssl` connections are accepted.
// MTLS appends `clientcert=verify-full`, requiring a client certificate
// signed by ssl_ca_file in addition to the password.
func pgHbaConf(mode string) string {
	suffix := ""
	if mode == "MTLS" {
		suffix = " clientcert=verify-full"
	}
	return fmt.Sprintf(`# Managed by the devex postgres module (%s listener).
local   all   all                 trust
hostssl all   all   0.0.0.0/0     scram-sha-256%s
hostssl all   all   ::/0          scram-sha-256%s
`, mode, suffix, suffix)
}
