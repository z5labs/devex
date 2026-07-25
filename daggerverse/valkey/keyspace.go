package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/valkey-io/valkey-go"

	"dagger/valkey/internal/dagger"
)

// keyspaceExportVersion is stamped into every file Export writes and
// checked by ImportFile. It exists so a file written by a future module
// whose schema moved is refused with an explanation rather than
// half-decoded into a keyspace.
const keyspaceExportVersion = 1

// keyspaceExportFile is the name the export lands under inside its
// content-addressed workdir subdir. It is what a caller sees in
// `dagger call export -o .`.
const keyspaceExportFile = "keyspace.json"

// keyspaceBatch is how many keys Export captures — and ImportFile
// restores — per pipelined round trip. Batching keeps a large keyspace
// from turning into one round trip per key without building a single
// command list proportional to the whole database.
const keyspaceBatch = 256

// keyspaceExport is the on-disk form of an export. It is JSON rather
// than a concatenation of raw DUMP payloads so the file stays
// inspectable (`jq '.keys[].key'` lists the keyspace) and so a payload,
// which is arbitrary binary, has an unambiguous framing.
type keyspaceExport struct {
	Version int           `json:"version"`
	Pattern string        `json:"pattern"`
	Keys    []exportedKey `json:"keys"`
}

// exportedKey is one key's captured state: its name, what was left of
// its TTL at capture time, and its DUMP payload.
type exportedKey struct {
	Key string `json:"key"`
	// TtlMs is the remaining TTL in milliseconds at capture time; 0
	// means the key was persistent. It is deliberately relative rather
	// than an absolute deadline: an import into a fresh server is
	// usually replaying a fixture much later, and RESTORE's ABSTTL form
	// would land every one of those keys already expired.
	TtlMs int64 `json:"ttlMs"`
	// Payload is the base64 of the raw DUMP serialization — the same
	// binary format the RDB uses, carrying the value's type, its
	// encoding, an RDB version, and a CRC64 footer that RESTORE
	// verifies.
	Payload string `json:"payload"`
}

// Export captures every key matching pattern and writes it to a single
// JSON file, returned via `dag.CurrentModule().WorkdirFile`. ImportFile
// reverses it, so the pair moves a pre-seeded cache between servers —
// the fixture path for a test that needs a populated keyspace it did not
// build command by command.
//
// Each key is captured with DUMP (the binary serialization RDB itself
// uses, so every type and encoding survives — strings, hashes, lists,
// sets, sorted sets, streams, and module types alike) plus its PTTL. The
// keyspace is walked with SCAN, cursor to exhaustion, with the same
// cluster fan-out Keys performs.
//
// This deliberately avoids the obvious approach of extracting
// `/data/dump.rdb`: a running Dagger Service's filesystem is not
// directly readable, so pulling the RDB out would mean restructuring the
// node as a Container with an exported mount. DUMP/RESTORE is pure
// client-side work and needs no filesystem access at all, which is also
// what makes it work unchanged against a remote managed instance
// (ElastiCache, MemoryDB) where there is no filesystem to reach.
//
// Only the client's logical database is exported. A key that expires
// between the SCAN that named it and the DUMP that captures it is
// skipped rather than failing the export.
//
// +cache="never"
func (c *Client) Export(
	ctx context.Context,
	// +default="*"
	pattern string,
) (*dagger.File, error) {
	if pattern == "" {
		return nil, fmt.Errorf(`pattern must not be empty; pass "*" to export the whole keyspace`)
	}

	client, cleanup, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	keys, err := c.scanKeys(ctx, client, pattern)
	if err != nil {
		return nil, err
	}

	out := keyspaceExport{
		Version: keyspaceExportVersion,
		Pattern: pattern,
		Keys:    make([]exportedKey, 0, len(keys)),
	}
	for start := 0; start < len(keys); start += keyspaceBatch {
		batch := keys[start:min(start+keyspaceBatch, len(keys))]

		// DUMP and PTTL are issued as one interleaved pipeline so a
		// batch costs a single round trip rather than two per key.
		cmds := make([]valkey.Completed, 0, 2*len(batch))
		for _, key := range batch {
			cmds = append(cmds,
				client.B().Dump().Key(key).Build(),
				client.B().Pttl().Key(key).Build(),
			)
		}
		replies := client.DoMulti(ctx, cmds...)

		for i, key := range batch {
			payload, err := replies[2*i].AsBytes()
			if err != nil {
				if valkey.IsValkeyNil(err) {
					// Expired (or was deleted) between the SCAN that
					// named it and this DUMP.
					continue
				}
				return nil, fmt.Errorf("dump %s: %w", key, err)
			}
			pttl, err := replies[2*i+1].ToInt64()
			if err != nil {
				return nil, fmt.Errorf("pttl %s: %w", key, err)
			}
			// -2 is "no such key" — the same race as a nil DUMP, just
			// lost on the second half of the pipeline. -1 is "no
			// expiry", which RESTORE spells as a 0 TTL.
			if pttl == -2 {
				continue
			}
			if pttl < 0 {
				pttl = 0
			}
			out.Keys = append(out.Keys, exportedKey{
				Key:     key,
				TtlMs:   pttl,
				Payload: base64.StdEncoding.EncodeToString(payload),
			})
		}
	}

	body, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal export: %w", err)
	}
	return writeWorkdirFile(keyspaceExportFile, body)
}

