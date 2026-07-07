package main

import "dagger/kafka/internal/dagger"

// SchemaRegistrySecurity describes how a Schema Registry's REST endpoint
// authenticates and encrypts traffic from clients, mirroring *ServerSecurity
// on the broker side. The same profile also parameterises the registry's
// kafka-storage connection to the backing cluster: the CA it carries doubles
// as the trust anchor the registry uses to dial the cluster's TLS/mTLS client
// listener (the single-CA convention — pass the same CA to the cluster and the
// registry).
type SchemaRegistrySecurity struct {
	// +private
	Mode string // PLAINTEXT | TLS | MTLS
	// +private
	CaKeyStore *dagger.File // PKCS#12 CA used to mint the registry's REST server leaf and derive the broker-facing truststore (TLS + MTLS)
	// +private
	CaKeyStorePassword *dagger.Secret
	// +private
	ClientTrustStore *dagger.File // PKCS#12 of CA(s) trusted to sign incoming REST client certs (MTLS only)
	// +private
	ClientTrustStorePassword *dagger.Secret
}

// SchemaRegistryClientSecurity describes how a SchemaRegistryClient's HTTP
// client authenticates to a Schema Registry's REST endpoint, mirroring
// *ClientSecurity on the broker side.
type SchemaRegistryClientSecurity struct {
	// +private
	Mode string // PLAINTEXT | TLS | MTLS
	// +private
	TrustStore *dagger.File // PKCS#12 of the CA(s) the client trusts to identify the registry (TLS + MTLS)
	// +private
	TrustStorePassword *dagger.Secret
	// +private
	KeyStore *dagger.File // PKCS#12 of the client's own leaf cert + key (MTLS only)
	// +private
	KeyStorePassword *dagger.Secret
}

// PlaintextSchemaRegistrySecurity returns a SchemaRegistrySecurity profile
// configured for unencrypted, unauthenticated traffic on the registry REST
// endpoint. It pairs with a PLAINTEXT cluster.
func (k *Kafka) PlaintextSchemaRegistrySecurity() *SchemaRegistrySecurity {
	return &SchemaRegistrySecurity{Mode: "PLAINTEXT"}
}

// TlsSchemaRegistrySecurity returns a SchemaRegistrySecurity profile that
// terminates TLS on the registry REST endpoint. caKeyStore is a PKCS#12
// archive containing the CA cert + private key the registry uses to mint its
// per-registry server leaf (bound to the registry's service hostname as a DNS
// SAN) and to derive the truststore its kafka-storage connection uses to
// verify the backing broker. Pair with a TLS cluster minted from the same CA.
func (k *Kafka) TlsSchemaRegistrySecurity(
	caKeyStore *dagger.File,
	caKeyStorePassword *dagger.Secret,
) *SchemaRegistrySecurity {
	return &SchemaRegistrySecurity{
		Mode:               "TLS",
		CaKeyStore:         caKeyStore,
		CaKeyStorePassword: caKeyStorePassword,
	}
}

// MtlsSchemaRegistrySecurity returns a SchemaRegistrySecurity profile that
// terminates mTLS on the registry REST endpoint. caKeyStore signs the
// registry's server leaf (and the registry's own client leaf presented to the
// broker over mTLS); clientTrustStore holds the CA(s) the registry accepts
// incoming REST client certs from. Pair with an MTLS cluster minted from the
// same CA.
func (k *Kafka) MtlsSchemaRegistrySecurity(
	caKeyStore *dagger.File,
	caKeyStorePassword *dagger.Secret,
	clientTrustStore *dagger.File,
	clientTrustStorePassword *dagger.Secret,
) *SchemaRegistrySecurity {
	return &SchemaRegistrySecurity{
		Mode:                     "MTLS",
		CaKeyStore:               caKeyStore,
		CaKeyStorePassword:       caKeyStorePassword,
		ClientTrustStore:         clientTrustStore,
		ClientTrustStorePassword: clientTrustStorePassword,
	}
}

// PlaintextSchemaRegistryClientSecurity returns a SchemaRegistryClientSecurity
// profile configured for unencrypted HTTP traffic.
func (k *Kafka) PlaintextSchemaRegistryClientSecurity() *SchemaRegistryClientSecurity {
	return &SchemaRegistryClientSecurity{Mode: "PLAINTEXT"}
}

// TlsSchemaRegistryClientSecurity returns a SchemaRegistryClientSecurity
// profile that opens an HTTPS connection to the registry. trustStore is a
// PKCS#12 archive of the CA(s) the client uses to verify the registry's REST
// leaf certificate (typically the truststore that pairs with the CA passed to
// TlsSchemaRegistrySecurity on the server side).
func (k *Kafka) TlsSchemaRegistryClientSecurity(
	trustStore *dagger.File,
	trustStorePassword *dagger.Secret,
) *SchemaRegistryClientSecurity {
	return &SchemaRegistryClientSecurity{
		Mode:               "TLS",
		TrustStore:         trustStore,
		TrustStorePassword: trustStorePassword,
	}
}

// MtlsSchemaRegistryClientSecurity returns a SchemaRegistryClientSecurity
// profile that opens an mTLS HTTPS connection: the registry presents its REST
// server cert (verified against trustStore) and the client presents its own
// leaf cert from keyStore (signed by a CA the registry trusts via its
// clientTrustStore).
func (k *Kafka) MtlsSchemaRegistryClientSecurity(
	keyStore *dagger.File,
	keyStorePassword *dagger.Secret,
	trustStore *dagger.File,
	trustStorePassword *dagger.Secret,
) *SchemaRegistryClientSecurity {
	return &SchemaRegistryClientSecurity{
		Mode:               "MTLS",
		TrustStore:         trustStore,
		TrustStorePassword: trustStorePassword,
		KeyStore:           keyStore,
		KeyStorePassword:   keyStorePassword,
	}
}
