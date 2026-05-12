package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"dagger/kafka/internal/dagger"
)

// writeWorkdirBytes writes content into a content-addressed subdir of the
// kafka module runtime's scratch workdir and returns it as a *dagger.File.
// Distinct callers writing distinct content land at distinct paths;
// identical content collapses to one file (idempotent across re-entry).
func writeWorkdirBytes(label, name string, content []byte) (*dagger.File, error) {
	sum := sha256.Sum256(content)
	dir := "kafka-" + label + "-" + hex.EncodeToString(sum[:8])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return nil, fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("write %s: %w", tmpPath, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("rename to %s: %w", path, err)
	}
	return dag.CurrentModule().WorkdirFile(path), nil
}

// clusterHostSuffix derives a short DNS-safe suffix from a clusterId so
// per-cluster service hostnames don't collide across parallel runs in the
// same engine session. SHA-256 hex first 10 chars: deterministic per
// clusterId (so cache keys are stable) and trivially DNS-LDH compliant
// (lowercase hex, never empty, no leading/trailing dashes).
func clusterHostSuffix(clusterId string) string {
	sum := sha256.Sum256([]byte(clusterId))
	return hex.EncodeToString(sum[:5]) // 10 hex chars = 40 bits
}

// randSuffix returns a fresh hex suffix for naming Dagger secrets uniquely
// across concurrent helper calls.
func randSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is fatal in module runtime context.
		panic(fmt.Sprintf("randSuffix: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// dagFileBytes materializes a *dagger.File via Export then ReadFile. Used
// for binary content (PKCS#12 archives) where File.Contents() would corrupt
// non-UTF-8 bytes when round-tripped through the GraphQL String type.
func dagFileBytes(ctx context.Context, f *dagger.File) ([]byte, error) {
	local := "kafka-tls-in-" + randSuffix()
	if _, err := f.Export(ctx, local); err != nil {
		return nil, fmt.Errorf("export file: %w", err)
	}
	defer os.Remove(local)
	return os.ReadFile(local)
}
