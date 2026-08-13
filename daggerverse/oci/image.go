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
// A caller that has fallible work to do between the push and the moment the
// image becomes resolvable — attaching referrers, say — wants
// PushImageUntagged and Tag instead, which split this into its two halves.
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
	// Repository before tag, which is the order this function reported its
	// refusals in before the body was shared with PushImageUntagged. pushImage
	// checks the repository again and that is the point: the duplicated check
	// is what keeps the order a property of this signature rather than of
	// whichever body happens to run.
	if err := validateRepository(repository); err != nil {
		return "", err
	}
	if err := validateTag(tag); err != nil {
		return "", err
	}
	return reg.pushImage(ctx, repository, tag, variants)
}

// PushImageUntagged pushes container variants to repository under their own
// digest and no tag at all, and returns that digest.
//
// The bytes land exactly as PushImage lands them — same manifest list, same
// blobs — but nothing in the repository names them, so nothing that resolves
// a tag can reach them. That is the point: a caller with fallible work to do
// against the pushed digest (attaching SBOMs, attaching provenance) can do it
// while the image is unreachable, and call Tag only once that work is done. A
// failure in between leaves an unreferenced manifest rather than a tag a
// consumer can pull.
//
// Pushing a manifest by digest is the same registry operation the referrers
// path already relies on, so it needs nothing of a registry that Attach does
// not need already.
//
// The manifest is unreferenced until it is tagged or something points at it,
// which means a registry running garbage collection is entitled to delete it.
// Registries collect on an operator-run sweep rather than continuously — it is
// offline and manual on distribution, and scheduled on GHCR — so the window
// this opens is not one a publish has to design around. A caller that leaves a
// digest untagged indefinitely is a caller relying on something no registry
// promises.
//
// +cache="never"
func (reg *Registry) PushImageUntagged(
	ctx context.Context,
	// Repository path within the registry, e.g. "z5labs/myapp".
	repository string,
	// Platform variants. One variant pushes a single image manifest; more
	// than one pushes a manifest list naming every platform.
	variants []*dagger.Container,
) (string, error) {
	return reg.pushImage(ctx, repository, "", variants)
}

// pushImage is the body of both pushes. An empty tag means "address the
// manifest by its own digest", which is what makes the untagged push the same
// code path rather than a second one that agrees with it.
func (reg *Registry) pushImage(ctx context.Context, repository, tag string, variants []*dagger.Container) (string, error) {
	if err := validateRepository(repository); err != nil {
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

	opts := c.remoteOptions(ctx)
	switch typed := root.(type) {
	case v1.ImageIndex:
		ref, err := c.destination(repository, tag, typed.Digest)
		if err != nil {
			return "", err
		}
		if err := remote.WriteIndex(ref, typed, opts...); err != nil {
			return "", c.scrub(fmt.Errorf("push manifest list to %s: %v", ref, err))
		}
		return digestOf(typed.Digest())
	case v1.Image:
		ref, err := c.destination(repository, tag, typed.Digest)
		if err != nil {
			return "", err
		}
		if err := remote.Write(ref, typed, opts...); err != nil {
			return "", c.scrub(fmt.Errorf("push image to %s: %v", ref, err))
		}
		return digestOf(typed.Digest())
	default:
		return "", fmt.Errorf("unsupported oci layout root %T", root)
	}
}

// destination renders the reference a push writes to: repository:tag when a
// tag was given, and repository@sha256:... when none was.
//
// digest is taken as the function that computes it rather than as a value, so
// that the digest of a manifest list is only ever computed when it is needed —
// a tagged push does not have to hash anything the registry is about to hash
// anyway.
func (c *conn) destination(repository, tag string, digest func() (v1.Hash, error)) (name.Reference, error) {
	if tag != "" {
		ref, err := name.NewTag(c.ref(repository, tag), c.nameOptions()...)
		if err != nil {
			return nil, fmt.Errorf("parse destination reference: %v", err)
		}
		return ref, nil
	}
	h, err := digest()
	if err != nil {
		return nil, fmt.Errorf("compute digest: %v", err)
	}
	ref, err := name.NewDigest(c.ref(repository, h.String()), c.nameOptions()...)
	if err != nil {
		return nil, fmt.Errorf("parse destination reference: %v", err)
	}
	return ref, nil
}

// Tag points tag at a manifest already in repository, named by its digest,
// and returns the digest it now resolves to.
//
// It moves an existing tag as readily as it creates a new one — a tag is a
// mutable name, and a registry PUT of a manifest under a tag is the only
// operation either case has. What it will not do is invent the bytes: the
// digest is read from the registry first, so tagging something that is not
// there fails naming the digest instead of leaving a tag that resolves to
// nothing.
//
// Nothing is re-uploaded. The manifest is fetched and PUT back under the new
// name, which is bytes the registry already holds; the blobs it names are
// untouched.
//
// +cache="never"
func (reg *Registry) Tag(
	ctx context.Context,
	// Repository holding the manifest.
	repository string,
	// Digest of the manifest to name, e.g. "sha256:...".
	digest string,
	// Tag to point at it.
	tag string,
) (string, error) {
	if err := validateRepository(repository); err != nil {
		return "", err
	}
	if strings.TrimSpace(digest) == "" {
		return "", errors.New("digest is required")
	}
	if err := validateTag(tag); err != nil {
		return "", err
	}

	c, err := reg.connect(ctx)
	if err != nil {
		return "", err
	}

	src, err := name.NewDigest(c.ref(repository, digest), c.nameOptions()...)
	if err != nil {
		return "", fmt.Errorf("parse digest reference: %v", err)
	}
	dst, err := name.NewTag(c.ref(repository, tag), c.nameOptions()...)
	if err != nil {
		return "", fmt.Errorf("parse destination reference: %v", err)
	}

	opts := c.remoteOptions(ctx)
	// The descriptor carries the manifest bytes and its media type, and
	// go-containerregistry's Taggable unpacking takes both straight off it, so
	// the manifest is PUT back exactly as the registry served it. Re-parsing it
	// into an Image or an ImageIndex first and re-serializing that is how a
	// field this module does not model gets dropped on the way through.
	desc, err := remote.Get(src, opts...)
	if err != nil {
		return "", c.scrub(fmt.Errorf("read %s in %s: %v", digest, repository, err))
	}
	if err := remote.Tag(dst, desc, opts...); err != nil {
		return "", c.scrub(fmt.Errorf("tag %s as %s: %v", digest, dst, err))
	}
	return desc.Digest.String(), nil
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
	if cfg := c.tlsClientConfig(); cfg != nil {
		opts = append(opts, remote.WithTransport(tlsTransport(cfg)))
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
