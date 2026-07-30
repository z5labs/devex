package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"dagger/bruno/internal/dagger"
)

const (
	// caCertPath is where WithCaCert's certificate is mounted. The module
	// mounts it rather than taking a path from the caller because a `--cacert`
	// pointing at a file that is not there is not one of bru's usage errors:
	// it prints "Cacert File ... does not exist" and carries on with the
	// default truststore, so the run fails verification for a reason that
	// looks nothing like the mistake.
	caCertPath = "/tmp/bruno-cacert.pem"

	// clientCertConfigPath is where the rendered `--client-cert-config`
	// document is mounted, and clientCertPathPrefix/clientKeyPathPrefix are
	// the stems the certificate and key of each entry are mounted under. All
	// three live outside the collection: the document is this module's, not
	// something the caller wrote, and writing it beside the caller's requests
	// would put it in the tree a collection is linted and committed from.
	clientCertConfigPath = "/tmp/bruno-client-cert-config.json"
	clientCertPathPrefix = "/tmp/bruno-client-cert-"
	clientKeyPathPrefix  = "/tmp/bruno-client-key-"

	// clientCertConfigSecretPrefix names the rendered document's secret. The
	// digest of the document is appended, because dag.SetSecret keys on the
	// name: two collections configured differently in one session would
	// otherwise both see whichever one was rendered first.
	clientCertConfigSecretPrefix = "bruno-client-cert-config-"

	// clientCertKindPem selects bru's PEM entry shape — certFilePath plus
	// keyFilePath — over its "pfx" one. A PKCS#12 archive is a single file and
	// so would not need the key to travel as a secret; it is not wrapped.
	clientCertKindPem = "cert"

	// imageUser is the non-root user the image runs as, and every mount here is
	// handed to it. A secret mount is root-owned 0400 by default, and a
	// certificate file arrives carrying whatever mode it was written with —
	// certificate-management writes 0600 — so an un-owned mount is a file bru
	// cannot open. That surfaces as "Error reading cert/key file", or for the CA
	// as a peer that simply fails to verify: nowhere near the permission bit
	// that caused it.
	imageUser = "node"
)

// clientCertDocument is the JSON `--client-cert-config` takes: bru merges its
// certs into the collection's own clientCertificates and then picks the first
// entry whose domain matches the request URL.
//
// It is rendered here rather than asked for from the caller because the paths
// in it are paths inside a container the caller never sees. A hand-written
// config would have to name the mount points this module chose.
type clientCertDocument struct {
	// Enabled is required: bru ignores a document that does not carry it,
	// warning rather than failing.
	Enabled bool              `json:"enabled"`
	Certs   []clientCertEntry `json:"certs"`
}

// clientCertEntry is one host-to-certificate binding. Kind carries the "type"
// key bru reads; the Go field is named around it because an exported field
// literally called Type breaks Dagger's dependency codegen.
type clientCertEntry struct {
	Domain       string `json:"domain"`
	Kind         string `json:"type"`
	CertFilePath string `json:"certFilePath"`
	KeyFilePath  string `json:"keyFilePath"`
	Passphrase   string `json:"passphrase,omitempty"`
}

// WithCaCert verifies peers against a custom CA certificate (`--cacert`), for
// the collection whose target presents a certificate signed by a private CA —
// an internal endpoint, or a service stood up for the length of the pipeline.
//
// bru adds the certificate to the default truststore rather than replacing it,
// so a collection that also reaches a public endpoint keeps working. Use
// WithoutTruststore for the private CA exclusively.
//
// This is the control WithInsecure is not: the run still verifies, it just
// verifies against the CA the caller named.
func (c *Collection) WithCaCert(cert *dagger.File) *Collection {
	out := c.clone()
	out.CaCert = cert
	return out
}

// WithoutTruststore verifies peers against the WithCaCert certificate alone
// (`--ignore-truststore`), ignoring the CAs the image ships.
//
// It only means anything alongside WithCaCert — bru evaluates the flag in
// combination with `--cacert` only — so on its own it is rejected by the run
// rather than silently doing nothing.
func (c *Collection) WithoutTruststore() *Collection {
	out := c.clone()
	out.IgnoreTruststore = true
	return out
}

// WithClientCert presents a client certificate to hosts matching host
// (`--client-cert-config`), for the collection that has to authenticate to an
// mTLS endpoint. Call it more than once for more than one host.
//
// The key is a *dagger.Secret and not a *dagger.File: it is key material, and a
// file's contents are content-addressed into the build cache and readable from
// a trace. It is mounted as a secret, outside the collection, and named only
// from the rendered config — so it reaches neither argv nor the collection tree.
//
// bru takes this as a JSON document referring to the certificate and key by
// path. That document is rendered at run time against the paths this module
// mounts them under, because a caller writing it by hand would have to name
// paths inside a container they cannot see.
func (c *Collection) WithClientCert(
	// Hostname pattern the certificate applies to, matched against the request
	// URL: "api.internal" for one host, "*.internal" for a wildcard. bru uses
	// the first configured host that matches.
	host string,
	// PEM certificate to present.
	cert *dagger.File,
	// PEM private key for the certificate.
	key *dagger.Secret,
	// Passphrase the private key is encrypted with, if it is.
	// +optional
	passphrase *dagger.Secret,
) *Collection {
	out := c.clone()
	out.ClientCertHosts = append(out.ClientCertHosts, host)
	out.ClientCerts = append(out.ClientCerts, cert)
	out.ClientKeys = append(out.ClientKeys, key)
	out.ClientPassphrases = append(out.ClientPassphrases, passphrase)
	return out
}

