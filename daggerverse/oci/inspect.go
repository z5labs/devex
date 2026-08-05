package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"dagger/oci/internal/dagger"

	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// maxFetchBytes bounds what Fetch will pull into memory. A registry serves
// whatever it was given, and a module runtime that OOMs takes the whole call
// with it; refusing with a size is a better failure than being killed.
const maxFetchBytes = 512 << 20 // 512 MiB

// Fetch downloads one blob or manifest by digest and returns it as a file.
//
// Blobs are tried first and manifests second, because the two live at
// different registry endpoints and a caller holding a digest out of a
// manifest's layer list has no reason to know which it is.
//
// +cache="never"
func (reg *Registry) Fetch(
	ctx context.Context,
	// Repository holding the content.
	repository string,
	// Digest of the blob or manifest, e.g. "sha256:...".
	digest string,
) (*dagger.File, error) {
	if err := validateRepository(repository); err != nil {
		return nil, err
	}
	if strings.TrimSpace(digest) == "" {
		return nil, errors.New("digest is required")
	}

	c, err := reg.connect(ctx)
	if err != nil {
		return nil, err
	}
	repo, err := c.repository(repository)
	if err != nil {
		return nil, err
	}

	blobs := repo.Blobs()
	desc, blobErr := blobs.Resolve(ctx, digest)
	var body io.ReadCloser
	if blobErr == nil {
		body, err = blobs.Fetch(ctx, desc)
		if err != nil {
			return nil, c.scrub(fmt.Errorf("fetch blob %s from %s: %v", digest, repository, err))
		}
	} else {
		var manifestErr error
		desc, body, manifestErr = repo.FetchReference(ctx, digest)
		if manifestErr != nil {
			return nil, c.scrub(fmt.Errorf("fetch %s from %s: not a blob (%v) and not a manifest (%v)",
				digest, repository, blobErr, manifestErr))
		}
	}
	defer body.Close()

	if desc.Size > maxFetchBytes {
		return nil, fmt.Errorf("%s in %s is %d bytes, over the %d byte fetch limit",
			digest, repository, desc.Size, int64(maxFetchBytes))
	}
	data, err := io.ReadAll(io.LimitReader(body, maxFetchBytes+1))
	if err != nil {
		return nil, c.scrub(fmt.Errorf("read %s from %s: %v", digest, repository, err))
	}
	if int64(len(data)) > maxFetchBytes {
		return nil, fmt.Errorf("%s in %s exceeds the %d byte fetch limit", digest, repository, int64(maxFetchBytes))
	}

	name := "content"
	if _, encoded, ok := strings.Cut(digest, ":"); ok && encoded != "" {
		name = encoded
	}
	return writeWorkdirFile(name, data)
}

// Resolve returns the digest a tag currently points at.
//
// +cache="never"
func (reg *Registry) Resolve(
	ctx context.Context,
	// Repository holding the tag.
	repository string,
	// Tag to resolve.
	tag string,
) (string, error) {
	if err := validateRepository(repository); err != nil {
		return "", err
	}
	if err := validateTag(tag); err != nil {
		return "", err
	}

	c, err := reg.connect(ctx)
	if err != nil {
		return "", err
	}
	repo, err := c.repository(repository)
	if err != nil {
		return "", err
	}

	desc, err := repo.Resolve(ctx, tag)
	if err != nil {
		return "", c.scrub(fmt.Errorf("resolve tag %q in %s: %v", tag, repository, err))
	}
	return desc.Digest.String(), nil
}

// Manifest returns the raw manifest JSON for a tag or a digest. It is raw
// rather than parsed because annotations, platforms and referrer subjects are
// all read back out of it, and re-encoding through a Go type would drop
// whatever this module does not model.
//
// +cache="never"
func (reg *Registry) Manifest(
	ctx context.Context,
	// Repository holding the manifest.
	repository string,
	// Tag or digest.
	reference string,
) (string, error) {
	if err := validateRepository(repository); err != nil {
		return "", err
	}
	if strings.TrimSpace(reference) == "" {
		return "", errors.New("reference is required")
	}

	c, err := reg.connect(ctx)
	if err != nil {
		return "", err
	}
	repo, err := c.repository(repository)
	if err != nil {
		return "", err
	}

	desc, body, err := repo.FetchReference(ctx, reference)
	if err != nil {
		return "", c.scrub(fmt.Errorf("fetch manifest %q from %s: %v", reference, repository, err))
	}
	defer body.Close()

	if desc.Size > maxFetchBytes {
		return "", fmt.Errorf("manifest %q in %s is %d bytes, over the %d byte fetch limit",
			reference, repository, desc.Size, int64(maxFetchBytes))
	}
	data, err := io.ReadAll(io.LimitReader(body, maxFetchBytes+1))
	if err != nil {
		return "", c.scrub(fmt.Errorf("read manifest %q from %s: %v", reference, repository, err))
	}
	return string(data), nil
}

// insecureTransport is go-containerregistry's default transport with
// certificate verification switched off. It only matters for a registry
// serving HTTPS with a certificate this runtime does not trust; a plain-HTTP
// registry never reaches a TLS handshake.
func insecureTransport() http.RoundTripper {
	tr, ok := remote.DefaultTransport.(*http.Transport)
	if !ok {
		return remote.DefaultTransport
	}
	clone := tr.Clone()
	clone.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in via insecure
	return clone
}

// writeWorkdirFile writes content to a content-addressed subdir of the
// module's scratch workdir and returns it as a *dagger.File. The subdir name
// is derived from a hash of the content, so distinct outputs land at distinct
// WorkdirFile paths (different Dagger File IDs) and identical outputs are
// idempotent.
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
