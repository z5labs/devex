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
| `bearerToken` | a token to send as-is, for a registry that issued one |
| `dockerConfig` | the contents of a `~/.docker/config.json`, as a secret |
| `service` | a Dagger-hosted registry, reached over the session network |
| `insecure` | talk plain HTTP and skip TLS verification. Off by default |

`service` replaces `host` as the address dialled, because a session service's
hostname is assigned by the engine and cannot be predicted by the caller.

`insecure` is explicit and is **not** inferred from `service` being set. The
publish path this module replaces made that inference, which meant a test
affordance decided production TLS behaviour. It is spelled `insecure` rather
than `tlsVerify` because a `+default=true` bool is unsettable from the CLI.

Client certificates for mTLS registries remain a follow-up.

#### Credentials

There are three ways to authenticate, and exactly one of them is used:

1. **`username` / `password`.** The most specific thing a caller can say, and
   the only source that names a user.
2. **`bearerToken`.** Explicit, but it says nothing about which registry it is
   for, so a caller who supplied a pair as well meant the pair.
3. **`dockerConfig`.** A file describing many registries at once, so it is the
   least specific and loses to anything aimed at this one.

Supplying none of them is an anonymous client, which is what a public registry
wants.

A lower source is **not** consulted once a higher one has been supplied, and a
401 does not fall through to the next. Retrying with a second credential would
authenticate as somebody the caller did not choose, and would turn one wrong
password into two failed attempts against a registry that may be counting
them.

```go
reg := dag.Oci().Registry("ghcr.io", dagger.OciRegistryOpts{
    DockerConfig: dag.SetSecret("docker-config", string(configJSON)),
})
```

The config is searched for the host actually dialled. Keys are matched the way
Docker wrote them rather than literally: `ghcr.io`, `https://ghcr.io` and
`https://ghcr.io/` are the same host, and Docker Hub is found under any of
`docker.io`, `index.docker.io`, `registry-1.docker.io`,
`registry.hub.docker.com` and the legacy `https://index.docker.io/v1/`. Within
an entry, `auth` (base64
`username:password`, padded or not), explicit `username`/`password`,
`registrytoken` and `identitytoken` are all read.

A config that says nothing about the host is not an error — it is a config
about other registries, and the caller gets anonymous access, which is the
same answer `docker pull` would give.

#### Credential helpers are not supported

**Credential helpers are not run.** A helper is an external
`docker-credential-*` binary the Docker CLI executes, and the module runtime
holds neither `gcloud`, nor `ecr-login`, nor a macOS keychain; reaching one
would mean shelling out to a helper container, which is the one thing this
module does not do anywhere.

A config that resolves the bound host through a helper therefore **fails**,
naming the binary it asked for:

```
docker config resolves ghcr.io through the credential helper
docker-credential-gcloud, which this module cannot run: no helper binaries
exist in the module runtime. Resolve the credential in the caller and pass it
as username/password or bearerToken
```

Falling through to anonymous instead would turn "your credential lives
somewhere I cannot reach" into an unrelated 401 from the registry, with
nothing in the message pointing at the cause.

This bites only when a helper owns *this* host. A `credsStore` beside an entry
that already holds a credential loses to that credential, and a `credsStore`
with no entry for this host is ignored — that is the ordinary shape of a
config file on any machine where somebody has run `docker login`, and treating
it as governing every host would break every anonymous pull.

#### Credentials do not reach an error

No credential value appears in an error leaving this module. Every plaintext
read while resolving one is scrubbed out of the text first, including
credentials that lost the precedence contest and, for a Docker config, the
entries for hosts this connection never dialled — the file arrived as one
secret, so all of it is the caller's secret. A `auth` blob is scrubbed as well
as the password inside it: base64 is not encryption, and the blob is the form
the credential actually travels in. Both encodings of that blob are scrubbed,
padded and unpadded, because the blob that leaks need not be the blob that was
written — anything rebuilding an `Authorization` header emits the padded form,
which is a different string carrying the identical credential.

A malformed config fails without quoting itself back, because `encoding/json`
puts the offending input in its message and the offending input is a file full
of passwords.

Nothing here reaches a container argument either: the module builds no
containers at all.

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

The bearer-token tests put an nginx gate in front of an anonymous zot, because
no registry in this repo's test estate issues bearer tokens — zot
authenticates with htpasswd and nothing else. The gate refuses everything
without one exact `Authorization: Bearer` header and answers its 401 with a
`Bearer` challenge, which is what makes both client libraries send the token
they were given rather than falling back to basic auth.

`CredentialResolutionSelfTest` is a `+check` on the module itself rather than a
test in `tests/`. It covers how a Docker config is *read* — key matching, entry
forms, helper refusal, what a malformed file may say — none of which touches
the network, and all of which would otherwise need a registry per case to
assert on a string comparison.

[zot]: https://zotregistry.dev/
