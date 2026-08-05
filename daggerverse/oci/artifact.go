package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"dagger/oci/internal/dagger"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	orascontent "oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/errdef"
	orasremote "oras.land/oras-go/v2/registry/remote"
)

// blobMediaType is the media type given to every file this module uploads as
// an artifact layer.
//
// Each file becomes one layer holding that file's raw bytes, named by the
// standard org.opencontainers.image.title annotation. The alternative — one
// tar layer for the whole directory — would make Fetch return an archive
// rather than the bytes that went in, and a caller wanting an archive can
// put one in the directory.
const blobMediaType = "application/octet-stream"

// PushArtifact pushes the files in contents to repository:tag as an OCI
// artifact of artifactType, and returns the manifest digest.
//
// Every file in contents, at any depth, becomes one layer whose
// org.opencontainers.image.title annotation is its path relative to the
// directory root. Layers are ordered by that path so the same directory
// always produces the same manifest.
//
// +cache="never"
func (reg *Registry) PushArtifact(
	ctx context.Context,
	// Repository path within the registry.
	repository string,
	// Tag to publish under.
	tag string,
	// Files to upload. Must contain at least one file.
	contents *dagger.Directory,
	// The artifact's type, e.g. "application/vnd.example.sbom.v1+json". This
	// is what a consumer filters on when listing referrers.
	artifactType string,
) (string, error) {
	if err := validateRepository(repository); err != nil {
		return "", err
	}
	if err := validateTag(tag); err != nil {
		return "", err
	}
	if strings.TrimSpace(artifactType) == "" {
		return "", errors.New("artifactType is required")
	}

	c, err := reg.connect(ctx)
	if err != nil {
		return "", err
	}

	work, err := os.MkdirTemp("", "oci-push-artifact-*")
	if err != nil {
		return "", fmt.Errorf("create work dir: %v", err)
	}
	defer os.RemoveAll(work)

	if _, err := contents.Export(ctx, work); err != nil {
		return "", fmt.Errorf("export artifact contents: %v", err)
	}

	store := memory.New()
	layers, err := pushDirectoryLayers(ctx, store, work)
	if err != nil {
		return "", err
	}
	if len(layers) == 0 {
		return "", errors.New("contents must hold at least one file")
	}

	manifest, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, artifactType,
		oras.PackManifestOptions{Layers: layers})
	if err != nil {
		return "", fmt.Errorf("pack artifact manifest: %v", err)
	}
	if err := store.Tag(ctx, manifest, tag); err != nil {
		return "", fmt.Errorf("tag artifact manifest: %v", err)
	}

	repo, err := c.repository(repository)
	if err != nil {
		return "", err
	}
	if _, err := oras.Copy(ctx, store, tag, repo, tag, oras.DefaultCopyOptions); err != nil {
		return "", c.scrub(fmt.Errorf("push artifact to %s: %v", c.ref(repository, tag), err))
	}
	return manifest.Digest.String(), nil
}