// ImportFile restores a file written by Export into the client's logical
// database and returns how many keys it wrote, TTLs included.
//
// It is not called `Import`, which is what the symmetry with Export
// wants, because it cannot be: the Go SDK caches every scalar-returning
// function on a generated struct field named after the GraphQL field, so
// an `Import` returning an Int renders as `import *int` in every
// consumer module's bindings and refuses to compile. `ApplyFile` is the
// naming this module already uses for the other file-taking method.
//
// Without replace a key already present in the target is a hard error:
// RESTORE answers BUSYKEY, which is the desired default for a fixture
// load — silently clobbering a keyspace someone else is using is worse
// than refusing. Pass replace to overwrite instead.
//
// The import is not transactional. The restores are pipelined in
// batches, so a mid-file failure (a collision, a corrupt payload, an RDB
// version the server is too old to read) leaves the keys already written
// in place. Re-running with replace after fixing the cause is the
// intended recovery.
//
// +cache="never"
func (c *Client) ImportFile(
	ctx context.Context,
	file *dagger.File,
	// +default=false
	replace bool,
) (int, error) {
	contents, err := file.Contents(ctx)
	if err != nil {
		return 0, fmt.Errorf("read export file: %w", err)
	}

	var payload keyspaceExport
	if err := json.Unmarshal([]byte(contents), &payload); err != nil {
		return 0, fmt.Errorf("parse export file: %w", err)
	}
	if payload.Version != keyspaceExportVersion {
		return 0, fmt.Errorf("export file is version %d; this module reads version %d", payload.Version, keyspaceExportVersion)
	}

	// Every payload is decoded before a single command is sent, so a
	// truncated or hand-edited file is rejected without having written
	// half of itself into the keyspace first.
	type restorable struct {
		key   string
		ttlMs int64
		blob  string
	}
	entries := make([]restorable, 0, len(payload.Keys))
	for i, entry := range payload.Keys {
		if entry.Key == "" {
			return 0, fmt.Errorf("export file entry %d has an empty key name", i)
		}
		if entry.TtlMs < 0 {
			return 0, fmt.Errorf("export file entry %q has a negative ttl of %dms", entry.Key, entry.TtlMs)
		}
		blob, err := base64.StdEncoding.DecodeString(entry.Payload)
		if err != nil {
			return 0, fmt.Errorf("decode payload for %q: %w", entry.Key, err)
		}
		entries = append(entries, restorable{key: entry.Key, ttlMs: entry.TtlMs, blob: string(blob)})
	}

	client, cleanup, err := c.dial(ctx)
	if err != nil {
		return 0, err
	}
	defer cleanup()

	restored := 0
	for start := 0; start < len(entries); start += keyspaceBatch {
		batch := entries[start:min(start+keyspaceBatch, len(entries))]

		cmds := make([]valkey.Completed, 0, len(batch))
		for _, entry := range batch {
			cmd := client.B().Restore().Key(entry.key).Ttl(entry.ttlMs).SerializedValue(entry.blob)
			if replace {
				cmds = append(cmds, cmd.Replace().Build())
				continue
			}
			cmds = append(cmds, cmd.Build())
		}

		for i, res := range client.DoMulti(ctx, cmds...) {
			if err := res.Error(); err != nil {
				return 0, fmt.Errorf("restore %s: %w", batch[i].key, err)
			}
			restored++
		}
	}
	return restored, nil
}

// writeWorkdirFile writes content to a content-addressed subdir of the
// module's scratch workdir and returns it as a *dagger.File. The subdir
// name is derived from a hash of the content, so distinct outputs land
// at distinct WorkdirFile paths (different Dagger File IDs) and
// identical outputs are idempotent.
//
// The write goes through a sibling temp file plus an atomic rename, so
// concurrent callers materializing the same content can never observe a
// partially written file.
func writeWorkdirFile(name string, content []byte) (*dagger.File, error) {
	sum := sha256.Sum256(content)
	dir := "out-" + hex.EncodeToString(sum[:])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name)

	tmp, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return nil, err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return nil, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return nil, err
	}
	return dag.CurrentModule().WorkdirFile(path), nil
}
