# oci

A Dagger module for talking to an OCI registry: push images and artifacts,
copy between repositories, attach and list referrers, and read manifests,
tags and blobs back.

It knows how to talk to a registry and nothing about why. It does not choose
tags, decide when to publish, or know what the bytes it uploads mean — the
caller owns all of that.

## Pure Go, no tool images

`Container.Publish` runs in the engine's BuildKit context, which does not see
session service bindings, so a module that needed to reach a Dagger-hosted
registry used to shell out to a `skopeo` container that could. Module Go code
has no such restriction: it reaches a Dagger service directly through
`Service.Endpoint()`.

So this module wraps [go-containerregistry][ggcr] and [oras-go][oras] rather
than pinning tool images. There is no helper container anywhere in it, and
nothing to keep in step with an upstream image tag.

[ggcr]: https://github.com/google/go-containerregistry
[oras]: https://oras.land/

## Caching

Registry state is mutable and pushes are side-effecting, so every function
here carries `+cache="never"` — `Registry()` and every method chained off it.
A cached `Resolve` would report the digest a moving tag pointed at a week ago;
a cached `PushImage` would report success without uploading anything.

## Functions

- [Registry](#registry) — bind a registry host and its credentials
- [PushImage](#pushimage) — publish container variants as an image or manifest list
- [PushArtifact](#pushartifact) — publish a directory as an OCI artifact
- [Copy](#copy) — copy a reference into this registry, all manifests intact
- [Attach](#attach) — upload a referrer against a subject manifest
- [Referrers](#referrers) — list what is attached to a subject
- [Fetch](#fetch) — download a blob or manifest by digest
- [Resolve](#resolve) — the digest a tag points at
- [Manifest](#manifest) — raw manifest JSON

### Registry

Binds one registry host and its credentials. Everything else chains off it.

```go
reg := dag.Oci().Registry("ghcr.io", dagger.OciRegistryOpts{
    Username: "z5labs",
    Password: token,
})
```

| argument | meaning |
| --- | --- |
| `host` | registry host, as it appears in an image reference. Ignored when `service` is set |
| `username` | basic-auth user; omit for an anonymous client |
| `password` | basic-auth secret |
| `service` | a Dagger-hosted registry, reached over the session network |
| `insecure` | talk plain HTTP and skip TLS verification. Off by default |

`service` replaces `host` as the address dialled, because a session service's
hostname is assigned by the engine and cannot be predicted by the caller.

`insecure` is explicit and is **not** inferred from `service` being set. The
publish path this module replaces made that inference, which meant a test
affordance decided production TLS behaviour. It is spelled `insecure` rather
than `tlsVerify` because a `+default=true` bool is unsettable from the CLI.

Password authentication only. Client certificates for mTLS registries, and
credential-helper and token flows, are follow-ups.

### PushImage

Pushes container variants to `repository:tag` and returns the pushed digest.
One variant pushes an image manifest; more than one pushes a manifest list
naming every platform.

```go
digest, err := reg.PushImage(ctx, "z5labs/myapp", "v1.2.3", []*dagger.Container{
    amd64, arm64,
})
```

`repository` and `tag` are separate parameters rather than one interpolated
reference: it keeps caller-supplied values out of any string that gets
re-parsed as something else, and it makes each half validatable.

Annotations set with `Container.WithAnnotation` survive the push and are
readable through [Manifest](#manifest).

### PushArtifact

Pushes the files in a directory to `repository:tag` as an OCI artifact of
`artifactType`, and returns the manifest digest.

```go
digest, err := reg.PushArtifact(ctx, "z5labs/myapp", "sbom",
    dag.Directory().WithFile("sbom.json", doc),
    "application/spdx+json")
```

Every file, at any depth, becomes one layer holding that file's raw bytes,
named by its path in the standard `org.opencontainers.image.title`
annotation. Layers are ordered by path, so the same directory always produces
the same manifest. One tar layer for the whole tree would make
[Fetch](#fetch) return an archive rather than the bytes that went in.

### Copy

Copies a reference into `repository:tag` on this registry, preserving every
manifest — a multi-platform source stays multi-platform, which is what
`skopeo copy --all` was for.

```go
digest, err := reg.Copy(ctx, "docker.io/library/alpine:3.20", "mirror/alpine", "3.20")
```

The source is read with this registry's credentials when it lives on this
registry, and anonymously otherwise. Cross-registry copies needing separate
source credentials are a follow-up, not a silent reuse of the destination's
password.

### Attach

Uploads a file as an OCI referrer of `subject`, and returns the referrer's own
digest.

```go
referrer, err := reg.Attach(ctx, "z5labs/myapp", imageDigest, sbom, "application/spdx+json")
```

`subject` is resolved first, so attaching to something that is not in the
repository fails naming the digest instead of leaving a dangling referrer
behind.

### Referrers

Lists the artifacts attached to `subject` as a JSON array of OCI descriptors.
`artifactType` is optional and narrows the listing to one type.

```go
raw, err := reg.Referrers(ctx, "z5labs/myapp", imageDigest,
    dagger.OciRegistryReferrersOpts{ArtifactType: "application/spdx+json"})
```

It returns JSON rather than a typed object for two reasons: codegen has no map
type, so annotations could not be modelled; and a module object returned from
a `+cache="never"` call detaches in Dagger v0.21, so lazily reading its fields
fails.

### Fetch

Downloads one blob or manifest by digest and returns it as a `*dagger.File`.
Blobs are tried first and manifests second, because the two live at different
registry endpoints and a caller holding a digest out of a layer list has no
reason to know which it is.

```go
contents, err := reg.Fetch("z5labs/myapp", layerDigest).Contents(ctx)
```

### Resolve

Returns the digest a tag currently points at.

```go
digest, err := reg.Resolve(ctx, "z5labs/myapp", "latest")
```

### Manifest

Returns the raw manifest JSON for a tag or a digest. Raw rather than parsed:
annotations, platforms and referrer subjects are all read back out of it, and
re-encoding through a Go type would drop whatever this module does not model.

```go
raw, err := reg.Manifest(ctx, "z5labs/myapp", "v1.2.3")
```

## Tests

`tests/` runs against [zot][zot] rather than `registry:2`, because the
referrer tests need a registry serving the native OCI 1.1 referrers API.
`oras` silently falls back to the OCI 1.1 tag schema against one that does
not, so a suite green over the fallback would be evidence about the fallback
rather than about GHCR. `registry:2.8` has no such endpoint, and
`registry:3.0.0` was measured here too and does not register the route either.
`requireNativeReferrersAPI` keeps that a checked fact rather than a claim in a
comment.

[zot]: https://zotregistry.dev/
