package main

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"dagger/oci/internal/dagger"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// PushImage pushes container variants to repository:tag and returns the
// digest of what was pushed.
//
// Multiple variants become one manifest list. The variants are materialized
// through a single AsTarball with the rest as platform variants, so the
// index this pushes is the one Dagger itself would have published —
// annotations, config and layer bytes included — rather than one this module
// reassembled.
//
// repository and tag are separate parameters rather than one interpolated
// reference: it keeps caller-supplied values out of any string that gets
// re-parsed as something else, and it makes each half validatable.
//
// +cache="never"
func (reg *Registry) PushImage(
	ctx context.Context,
	// Repository path within the registry, e.g. "z5labs/myapp".
	repository string,
	// Tag to publish under.
	tag string,
	// Platform variants. One variant pushes a single image manifest; more
	// than one pushes a manifest list naming every platform.
	variants []*dagger.Container,
) (string, error) {
	if err := validateRepository(repository); err != nil {
		return "", err
	}
	if err := validateTag(tag); err != nil {
		return "", err
	}
	if len(variants) == 0 {
		return "", errors.New("at least one variant is required")
	}

	c, err := reg.connect(ctx)
	if err != nil {
		return "", err
	}

	work, err := os.MkdirTemp("", "oci-push-image-*")
	if err != nil {
		return "", fmt.Errorf("create work dir: %v", err)
	}
	defer os.RemoveAll(work)

	tarPath := filepath.Join(work, "image.tar")
	tarball := variants[0].AsTarball(dagger.ContainerAsTarballOpts{
		PlatformVariants: variants[1:],
	})
	if _, err := tarball.Export(ctx, tarPath); err != nil {
		return "", fmt.Errorf("export image tarball: %v", err)
	}

	layoutDir := filepath.Join(work, "layout")
	if err := extractTar(tarPath, layoutDir); err != nil {
		return "", fmt.Errorf("extract image tarball: %v", err)
	}

	idx, err := layout.ImageIndexFromPath(layoutDir)
	if err != nil {
		return "", fmt.Errorf("read oci layout: %v", err)
	}
	root, err := layoutRoot(idx)
	if err != nil {
		return "", err
	}

	ref, err := name.NewTag(c.ref(repository, tag), c.nameOptions()...)
	if err != nil {
		return "", fmt.Errorf("parse destination reference: %v", err)
	}

	opts := c.remoteOptions(ctx)
	switch typed := root.(type) {
	case v1.ImageIndex:
		if err := remote.WriteIndex(ref, typed, opts...); err != nil {
			return "", c.scrub(fmt.Errorf("push manifest list to %s: %v", ref, err))
		}
		return digestOf(typed.Digest())
	case v1.Image:
		if err := remote.Write(ref, typed, opts...); err != nil {
			return "", c.scrub(fmt.Errorf("push image to %s: %v", ref, err))
		}
		return digestOf(typed.Digest())
	default:
		return "", fmt.Errorf("unsupported oci layout root %T", root)
	}
}