// Attach uploads content as an OCI referrer of subject and returns the
// referrer's own digest.
//
// subject is a manifest digest in this repository; it is resolved first, so
// attaching to something that is not there fails naming the digest rather
// than leaving a dangling referrer behind.
//
// +cache="never"
func (reg *Registry) Attach(
	ctx context.Context,
	// Repository holding the subject. The referrer lands here too — the
	// referrers API is per-repository.
	repository string,
	// Digest of the manifest being attached to, e.g. "sha256:...".
	subject string,
	// The bytes to attach: one file, one layer.
	content *dagger.File,
	// The referrer's artifact type, which is what Referrers filters on.
	artifactType string,
) (string, error) {
	if err := validateRepository(repository); err != nil {
		return "", err
	}
	if strings.TrimSpace(subject) == "" {
		return "", errors.New("subject is required")
	}
	if strings.TrimSpace(artifactType) == "" {
		return "", errors.New("artifactType is required")
	}
	if content == nil {
		return "", errors.New("content is required")
	}

	c, err := reg.connect(ctx)
	if err != nil {
		return "", err
	}
	repo, err := c.repository(repository)
	if err != nil {
		return "", err
	}

	subjectDesc, err := repo.Resolve(ctx, subject)
	if err != nil {
		return "", c.scrub(fmt.Errorf("resolve subject %s in %s: %v", subject, repository, err))
	}

	work, err := os.MkdirTemp("", "oci-attach-*")
	if err != nil {
		return "", fmt.Errorf("create work dir: %v", err)
	}
	defer os.RemoveAll(work)

	title, err := content.Name(ctx)
	if err != nil {
		return "", fmt.Errorf("read content name: %v", err)
	}
	if title == "" {
		title = "content"
	}
	path := filepath.Join(work, "content")
	if _, err := content.Export(ctx, path); err != nil {
		return "", fmt.Errorf("export attachment content: %v", err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is this function's own temp dir
	if err != nil {
		return "", fmt.Errorf("read attachment content: %v", err)
	}

	store := memory.New()
	layer := orascontent.NewDescriptorFromBytes(blobMediaType, data)
	layer.Annotations = map[string]string{ocispec.AnnotationTitle: title}
	if err := store.Push(ctx, layer, bytes.NewReader(data)); err != nil {
		return "", fmt.Errorf("stage attachment layer: %v", err)
	}

	manifest, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, artifactType,
		oras.PackManifestOptions{
			Layers:  []ocispec.Descriptor{layer},
			Subject: &subjectDesc,
		})
	if err != nil {
		return "", fmt.Errorf("pack referrer manifest: %v", err)
	}

	// CopyGraph rather than Copy: a referrer is addressed by digest and
	// carries no tag of its own. The subject is a successor of the manifest,
	// but it already exists at the destination, so the walk skips it instead
	// of looking for it in this in-memory store.
	if err := oras.CopyGraph(ctx, store, repo, manifest, oras.DefaultCopyGraphOptions); err != nil {
		return "", c.scrub(fmt.Errorf("push referrer to %s: %v", c.ref(repository, ""), err))
	}
	return manifest.Digest.String(), nil
}

// Referrers lists the artifacts attached to subject as a JSON array of OCI
// descriptors, newest registry ordering preserved.
//
// It returns JSON rather than a typed object for two reasons: codegen has no
// map type, so annotations could not be modelled; and a module object
// returned from a never-cached call detaches in Dagger v0.21, so lazily
// reading its fields fails.
//
// +cache="never"
func (reg *Registry) Referrers(
	ctx context.Context,
	// Repository holding the subject.
	repository string,
	// Digest of the manifest whose referrers are wanted.
	subject string,
	// Restrict the listing to one artifact type. Empty lists them all.
	//
	// +optional
	artifactType string,
) (string, error) {
	if err := validateRepository(repository); err != nil {
		return "", err
	}
	if strings.TrimSpace(subject) == "" {
		return "", errors.New("subject is required")
	}

	c, err := reg.connect(ctx)
	if err != nil {
		return "", err
	}
	repo, err := c.repository(repository)
	if err != nil {
		return "", err
	}

	subjectDesc, err := repo.Resolve(ctx, subject)
	if err != nil {
		return "", c.scrub(fmt.Errorf("resolve subject %s in %s: %v", subject, repository, err))
	}

	found := []ocispec.Descriptor{}
	err = repo.Referrers(ctx, subjectDesc, artifactType, func(page []ocispec.Descriptor) error {
		found = append(found, page...)
		return nil
	})
	if err != nil {
		return "", c.scrub(fmt.Errorf("list referrers of %s in %s: %v", subject, repository, err))
	}

	out, err := json.Marshal(found)
	if err != nil {
		return "", fmt.Errorf("encode referrers: %v", err)
	}
	return string(out), nil
}

// repository builds an oras client for one repository on this connection.
func (c *conn) repository(repository string) (*orasremote.Repository, error) {
	repo, err := orasremote.NewRepository(c.addr + "/" + repository)
	if err != nil {
		return nil, fmt.Errorf("parse repository %q: %v", repository, err)
	}
	repo.PlainHTTP = c.insecure
	repo.Client = c.httpClient()
	return repo, nil
}

// pushDirectoryLayers stages every regular file under root into store and
// returns their descriptors, ordered by path.
func pushDirectoryLayers(ctx context.Context, store *memory.Store, root string) ([]ocispec.Descriptor, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk artifact contents: %v", err)
	}
	sort.Strings(paths)

	layers := make([]ocispec.Descriptor, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path) //nolint:gosec // path came from walking this function's own temp dir
		if err != nil {
			return nil, fmt.Errorf("read %s: %v", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil, err
		}
		desc := orascontent.NewDescriptorFromBytes(blobMediaType, data)
		desc.Annotations = map[string]string{ocispec.AnnotationTitle: filepath.ToSlash(rel)}
		// Two files with identical bytes share a digest, so the second Push
		// is a duplicate. The descriptors still differ by title annotation,
		// which is not part of the digest, so both belong in the manifest.
		if err := store.Push(ctx, desc, bytes.NewReader(data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
			return nil, fmt.Errorf("stage %s: %v", rel, err)
		}
		layers = append(layers, desc)
	}
	return layers, nil
}