// tlsArgs renders the TLS flags. The paths are this module's mount points, not
// anything the caller passed, which is why none of them can be missing by the
// time bru reads them.
func (c *Collection) tlsArgs() []string {
	var args []string
	if c.CaCert != nil {
		args = append(args, "--cacert", caCertPath)
	}
	if c.IgnoreTruststore {
		args = append(args, "--ignore-truststore")
	}
	if len(c.ClientCertHosts) > 0 {
		args = append(args, "--client-cert-config", clientCertConfigPath)
	}
	return args
}

// withTLS stages what the TLS flags point at: the CA certificate as a file, and
// each client certificate beside its key and the document that binds them to a
// host.
//
// The key and the document travel as secret mounts. The key is key material;
// the document is too, because it carries the passphrase in plaintext — that is
// the shape bru reads it in, so keeping it out of a cacheable layer is the only
// place that can be dealt with.
func (c *Collection) withTLS(ctx context.Context, ctr *dagger.Container) (*dagger.Container, error) {
	if c.CaCert != nil {
		ctr = ctr.WithMountedFile(caCertPath, c.CaCert, fileMount())
	}
	if len(c.ClientCertHosts) == 0 {
		return ctr, nil
	}
	config, err := c.clientCertConfig(ctx)
	if err != nil {
		return nil, err
	}
	ctr = ctr.WithMountedSecret(clientCertConfigPath, config, secretMount())
	for i := range c.ClientCertHosts {
		ctr = ctr.
			WithMountedFile(clientCertPath(i), c.ClientCerts[i], fileMount()).
			WithMountedSecret(clientKeyPath(i), c.ClientKeys[i], secretMount())
	}
	return ctr, nil
}

// clientCertConfig renders the `--client-cert-config` document.
//
// It comes back as a secret rather than a file because a passphrase is a
// plaintext field of it. A *dagger.File built here would be content-addressed
// into the build cache and its contents readable from the trace, which would
// undo the reason WithClientCert takes the passphrase as a secret at all.
func (c *Collection) clientCertConfig(ctx context.Context) (*dagger.Secret, error) {
	doc := clientCertDocument{Enabled: true}
	for i, host := range c.ClientCertHosts {
		entry := clientCertEntry{
			Domain:       host,
			Kind:         clientCertKindPem,
			CertFilePath: clientCertPath(i),
			KeyFilePath:  clientKeyPath(i),
		}
		if passphrase := c.ClientPassphrases[i]; passphrase != nil {
			plaintext, err := passphrase.Plaintext(ctx)
			if err != nil {
				return nil, fmt.Errorf("WithClientCert: read the passphrase for %q: %w", host, err)
			}
			entry.Passphrase = plaintext
		}
		doc.Certs = append(doc.Certs, entry)
	}
	rendered, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("WithClientCert: render the client certificate config: %w", err)
	}
	digest := sha256.Sum256(rendered)
	name := clientCertConfigSecretPrefix + hex.EncodeToString(digest[:8])
	return dag.SetSecret(name, string(rendered)), nil
}

// validateTLS reports the TLS half of the deferred builder validation.
func (c *Collection) validateTLS() error {
	if c.Insecure && c.CaCert != nil {
		return fmt.Errorf(
			"WithCaCert: cannot be combined with WithInsecure: bru drops --cacert when --insecure is set, so the run would verify nothing rather than verify against the supplied CA")
	}
	if c.IgnoreTruststore && c.CaCert == nil {
		return fmt.Errorf(
			"WithoutTruststore: requires WithCaCert: --ignore-truststore is evaluated in combination with --cacert only, and alone it would leave the run nothing to verify against")
	}
	seen := make(map[string]bool, len(c.ClientCertHosts))
	for _, host := range c.ClientCertHosts {
		if strings.TrimSpace(host) == "" {
			return fmt.Errorf("WithClientCert: host pattern is required")
		}
		if seen[host] {
			// bru stops at the first entry whose domain matches, so the second
			// certificate for a host is never presented — and a caller who
			// configured two of them meant something by the other one.
			return fmt.Errorf(
				"WithClientCert: host %q is configured twice: bru presents the first entry that matches a request URL, so the second would never be used", host)
		}
		seen[host] = true
	}
	return nil
}

// clientCertPath and clientKeyPath are where the nth entry's certificate and
// key are mounted. One path per entry, so a collection presenting a different
// certificate to each of two hosts does not have them overwrite each other.
func clientCertPath(i int) string {
	return clientCertPathPrefix + strconv.Itoa(i) + ".pem"
}

func clientKeyPath(i int) string {
	return clientKeyPathPrefix + strconv.Itoa(i) + ".pem"
}

// secretMount makes a secret mount readable by the user the image runs as.
// Dagger's default is root-owned 0400, and Mode only takes effect once an
// owner is set.
func secretMount() dagger.ContainerWithMountedSecretOpts {
	return dagger.ContainerWithMountedSecretOpts{Owner: imageUser, Mode: 0o400}
}

// fileMount does the same for a certificate file, which arrives with whatever
// mode it was written with — 0600 from certificate-management — and would
// otherwise be a root-owned file the run cannot open.
func fileMount() dagger.ContainerWithMountedFileOpts {
	return dagger.ContainerWithMountedFileOpts{Owner: imageUser}
}