// Copy copies srcRef into repository:tag on this registry, preserving every
// manifest — a multi-platform source stays multi-platform, matching what
// `skopeo copy --all` did before this module existed. It returns the digest
// at the destination.
//
// The source is read with this registry's credentials when it lives on this
// registry, and anonymously otherwise; cross-registry copies needing source
// credentials are a follow-up, not a silent reuse of the destination's.
//
// +cache="never"
func (reg *Registry) Copy(
	ctx context.Context,
	// Fully-qualified source reference, e.g. "docker.io/library/alpine:3.20"
	// or "<host>/<repo>@sha256:...".
	srcRef string,
	// Destination repository on this registry.
	repository string,
	// Destination tag.
	tag string,
) (string, error) {
	if strings.TrimSpace(srcRef) == "" {
		return "", errors.New("srcRef is required")
	}
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

	// The source shares this registry's scheme and credentials only when it
	// is this registry. Deciding that from the parsed name rather than from
	// a flag means a caller cannot accidentally send the destination's
	// password to a third-party host.
	var srcNameOpts []name.Option
	if strings.HasPrefix(srcRef, c.addr+"/") && c.insecure {
		srcNameOpts = append(srcNameOpts, name.Insecure)
	}
	src, err := name.ParseReference(srcRef, srcNameOpts...)
	if err != nil {
		return "", fmt.Errorf("parse source reference %q: %v", srcRef, err)
	}

	srcOpts := []remote.Option{remote.WithContext(ctx)}
	if src.Context().RegistryStr() == c.addr {
		srcOpts = c.remoteOptions(ctx)
	} else {
		srcOpts = append(srcOpts, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	}

	desc, err := remote.Get(src, srcOpts...)
	if err != nil {
		return "", c.scrub(fmt.Errorf("read source %s: %v", srcRef, err))
	}

	dst, err := name.NewTag(c.ref(repository, tag), c.nameOptions()...)
	if err != nil {
		return "", fmt.Errorf("parse destination reference: %v", err)
	}
	dstOpts := c.remoteOptions(ctx)

	if desc.MediaType.IsIndex() {
		idx, err := desc.ImageIndex()
		if err != nil {
			return "", fmt.Errorf("read source manifest list: %v", err)
		}
		if err := remote.WriteIndex(dst, idx, dstOpts...); err != nil {
			return "", c.scrub(fmt.Errorf("copy manifest list to %s: %v", dst, err))
		}
		return digestOf(idx.Digest())
	}

	img, err := desc.Image()
	if err != nil {
		return "", fmt.Errorf("read source image: %v", err)
	}
	if err := remote.Write(dst, img, dstOpts...); err != nil {
		return "", c.scrub(fmt.Errorf("copy image to %s: %v", dst, err))
	}
	return digestOf(img.Digest())
}

// layoutRoot picks the thing an OCI layout is actually describing.
//
// Dagger's AsTarball writes a layout whose index.json wraps the pushable
// object: one image-manifest descriptor for a single platform, and for
// several either one index descriptor or the variants listed directly. All
// three shapes are handled because which one appears is Dagger's choice, not
// this module's, and it has changed between engine versions.
func layoutRoot(idx v1.ImageIndex) (any, error) {
	im, err := idx.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("read oci layout index: %v", err)
	}
	switch len(im.Manifests) {
	case 0:
		return nil, errors.New("oci layout contains no manifests")
	case 1:
		desc := im.Manifests[0]
		if desc.MediaType.IsIndex() {
			child, err := idx.ImageIndex(desc.Digest)
			if err != nil {
				return nil, fmt.Errorf("read manifest list %s from layout: %v", desc.Digest, err)
			}
			return child, nil
		}
		img, err := idx.Image(desc.Digest)
		if err != nil {
			return nil, fmt.Errorf("read image %s from layout: %v", desc.Digest, err)
		}
		return img, nil
	default:
		// The layout's own index already is the manifest list.
		return idx, nil
	}
}

func digestOf(h v1.Hash, err error) (string, error) {
	if err != nil {
		return "", fmt.Errorf("compute digest: %v", err)
	}
	return h.String(), nil
}

// nameOptions carries the plain-HTTP decision into reference parsing.
// go-containerregistry derives the scheme from the parsed name, so this is
// where insecure has to be applied — not on the transport.
func (c *conn) nameOptions() []name.Option {
	if c.insecure {
		return []name.Option{name.Insecure}
	}
	return nil
}

// remoteOptions builds the go-containerregistry options for this connection.
//
// RegistryToken is go-containerregistry's name for a bearer token sent as-is,
// and IdentityToken its name for an OAuth2 refresh token — the same two
// things oras calls AccessToken and RefreshToken. The mapping lives here so
// that credential resolution never has to know which library will be used.
func (c *conn) remoteOptions(ctx context.Context) []remote.Option {
	opts := []remote.Option{remote.WithContext(ctx)}
	if !c.cred.empty() {
		opts = append(opts, remote.WithAuth(authn.FromConfig(authn.AuthConfig{
			Username:      c.cred.username,
			Password:      c.cred.password,
			RegistryToken: c.cred.accessToken,
			IdentityToken: c.cred.refreshToken,
		})))
	} else {
		opts = append(opts, remote.WithAuth(authn.Anonymous))
	}
	if c.insecure {
		opts = append(opts, remote.WithTransport(insecureTransport()))
	}
	return opts
}

// extractTar unpacks a tar stream into dir. The archive is an OCI layout
// produced by the engine, so only regular files and directories are expected;
// anything else is refused rather than skipped, because a layout that
// contains one is not a layout this module knows how to push.
func extractTar(tarPath, dir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(dir, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			//nolint:gosec // the archive is engine-produced, not caller-supplied
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected entry %q of type %d in oci layout", hdr.Name, hdr.Typeflag)
		}
	}
}

// safeJoin refuses archive entries that would escape dir.
func safeJoin(dir, entry string) (string, error) {
	target := filepath.Join(dir, filepath.Clean("/"+entry))
	if !strings.HasPrefix(target, filepath.Clean(dir)+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive entry %q escapes the extraction directory", entry)
	}
	return target, nil
}
