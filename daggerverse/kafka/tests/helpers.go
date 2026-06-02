package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"dagger/tests/internal/dagger"
)

// consumedRecord mirrors the kafka module's ConsumedRecord. Consume returns a
// JSON array string (Dagger v0.21 cannot lazily re-resolve fields of objects
// returned from a +cache="never" cross-module call), so tests unmarshal into
// this local type. The getter methods preserve the previous object-style call
// sites (records[i].Key(ctx)); the returned error is always nil.
type consumedRecord struct {
	KeyV           string `json:"key"`
	ValueV         string `json:"value"`
	KeySchemaIDV   int    `json:"keySchemaId"`
	ValueSchemaIDV int    `json:"valueSchemaId"`
}

func (r consumedRecord) Key(context.Context) (string, error)   { return r.KeyV, nil }
func (r consumedRecord) Value(context.Context) (string, error) { return r.ValueV, nil }
func (r consumedRecord) KeySchemaID(context.Context) (int, error) {
	return r.KeySchemaIDV, nil
}
func (r consumedRecord) ValueSchemaID(context.Context) (int, error) {
	return r.ValueSchemaIDV, nil
}

// consume calls the kafka module's Consume and unmarshals the JSON array it
// returns into a slice of consumedRecord.
func consume(ctx context.Context, client *dagger.KafkaClient, topic string, opts dagger.KafkaClientConsumeOpts) ([]consumedRecord, error) {
	raw, err := client.Consume(ctx, topic, opts)
	if err != nil {
		return nil, err
	}
	var records []consumedRecord
	if err := json.Unmarshal([]byte(raw), &records); err != nil {
		return nil, fmt.Errorf("unmarshal consumed records: %w", err)
	}
	return records, nil
}

// newClusterId mints a fresh KRaft-shaped cluster ID — 16 random bytes
// rendered as 22 unpadded base64-url characters — by feeding random bytes
// from the random module through the standard library.
func newClusterId(ctx context.Context) (string, error) {
	h, err := dag.Random().Sha256(ctx)
	if err != nil {
		return "", fmt.Errorf("random sha256: %w", err)
	}
	if len(h) < 32 {
		return "", fmt.Errorf("random sha256 too short: %d", len(h))
	}
	raw, err := hex.DecodeString(h[:32])
	if err != nil {
		return "", fmt.Errorf("decode random sha256: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// freshCa mints a fresh per-test root CA via the certificate-management
// module. Each call uses fresh inputs (random key, random password,
// random serial, time.Now() notBefore) so the resulting CA is unique per
// test. Returns the CA itself (lazy chain) for further leaf signing.
func freshCa(ctx context.Context, label string) (*dagger.CertificateManagementCertificateAuthority, error) {
	keyPem, err := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).Pem().Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate %s ca key: %w", label, err)
	}
	suffix, err := randHex(ctx)
	if err != nil {
		return nil, err
	}
	key := dag.SetSecret(label+"-ca-key-"+suffix, keyPem)

	pwdHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate %s ca password: %w", label, err)
	}
	pwd := dag.SetSecret(label+"-ca-pwd-"+suffix, pwdHex)

	serial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate %s ca serial: %w", label, err)
	}
	nb := time.Now().UTC().Format(time.RFC3339)
	return dag.CertificateManagement().CreateCertificateAuthority(nb, serial, pwd, key,
		dagger.CertificateManagementCreateCertificateAuthorityOpts{
			CommonName:   "Test CA " + label,
			ValidityDays: 30,
		}), nil
}

// randHex returns a fresh hex suffix, used to disambiguate Dagger secret
// names across concurrent test invocations within the same engine session.
func randHex(ctx context.Context) (string, error) {
	h, err := dag.Random().Sha256(ctx)
	if err != nil {
		return "", fmt.Errorf("randHex: %w", err)
	}
	if len(h) < 16 {
		return "", fmt.Errorf("randHex too short: %d", len(h))
	}
	return h[:16], nil
}

// randomTopicName mints a fresh, lower-case-alpha-prefixed topic name so
// tests don't collide and so the name is a valid Kafka topic identifier.
func randomTopicName(ctx context.Context) (string, error) {
	h, err := dag.Random().Sha256(ctx)
	if err != nil {
		return "", fmt.Errorf("random sha256: %w", err)
	}
	if len(h) < 16 {
		return "", fmt.Errorf("random sha256 too short: %d", len(h))
	}
	return "t-" + h[:16], nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
