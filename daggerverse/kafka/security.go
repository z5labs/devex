package main

import "dagger/kafka/internal/dagger"

// ServerSecurity describes how a Kafka cluster's external listener
// authenticates and encrypts traffic from clients. Internal listeners
// (inter-broker + controller-quorum) are always mTLS, regardless of mode.
type ServerSecurity struct {
	// +private
	Mode string // PLAINTEXT | TLS | MTLS
	// +private
	CaKeyStore *dagger.File // PKCS#12 CA used to mint per-broker external leaf certs (TLS + MTLS)
	// +private
	CaKeyStorePassword *dagger.Secret
	// +private
	ClientTrustStore *dagger.File // PKCS#12 of CA(s) trusted to sign incoming client certs (MTLS only)
	// +private
	ClientTrustStorePassword *dagger.Secret
}

// ClientSecurity describes how a franz-go client authenticates to a Kafka
// broker.
type ClientSecurity struct {
	// +private
	Mode string // PLAINTEXT | TLS | MTLS
	// +private
	TrustStore *dagger.File // PKCS#12 of the CA(s) the client trusts to identify the broker (TLS + MTLS)
	// +private
	TrustStorePassword *dagger.Secret
	// +private
	KeyStore *dagger.File // PKCS#12 of the client's own leaf cert + key (MTLS only)
	// +private
	KeyStorePassword *dagger.Secret
}

// PlaintextServerSecurity returns a ServerSecurity profile configured for
// unencrypted, unauthenticated traffic on the external listener. Internal
// listeners (inter-broker + controller-quorum) still use mTLS.
func (k *Kafka) PlaintextServerSecurity() *ServerSecurity {
	return &ServerSecurity{Mode: "PLAINTEXT"}
}

// TlsServerSecurity returns a ServerSecurity profile that terminates TLS on
// the external listener. caKeyStore is a PKCS#12 archive containing the
// CA cert + private key the cluster uses to mint per-broker leaf certs;
// each broker leaf carries its stable hostname (e.g. "broker-100") as a
// DNS SAN so franz-go clients dialing the bootstrap address can verify
// the broker against the same CA's truststore.
func (k *Kafka) TlsServerSecurity(
	caKeyStore *dagger.File,
	caKeyStorePassword *dagger.Secret,
) *ServerSecurity {
	return &ServerSecurity{
		Mode:               "TLS",
		CaKeyStore:         caKeyStore,
		CaKeyStorePassword: caKeyStorePassword,
	}
}

// MtlsServerSecurity returns a ServerSecurity profile that terminates mTLS
// on the external listener. caKeyStore signs per-broker server leaves;
// clientTrustStore holds the CA(s) the broker will accept incoming client
// certs from (this can be the same CA as caKeyStore or an independent one
// for asymmetric trust).
func (k *Kafka) MtlsServerSecurity(
	caKeyStore *dagger.File,
	caKeyStorePassword *dagger.Secret,
	clientTrustStore *dagger.File,
	clientTrustStorePassword *dagger.Secret,
) *ServerSecurity {
	return &ServerSecurity{
		Mode:                     "MTLS",
		CaKeyStore:               caKeyStore,
		CaKeyStorePassword:       caKeyStorePassword,
		ClientTrustStore:         clientTrustStore,
		ClientTrustStorePassword: clientTrustStorePassword,
	}
}

// PlaintextClientSecurity returns a ClientSecurity profile configured for
// unencrypted, unauthenticated traffic.
func (k *Kafka) PlaintextClientSecurity() *ClientSecurity {
	return &ClientSecurity{Mode: "PLAINTEXT"}
}

// TlsClientSecurity returns a ClientSecurity profile that opens a TLS
// connection to the broker. trustStore is a PKCS#12 archive of the CA(s)
// the client uses to verify the broker's leaf certificate (typically the
// truststore that pairs with the CA passed to TlsServerSecurity on the
// server side).
func (k *Kafka) TlsClientSecurity(
	trustStore *dagger.File,
	trustStorePassword *dagger.Secret,
) *ClientSecurity {
	return &ClientSecurity{
		Mode:               "TLS",
		TrustStore:         trustStore,
		TrustStorePassword: trustStorePassword,
	}
}

// MtlsClientSecurity returns a ClientSecurity profile that opens an mTLS
// connection: the broker presents its server cert (verified against
// trustStore) and the client presents its own leaf cert from keyStore
// (signed by a CA the broker trusts via its clientTrustStore).
func (k *Kafka) MtlsClientSecurity(
	keyStore *dagger.File,
	keyStorePassword *dagger.Secret,
	trustStore *dagger.File,
	trustStorePassword *dagger.Secret,
) *ClientSecurity {
	return &ClientSecurity{
		Mode:               "MTLS",
		TrustStore:         trustStore,
		TrustStorePassword: trustStorePassword,
		KeyStore:           keyStore,
		KeyStorePassword:   keyStorePassword,
	}
}
