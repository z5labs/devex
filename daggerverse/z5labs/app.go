package main

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"dagger/z-5-labs/internal/dagger"
)

// App is a built application: one container image per platform, the
// documents describing each of them, and the version they all carry.
// Construct it with GoChain.App and publish it with Publish.
//
// # It does not know what built it
//
// App holds images, documents and build facts — never a binary, never a
// source tree, never a name. That is why there is no binary-name input and
// no Binary accessor: an application's binary name is a detail of the
// language chain that compiled it, and an App that carried one would make
// every future chain — a Zig chain, a Java chain, an App assembled from
// nothing at all — have to invent one.
//
// The consequence worth stating is that App cannot describe its own
// contents. `dag.Go().Spdx(binary, source)` needs both of the things App
// refuses to hold, so a document is produced by whoever contributed each
// thing in the image and carried here as one file per contribution. Publish
// assembles them into the SPDX and CycloneDX pair it attaches, and the `oci`
// module that does the attaching never learns that any of it is an SBOM —
// the separation daggerverse/CLAUDE.md asks for. It costs nothing, because
// dag.Go().Spdx returns a lazy *dagger.File that no unpublished app ever
// evaluates. sbom.go carries the whole arrangement and why the documents
// describe the image rather than the binary.
//
// # Configuration is chained, not constructed
//
// Where the images are published, whose identity vouches for them and
// whether the connection is verified are all With* methods rather than
// constructor arguments, because none of them is a property of the
// artifact. The same App can be published to a mirror, to an internal
// registry, or nowhere at all.
type App struct {
	// Version is what every binary was stamped with and what every image
	// is published under.
	//
	// +private
	Version string
	// Commit is the unabbreviated HEAD SHA the build read. It is a build
	// fact rather than a property of the chain — an App assembled some
	// other way still came from some revision — and the provenance
	// predicate reports it.
	//
	// +private
	Commit string
	// SourceURI is the origin remote the source came from, credentials
	// already stripped, empty when the tree had no origin.
	//
	// +private
	SourceURI string
	// Pkg is the package that was built, recorded for the provenance
	// predicate's internal parameters.
	//
	// +private
	Pkg string
	// Variants are the built images and the documents describing what went
	// into each of them, one per platform, in the order the platforms were
	// given.
	//
	// The +private is load bearing: variant is an unexported type, so
	// exposing this field would ask the generator to register a schema
	// object it cannot build. Unexported types round-trip across the call
	// boundary intact as long as their own fields stay exported — the
	// round trip is encoding/json.
	//
	// +private
	Variants []*variant
	// ContributedPaths are the image paths something has been placed at
	// since the images were built, in the order they were placed, each
	// beside what holds it. It is what makes a second contribution
	// overlapping the first refusable — see contribute.go — and it is a
	// field rather than something derived from the variants because a
	// variant's contributions are named for error messages and a language
	// chain's are named after a binary rather than a path.
	//
	// The holder travels with the path because it is the whole content of
	// the refusal: "something is already contributed at /usr/local/bin/gen"
	// tells a caller nothing they can act on, while "the entry of the
	// application composed at /usr/local/bin/gen" tells them which call to
	// go and look at.
	//
	// +private
	ContributedPaths []occupied
	// Payload is what this application's constructor declared makes it
	// runnable: the paths it put in every image and which of them is the
	// entry. It is what crosses when this App is composed into another's
	// image, and it is a declaration rather than anything read back off a
	// container — compose.go records why reading the entrypoint instead
	// fails silently in four different ways.
	//
	// +private
	Payload appPayload
	// Composed are the applications composed into this one, each with the
	// path its entry landed at in these images and the version it was built
	// under. Publish executes every one of them before it pushes anything,
	// and records them in the provenance predicate's internal parameters —
	// which is the only place that says which release of a plugin shipped,
	// since the derived image is published under the base's version.
	//
	// +private
	Composed []composedApp

	// +private
	Registry string
	// +private
	RegistryUsername string
	// +private
	RegistryAuth *dagger.Secret
	// +private
	RegistryService *dagger.Service
	// +private
	Insecure bool
	// +private
	IDTokenRequestURL string
	// +private
	IDTokenRequestToken *dagger.Secret
	// +private
	IDTokenService *dagger.Service
	// +private
	SigningKey *dagger.Secret
	// FulcioService and RekorService are a sigstore standing inside this
	// session, in place of the public one. They are set together or not at
	// all — see WithSessionSigstore, which is the only thing that sets
	// them.
	//
	// +private
	FulcioService *dagger.Service
	// +private
	RekorService *dagger.Service
}

// variant is one platform's image and the documents describing what went
// into it.
type variant struct {
	Platform  dagger.Platform
	Container *dagger.Container
	// Contributions is one document per thing that entered this image. They
	// are inputs to the image-level documents Publish assembles and attaches,
	// not attachments themselves — see sbom.go for why the subject moved from
	// the binary to the image, and what a consumer fetches as a result.
	Contributions []contribution
}

// contribution is one thing that entered the image, and the document
// describing it: an SPDX 2.3 JSON file, whatever produced the bytes.
//
// Name is not published. It names the contribution in an error, which is
// the whole reason it is carried: a publish that fails because one document
// of five will not parse has to be able to say which one.
//
// The bytes themselves are carried beside the document, in exactly one of
// Content and Tree, and that is what turns an asserted document into a
// checkable one: a publish hashes them and refuses a document naming some
// other digest. Before this they were not held at all — the image had them and
// nothing connected them to the document — so a document about the wrong
// artifact was undetectable by construction. digest.go carries the rule and
// why it runs where it runs.
type contribution struct {
	Name string
	// Path is where in the image the bytes live. It is what composition
	// reads to move a contribution into a derived image, and it is carried
	// beside Name rather than replacing it because the two differ for an
	// application's own executable: Name is what it is called, which is what
	// an error message about its document should say, and Path is where it
	// is, which is what a copy needs.
	Path string
	File *dagger.File
	// Content is the file that entered the image: an application's own
	// executable, or a contributed file. Nil for a directory contribution.
	Content *dagger.File
	// Tree is the directory that entered the image. Nil for everything else.
	Tree *dagger.Directory
}

// Container returns the image built for platform.
//
// This is the same container Publish pushes, not a second build that merely
// agrees with it: GoChain.App and this method are both session-cached, so
// within one chained call the app is built once and everything downstream —
// a check on the image, the publish, an export — sees those exact bytes.
// That is what makes a check seam meaningful, and it is why the caching is
// part of the API rather than an optimization.
//
// The guarantee is bounded by the session. Two separate `dagger call`
// invocations are two sessions and two builds, and while a build of one
// (commit, version) pair is byte-identical to another by construction,
// nothing here promises that the second invocation reuses the first's
// containers. A caller that needs one build inspected and then published
// chains both onto one call.
//
// What this container has *not* been through is the image-configuration
// check. That is a publish-time gate — App.Publish holds every variant to
// expectedImageConfig before the first byte moves — and it deliberately does
// not run here. An inspection seam exists for the image that is wrong as much
// as for the one that is right, and a Container that refused to hand back a
// container whose configuration failed the gate would withhold the bytes at
// exactly the moment somebody is trying to find out what is wrong with them;
// `docker load` and a look at the config is how that is done.
//
// That is a statement about *this module's* publish path and not a guarantee
// about the bytes. What comes back is an ordinary core Container, and a caller
// who wants to can push it themselves — `container --platform=linux/amd64
// publish --address=…` — which goes round this gate exactly as it goes round
// the annotations, the assembled SBOMs, the provenance and the signature that
// make a z5labs release what it is. An image that leaves through Publish has
// been checked; an image somebody exported from here and pushed by hand is
// theirs.
//
// +cache="session"
func (a *App) Container(ctx context.Context, platform dagger.Platform) (*dagger.Container, error) {
	for _, v := range a.Variants {
		if v.Platform == platform {
			return v.Container, nil
		}
	}
	return nil, fmt.Errorf("this app was not built for platform %q; it carries %s", platform, strings.Join(a.platformNames(), ", "))
}

// Containers returns every platform's image, in the order the platforms
// were given to App. Same guarantee, and the same session bound, as
// Container.
//
// +cache="session"
func (a *App) Containers(ctx context.Context) ([]*dagger.Container, error) {
	out := make([]*dagger.Container, 0, len(a.Variants))
	for _, v := range a.Variants {
		out = append(out, v.Container)
	}
	return out, nil
}

// WithRegistry sets the registry to publish to.
//
// address is the registry alone — "ghcr.io", "registry.example.internal:5000"
// — and never a repository path. The repository is stated to Publish, so
// that the same app can go to a mirror or to an internal registry by
// changing this and nothing else.
//
// username and auth are the credential. There is no unauthenticated
// publish: a registry that accepts anonymous writes is one this pipeline
// has no way to tell from a misconfigured one.
//
// The +cache directive is repeated here rather than stated once for the
// chain: caching is per function in Dagger, so a chained method left
// undirected takes the default seven-day TTL and can hand a later session a
// stale object built from arguments it only appears to share — a registry
// service, for one, whose engine-assigned address is long gone.
//
// +cache="session"
func (a *App) WithRegistry(address, username string, auth *dagger.Secret) *App {
	a.Registry = address
	a.RegistryUsername = username
	a.RegistryAuth = auth
	return a
}

// WithOidc supplies the CI provider's OIDC token request machinery, which
// is what a publish signs its provenance with.
//
// requestUrl is the token request endpoint — ACTIONS_ID_TOKEN_REQUEST_URL
// on GitHub Actions, and whatever the equivalent is elsewhere.
// requestToken is the bearer for it, a secret because it is a credential
// for minting identity tokens.
//
// There is deliberately no repository, ref or commit parameter beside them.
// Every identifying field in the provenance comes out of the exchanged
// token's claims, because anything a caller could have supplied attests to
// nothing.
//
// +cache="session"
func (a *App) WithOidc(requestUrl string, requestToken *dagger.Secret) *App {
	a.IDTokenRequestURL = requestUrl
	a.IDTokenRequestToken = requestToken
	return a
}

// WithSigningKey signs the provenance with a caller-supplied PEM-encoded EC
// private key instead of an ephemeral key certified by the public sigstore
// CA.
//
// This selects the signing mode and nothing else: the workload identity
// token is still exchanged, and the predicate still says only what that
// token's claims say. Use it for a build that cannot reach a public CA.
// Leaving it unset is keyless signing and is what a normal CI publish
// should do.
//
// It changes what a consumer has to run, and the change is a downgrade
// worth stating. Nothing certifies a supplied key, so there is no identity
// to verify against and nothing to record in the public transparency log.
// That covers both signed things: the image signature carries the signature
// alone, and the provenance envelope carries a bare public key with no log
// entry, where a keyless publish gives each of them a certificate and a
// countersigned log entry. Verifying the image then means
//
//	cosign verify <ref> --key cosign.pub --insecure-ignore-tlog=true
//
// where the keyless mode gets an identity and an issuer and no such flag.
// A caller who does not want to hand their consumers that flag should not
// be supplying a key.
//
// +cache="session"
func (a *App) WithSigningKey(key *dagger.Secret) *App {
	a.SigningKey = key
	return a
}

// WithInsecure publishes over plain HTTP with no TLS verification.
//
// It is off unless a caller asks for it, and it is deliberately not
// inferred from WithRegistryService being set: that inference made a caller
// who supplied a service for their own reasons silently publish over an
// unverified connection. It is spelled insecure rather than tlsVerify
// because a bool defaulting to true cannot be turned off from the CLI —
// which is also why this method takes no argument at all.
//
// +cache="session"
func (a *App) WithInsecure() *App {
	a.Insecure = true
	return a
}

// WithRegistryService points the publish at a Dagger-hosted registry
// reached over the session network instead of over the public network.
//
// A service's endpoint is assigned by the engine, so it cannot be written
// into an address ahead of time; this is how the publish learns it. Used by
// the test suite against a local registry, and by anyone whose private
// registry is itself a Dagger service.
//
// +cache="session"
func (a *App) WithRegistryService(svc *dagger.Service) *App {
	a.RegistryService = svc
	return a
}

// WithOidcService points the token exchange at a Dagger-hosted OIDC
// endpoint, reached over the session network instead of the public one.
// Its engine-assigned endpoint replaces the host in WithOidc's requestUrl;
// the path and query stay the caller's, because those are part of the
// provider's protocol.
//
// This exists for the same reason WithRegistryService does, and is used by
// the test suite, which runs a real token endpoint rather than relaxing the
// provenance requirement into the shape of the tests.
//
// +cache="session"
func (a *App) WithOidcService(svc *dagger.Service) *App {
	a.IDTokenService = svc
	return a
}

// WithSessionSigstore points keyless signing at a certificate authority and
// a transparency log running inside this Dagger session, instead of at the
// public sigstore.
//
// It exists so the keyless path can be *executed* rather than only
// described: without it, the certificate request, the chain split, the log
// uploads — the image signatures' and the provenance envelope's — and the
// three annotations that carry them are reachable only by publishing a real
// release. The suite stands up a CA of its own and
// verifies the result with stock `cosign verify --certificate-identity`,
// which is the command Publish's doc comment tells consumers to run.
//
// # What it takes to redirect a real publish with this
//
// Deliberate work, which is the property being bought — not impossibility,
// which would be a stronger claim than the type supports. A *dagger.Service
// is a container the caller controls, and a container can proxy: a service
// running `socat` forwards a certificate request, workload identity token
// and all, straight out of the session. So this is not a boundary; it is a
// seam that cannot be crossed by accident. Three decisions make that so:
//
//   - It takes services, never URLs. There is no string argument here for a
//     typo, an inherited environment variable or a templated value to land
//     in, which is the whole class of accident a `--fulcio-url` flag would
//     have opened: one wrong character and a release is certified by
//     somebody else's CA. Redirecting this one takes a caller writing a
//     service and passing it, which is not a thing that happens to a release
//     pipeline unattended.
//   - Both are required, in one call. A publish is either wholly against a
//     session-hosted sigstore or wholly against the public one; there is no
//     state in which the certificate comes from one place and the log entry
//     from another, which is incoherent rather than merely unusual. A pair
//     that arrives half set is refused rather than quietly completed from
//     the public sigstore — see sigstoreEndpoints.
//   - It is never inferred. Not from WithRegistryService, not from
//     WithOidcService, not from WithInsecure — inferring it from the shape
//     of a test session is the relaxation daggerverse/CLAUDE.md names, and
//     it would leave the production path as the only unexercised one, which
//     is the situation this method exists to end.
//
// It also conflicts with WithSigningKey rather than being ignored beside it.
// A supplied key is never certified by anything, so a call setting both has
// asked for two different modes; Publish refuses instead of picking one
// silently.
//
// The name says "session" for the same reason WithInsecure says "insecure":
// this method's job is to be conspicuous in a file where it does not belong.
// A release pipeline signing against a sigstore that exists only for the
// length of one build is a thing a reader should stop at.
//
// What a session-hosted sigstore cannot establish is stated where it is
// asserted: a local log's countersignature is trusted by nobody, so a
// verifier still has to be told to ignore the log, and nothing here says
// anything about the public services' availability.
//
// +cache="session"
func (a *App) WithSessionSigstore(fulcio, rekor *dagger.Service) *App {
	a.FulcioService = fulcio
	a.RekorService = rekor
	return a
}

// Publish pushes this app to every repository named and returns one
// digest-pinned reference per published tag.
//
// Each repository is a *path* appended to the address given to
// WithRegistry: "z5labs/hello" against "ghcr.io" publishes
// ghcr.io/z5labs/hello. The registry stays a separate input because a
// mirror or an internal registry serves the same release, and the
// repository is stated here rather than derived from the binary because the
// two are not the same thing — a binary called `hello` is routinely
// published as `hello-service`.
//
// One manifest list is pushed per repository, naming every platform
// variant, so a consumer pulls a repository and gets their architecture.
// The returned references are `<address>/<repository>:<tag>@<digest>`:
// pinned, because a tag is a mutable name and a caller anchoring a
// deployment or a release note to what shipped has to be able to name
// immutable bytes.
//
// # One release, a family of tags
//
// A release is published under every tag its version implies, not under the
// version alone: `v1.2.3` also comes to name `v1.2`, `v1` and `latest`, so a
// consumer can pin at the level of risk they want. Every tag of one release
// names one digest — the same manifest list, pushed once — and one reference
// comes back per tag, in the order the tags were written.
//
// A SemVer **prerelease** publishes its own full version tag and moves none
// of the moving ones, and a version that is not SemVer publishes as a single
// tag. versionTags derives the family and is where those rules are stated;
// it is a pure function of the version, so what it cannot see — a release
// published out of order walking `v1` backwards — is recorded there too.
//
// # How to read what comes back
//
// The references are grouped **repository-major**: every tag of the first
// repository, in family order, then every tag of the second. The first
// reference of each group is the immutable one — the full version — so the
// reference a caller pins a deployment or a release note to is
// `refs[i*len(family)]`, and a caller who does not want to know the family's
// size can take the references whose tag is the version they passed.
//
// This was one reference per repository before the family existed, so a
// caller indexing `refs[i]` per repository reads a tag of the first
// repository now rather than the reference for `repositories[i]`. The
// grouping is stated here because there is nowhere better: a structured
// return — a repository, a digest and its tags — is the shape this wants,
// and it is a schema object that every consumer of this module would have to
// take a binding for, so it is worth doing deliberately rather than as part
// of this change.
//
// Every published digest carries an SPDX and a CycloneDX document per
// platform and a signed SLSA provenance statement whose build identity
// comes from an exchanged workload identity token. A publish that cannot
// produce provenance fails rather than publishing without it — see
// newSigner — and so does one that cannot produce a complete document for
// every platform, for the same reason and by the same rule.
//
// Those two documents describe the *image*: every byte in it, not only the
// executable a language chain built. They are assembled here, at publish
// time, out of one document per thing that entered the image, and they
// replace those rather than sitting beside them — a consumer fetches two
// documents per platform and nothing else. sbom.go records why the subject
// is the image, why assembly is not a scan, and what a contribution has to
// supply.
//
// Those documents are attached as OCI referrers of the digest and are
// discoverable that way alone, so `cosign verify-attestation` finds nothing
// even though they are there. The package doc says why and gives the
// `oras discover` commands that do find them; a consumer pointed at this
// method needs that section too.
//
// The image is signed too, and not merely the provenance statement about
// it: the manifest list and every per-platform manifest beneath it each
// carry a cosign signature, so a consumer runs
//
//	cosign verify <address>/<repository>:<version> \
//	  --certificate-identity-regexp '^https://github.com/<owner>/<repo>/\.github/workflows/' \
//	  --certificate-oidc-issuer https://token.actions.githubusercontent.com
//
// against the tag, and the same command against any per-platform digest
// their runtime resolved. Both pass, which is the point of signing every
// manifest rather than only the one the tag names — see signImage. A
// publish that cannot sign fails in the same way and for the same reason as
// one that cannot attest. WithSigningKey changes the verifying command; its
// doc comment says how.
//
// Within one repository the tag is the last thing written: the manifest list
// goes up under its own digest, the attestations are attached to that digest,
// and only then does the tag come to name it. So a publish that fails leaves
// no tag pointing at an unattested image — it leaves an unreferenced manifest,
// or, when the version was already published, the previous release still in
// place. attachAttestations records why that ordering was chosen and what the
// alternatives cost.
//
// Repositories are published in the order given, and the operation is not
// atomic: a failure part way through leaves the earlier repositories
// published, and says which ones in its error. A registry has no transaction
// spanning repositories, so the alternative to saying so is not atomicity —
// it is a caller who cannot tell what shipped.
//
// Publishing is a side effect against an external registry, so it is
// uncached: a re-run must actually push. The build above it is session
// cached, so the bytes pushed are the bytes Container returned.
//
// +cache="never"
func (a *App) Publish(ctx context.Context, repositories []string) ([]string, error) {
	if len(repositories) == 0 {
		return nil, fmt.Errorf("publish requires at least one repository to publish to")
	}
	for _, repository := range repositories {
		if err := validateRepository(repository); err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(a.Registry) == "" {
		return nil, fmt.Errorf("publish requires a registry: call withRegistry with the address, the username and the credential first")
	}
	// Both halves of the credential are required arguments of withRegistry,
	// so the schema rejects a missing one before this runs; what is reachable
	// is withRegistry never having been called, which the branch above
	// catches, and an empty string passed for the username, which nothing
	// else would. An empty username used to fall back to "ci" — inherited
	// from the old defaulted parameter — and publishing as some other
	// principal is the least useful answer to a caller who typed nothing:
	// what comes back is a 401 naming a user they never chose.
	if a.RegistryAuth == nil || strings.TrimSpace(a.RegistryUsername) == "" {
		return nil, fmt.Errorf("publish requires a credential: call withRegistry with the address, the username and the credential")
	}
	if len(a.Variants) == 0 {
		return nil, fmt.Errorf("this app carries no images to publish")
	}
	// The family is derived before the first byte moves, for the same reason
	// the signer is: a version that cannot be a tag has no family and no
	// single tag either, and learning that after the push is learning it from
	// an untagged manifest. It also re-runs the version validation App made,
	// which is what keeps the refusal of SemVer build metadata a property of
	// publishing rather than of one constructor.
	tags, err := versionTags(a.Version)
	if err != nil {
		return nil, err
	}
	// Provenance is resolved before the first byte is pushed, so a run that
	// cannot produce it fails without leaving a half-attested image behind.
	// It is also why this is not an "if configured" branch: an attestation
	// step that can be omitted is one that will be, and the image published
	// without it looks exactly like one published with.
	sgn, err := a.newSigner(ctx)
	if err != nil {
		return nil, err
	}
	// The contribution documents are read and parsed here for the same
	// reason, and it is not an optimization either. A publish that cannot
	// produce a complete document describing the image has to fail, not
	// warn — an incomplete SBOM is well-formed, attached, and
	// indistinguishable from a complete one, which is exactly the failure
	// devex#409 exists to close. Everything fallible about assembly is in
	// this call; what is left needs only the digest, which does not exist
	// until the push. See sbom.go.
	boms, err := a.resolveContributions(ctx)
	if err != nil {
		return nil, err
	}
	if err := a.assertImageConfiguration(ctx); err != nil {
		return nil, err
	}
	// A composed payload is proven complete by running it, and nothing about
	// its API shape can establish the same thing. It happens here, with
	// everything else that has to be true before the first byte moves, so a
	// payload that cannot start fails the publish instead of failing in front
	// of a consumer. compose.go carries what the check can and cannot tell
	// apart.
	if err := a.assertComposedPayloadsRun(ctx); err != nil {
		return nil, err
	}
	// Force the builds before the first byte moves. Everything above this
	// reads image *config*, which resolves without compiling anything, so
	// without this the first thing to solve the build graph is the push —
	// and a compile error would arrive wrapped in "publish <repo>:<version>",
	// blaming a registry that is fine. The cost is nothing: these are the
	// containers the push consumes, and the result is session-cached.
	for _, v := range a.Variants {
		if _, err := v.Container.Sync(ctx); err != nil {
			return nil, fmt.Errorf("build %s: %v", v.Platform, err)
		}
	}
	// The registry is the oci module's business, not this pipeline's. It
	// knows that Container.Publish cannot see session service bindings and
	// works around it in pure Go; this only knows which bytes to push and
	// what to call them.
	registry := dag.Oci().Registry(a.Registry, dagger.OciRegistryOpts{
		Username: a.RegistryUsername,
		Password: a.RegistryAuth,
		Service:  a.RegistryService,
		Insecure: a.Insecure,
	})
	containers := make([]*dagger.Container, 0, len(a.Variants))
	for _, v := range a.Variants {
		containers = append(containers, v.Container)
	}
	refs := make([]string, 0, len(repositories)*len(tags))
	for _, repository := range repositories {
		// Push, attach, then tag. The ordering is the whole subject of
		// attachAttestations' doc comment; read that before changing it.
		//
		// Every variant goes in one call, so a multi-platform build
		// publishes one manifest list naming them all rather than a tag per
		// architecture.
		digest, err := registry.PushImageUntagged(ctx, repository, containers)
		if err != nil {
			return nil, fmt.Errorf("publish %s:%s%s: %v", repository, a.Version, alreadyPublished(refs), err)
		}
		// The predicate names the whole family, and the attach happens before
		// any of it is written — so on a publish that fails part way through
		// the tag loop below, the statement claims moving tags that never came
		// to name this digest. That overclaim is not new: the attach has
		// always preceded the tag, for the reason attachAttestations records,
		// so the single-tag version made the same claim and merely made it
		// all-or-nothing.
		//
		// It stays the family rather than shrinking to the immutable tag,
		// because the field is what the release published under and a
		// statement naming only v1.2.3 would be *wrong* about every successful
		// publish rather than merely optimistic about a failed one. The
		// failure is not silent either: Publish returns an error naming the
		// tag it could not write, and the digest keeps every tag written
		// before it.
		facts := buildFacts{
			Repository: repository,
			Tags:       tags,
			Digest:     digest,
			Platforms:  a.platformNames(),
			Pkg:        a.Pkg,
			SourceURI:  a.SourceURI,
			Commit:     a.Commit,
			Version:    a.Version,
			Composed:   a.Composed,
		}
		if err := a.attachAttestations(ctx, registry, sgn, facts, boms); err != nil {
			// "no tag" rather than "nothing": the digest in this very message
			// resolves, and a caller who wants to look at what was left behind
			// should be able to. What the ordering buys is that no *name*
			// reaches it, so nobody arrives at it by pulling a release.
			return nil, fmt.Errorf("%v; %s was left untagged in %s, so no tag resolves to it%s",
				err, digest, repository, alreadyPublished(refs))
		}
		// The image itself is signed last of the fallible steps, for the
		// reason signImage records: it is the only one that writes a tag of
		// its own, so it gets the smallest window in which a later failure
		// can leave one behind.
		if err := a.signImage(ctx, registry, sgn, repository, digest); err != nil {
			return nil, fmt.Errorf("%v; %s was left untagged in %s, so no tag resolves to it%s",
				err, digest, repository, alreadyPublished(refs))
		}
		// Only now does the release become something a consumer can name. The
		// tags move last because they are the only step whose effect is
		// visible to anyone who did not push it.
		//
		// They are written narrowest first — the full version, then v1.2, then
		// v1, then latest — because a failure part way through leaves the tags
		// after it unmoved, and the wider a tag is the more consumers a
		// half-finished release would have handed the new bytes to. The
		// immutable one costs nothing to write first: it named no release
		// before this one.
		for _, tag := range tags {
			if _, err := registry.Tag(ctx, repository, digest, tag); err != nil {
				return nil, fmt.Errorf("tag %s as %s:%s%s: %v", digest, repository, tag, alreadyPublished(refs), err)
			}
			refs = append(refs, fmt.Sprintf("%s/%s:%s@%s", a.Registry, repository, tag, digest))
		}
	}
	return refs, nil
}

// alreadyPublished names what a partial publish left behind, for appending to
// the error that stopped it.
//
// Publishing several repositories is not atomic and cannot be: each is a
// separate push to a registry that has no notion of a transaction spanning
// them. So the failure has to say what already shipped. Without it a caller
// publishing to a public registry and an internal mirror in one call — the
// case Publish exists to serve — cannot tell "nothing shipped" from "the
// public one already has the release", and Publish is uncached, so their
// retry pushes everything again.
func alreadyPublished(refs []string) string {
	if len(refs) == 0 {
		return ""
	}
	return " (already published, and left in place: " + strings.Join(refs, ", ") + ")"
}

// newSigner resolves the identity this publish signs its provenance with,
// and refuses the publish when the machinery to do so was not supplied.
//
// Refusing rather than skipping is the decision this function exists to
// make. An unattested image is indistinguishable from an attested one until
// someone goes looking, so "provenance when configured" is provenance
// nobody can rely on — and the reason this pipeline exists at all is that a
// build step living outside the standard one drifts out of it. The error
// names the missing inputs and how to obtain them, because the failure a
// caller hits is almost always a missing permission rather than a missing
// argument.
//
// The missing inputs are named as a caller can supply them — the withOidc
// arguments — rather than as the environment variables they usually come
// from. Both appear, because the two failures look different from the two
// ends: someone reading this on GitHub Actions is looking for the
// permission, and someone driving the CLI is looking for the flag.
func (a *App) newSigner(ctx context.Context) (*signer, error) {
	var missing []string
	if strings.TrimSpace(a.IDTokenRequestURL) == "" {
		missing = append(missing, "withOidc --request-url")
	}
	if a.IDTokenRequestToken == nil {
		missing = append(missing, "withOidc --request-token")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf(
			"publishing requires provenance and provenance requires a workload identity token, but %s %s not set; "+
				"on GitHub Actions grant `permissions: id-token: write` and pass ACTIONS_ID_TOKEN_REQUEST_URL and "+
				"ACTIONS_ID_TOKEN_REQUEST_TOKEN to withOidc, or on any other CI the equivalent OIDC token request endpoint and its bearer token",
			strings.Join(missing, " and "), pluralIsAre(len(missing)))
	}
	// Both modes were asked for at once. Nothing certifies a supplied key,
	// so the sigstore that was stood up would never be asked for anything —
	// and the caller would get a signature with no certificate, no log entry
	// and no identity to verify against, which is the opposite of what
	// naming a CA says they wanted. Refusing names the contradiction where
	// picking one silently would leave them to find it in a consumer's
	// failed verification.
	if a.SigningKey != nil && (a.FulcioService != nil || a.RekorService != nil) {
		return nil, fmt.Errorf(
			"withSigningKey and withSessionSigstore were both called, but a supplied key is never certified by a CA: " +
				"drop withSigningKey to sign keyless against the sigstore you supplied, or drop withSessionSigstore to sign with your key")
	}
	sigstore, err := a.sigstoreEndpoints(ctx)
	if err != nil {
		return nil, err
	}
	return newSigner(ctx, a.IDTokenRequestURL, a.IDTokenRequestToken, a.SigningKey, a.IDTokenService, sigstore)
}

// sigstoreEndpoints is the CA and the log this publish uses: the public
// sigstore, or the one the caller stood up in this session.
//
// Neither service is started unless both are set, so the supplied-key mode —
// which reaches no CA and no log — starts nothing. That falls out of the
// first branch rather than needing a clause of its own: newSigner has
// already refused a signing key beside either service, so a supplied key
// implies both are nil.
//
// # A half-set pair is a refusal, not a fallback
//
// The tempting shape is `if either is nil, use the public sigstore`, and it
// is wrong in the one direction that matters. A caller who asked for a
// session-hosted CA and reached this with half a pair would silently send a
// live workload identity token to fulcio.sigstore.dev and mint a real,
// publicly logged certificate — the exact outcome WithSessionSigstore exists
// to make impossible — and nothing in the output would say so. Nothing can
// construct that state today, because the only setter sets both; it is
// refused anyway, because "unreachable" and "checked" are different and only
// one of them survives a second setter.
func (a *App) sigstoreEndpoints(ctx context.Context) (sigstoreEndpoints, error) {
	if a.FulcioService == nil && a.RekorService == nil {
		return defaultSigstore(), nil
	}
	if a.FulcioService == nil || a.RekorService == nil {
		missing := "certificate authority"
		if a.RekorService == nil {
			missing = "transparency log"
		}
		return sigstoreEndpoints{}, fmt.Errorf(
			"this publish was given a session-hosted sigstore with no %s; refusing to fall back to the public %s, "+
				"which would send this build's workload identity token to a certificate authority the caller did not ask for",
			missing, missing)
	}
	fulcio, err := serviceOrigin(ctx, a.FulcioService, "certificate authority")
	if err != nil {
		return sigstoreEndpoints{}, err
	}
	rekor, err := serviceOrigin(ctx, a.RekorService, "transparency log")
	if err != nil {
		return sigstoreEndpoints{}, err
	}
	return sigstoreEndpoints{fulcio: fulcio, rekor: rekor}, nil
}

// pluralIsAre keeps the refusal message grammatical whether one input is
// missing or both. A message that reads like a template is one people stop
// reading.
func pluralIsAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// expectedImageEnv is the environment every image this module publishes
// carries, in full.
//
// "In full" is the point. The standardized set is everything, because no
// caller-facing method adds to it: the chain declares nothing beyond this,
// and the last seam that could have — a WithEnvVariable on the image — is
// gone. That is what makes an image's environment assertable rather than
// merely documented.
//
// It is also what imageForEntry writes, rather than a second list beside it,
// so the set an image carries and the set a publish demands cannot drift
// apart.
//
// HOME and TMPDIR are here because the alternative is not "no value" but "the
// runtime's value": a process reads a home directory and a scratch directory
// out of the environment whether or not the image set them, and what it reads
// when the image sets neither varies by engine and by whether a caller
// overrode the user (devex#424). Pinning them makes the answer the image's,
// which is the same reason PATH is pinned. What the image carries *behind*
// them differs — a read-only HOME directory, and no /tmp at all — and the
// package doc's "The image contract" is where that is written down for
// adopters.
func expectedImageEnv() map[string]string {
	return map[string]string{
		"PATH":   appPath,
		"HOME":   appHomeDir,
		"TMPDIR": appTmpDir,
	}
}

// imageConfig is an image's OCI configuration, in full: every field of it
// this module has an opinion about, which is every field the config carries
// that a caller of this module could observe.
//
// It is a plain struct of what was *read* rather than a container, for the
// reason diffImageEnv's doc comment gives at length and which applies to
// every field here rather than only to the environment: the promise this
// module makes about most of them is "empty", and an empty field cannot be
// made non-empty through the public API, so a check driven only through that
// API can exercise the accepting side and nothing else. Split this way,
// ImageConfigSelfTest drives every branch in process and
// assertImageConfiguration is left with the part that genuinely needs a
// container.
type imageConfig struct {
	// Env is the image's environment, name to value.
	Env map[string]string
	// User is the OCI configuration's User field. Empty means the runtime
	// picks, which means uid 0 — which is why an image this pipeline
	// publishes never leaves it empty. See expectedImageConfig.
	User string
	// Entrypoint is what the image execs, argument by argument.
	Entrypoint []string
	// WorkingDir is the OCI configuration's WorkingDir field.
	WorkingDir string
	// DefaultArgs is the image's CMD: the arguments appended to Entrypoint
	// when a runtime is given none.
	DefaultArgs []string
	// ExposedPorts is every port the image declares, each rendered
	// "<port>/<protocol>" with the protocol lower-cased — the form the OCI
	// image config's own ExposedPorts keys take, and the one a Dockerfile's
	// EXPOSE line is written in — and sorted, so two reads of one image
	// compare equal. Dagger's NetworkProtocol is spelled "TCP"; rendering it
	// as it comes would leave an expectation written the way every other
	// document in the ecosystem writes it refused against a value that looks
	// identical.
	ExposedPorts []string
	// Labels is the image's labels, name to value. They are not the manifest
	// annotations imageForEntry writes: Dagger keeps the two apart, and a
	// container carrying six annotations reads back zero labels (measured,
	// Dagger v0.21.8).
	Labels map[string]string
}

// expectedImageConfig is the OCI configuration every image this module
// publishes carries, in full, for an image whose entrypoint is entrypoint.
//
// "In full" is the same claim expectedImageEnv makes about the environment,
// widened to the rest of the configuration and true for the same reason: no
// caller-facing method sets any of these fields, so what an image promises is
// a property of the pipeline rather than of the call that built it.
//
// Every field but the entrypoint and the user is empty, and that is the
// promise rather than an absence of one. An image with no working directory,
// no default arguments, no exposed ports and no labels is one whose behaviour
// is what its entrypoint does, and each of those would otherwise be inherited
// from a base layer the moment this module builds on one — which is the
// direction it is going. The package doc's "The image contract" is where the
// same set is written for adopters, and "# No working directory" is why that
// field in particular is a decision.
//
// User is appOwner, and it is the one field here whose expected value is a
// non-empty constant. An image config's User is what a runtime resolves when
// nothing overrides it, and an empty one resolves to uid 0 — so "no user" is
// not a neutral setting, it is root (devex#399). This line is what stops a
// later refactor dropping the WithUser in imageForEntry and shipping root
// again: nothing else would fail, because an image that runs as root works.
//
// The entrypoint is a parameter because it is the one part of the
// configuration that is a property of the application rather than of the
// module. It comes from the payload the constructor declared, which is the
// same value composition reads — never from the image, which is what is being
// checked.
func expectedImageConfig(entrypoint string) imageConfig {
	return imageConfig{
		Env:        expectedImageEnv(),
		User:       appOwner,
		Entrypoint: []string{entrypoint},
	}
}

// assertImageConfiguration refuses to publish an image whose configuration is
// not exactly what expectedImageConfig describes. It reads each variant's
// configuration and hands the comparison to diffImageConfig.
//
// It costs an image-config read per variant and no build: nothing here forces
// the rootfs to be solved, which is why it can run before the builds Publish
// forces further down.
func (a *App) assertImageConfiguration(ctx context.Context) error {
	if err := checkExpectedEntrypoint(a.Payload.Entry); err != nil {
		return fmt.Errorf("refusing to publish: %v", err)
	}
	want := expectedImageConfig(a.Payload.Entry)
	for _, v := range a.Variants {
		got, err := readImageConfig(ctx, v.Container)
		if err != nil {
			return fmt.Errorf("read the %s image's configuration: %v", v.Platform, err)
		}
		if err := diffImageConfig(got, want); err != nil {
			return fmt.Errorf("refusing to publish: the %s image %v", v.Platform, err)
		}
	}
	return nil
}

// checkExpectedEntrypoint reports why a declared entry cannot be the
// entrypoint of an image this pipeline publishes, or nil when it can.
//
// The entrypoint is the one field of the configuration whose expected value is
// not a constant — it is whatever the constructor declared the payload's entry
// to be — so comparing the image against it can only catch the image and the
// declaration disagreeing. It cannot catch the declaration itself being wrong,
// and "an absolute path, /app/<binary>" is a promise the package doc makes to
// everyone writing a `COPY --from=` line. This is where that half is checked,
// against the constant, before the declaration is used as an expectation.
//
// It is a free function over a string for the reason everything else in this
// check is a free function over what was read: no constructor in this module
// produces an entry outside /app, so driving these branches through the public
// API is impossible and they would be deletable with every test still green.
func checkExpectedEntrypoint(entry string) error {
	if strings.TrimSpace(entry) == "" {
		return fmt.Errorf("this application declares no entry to exec, so there is nothing to hold its images' entrypoint to")
	}
	name, ok := strings.CutPrefix(entry, appDir+"/")
	if !ok || name == "" || strings.Contains(name, "/") {
		return fmt.Errorf(
			"this application's entry is %q, and every image this pipeline publishes execs a binary directly in %s",
			entry, appDir)
	}
	return nil
}

// readImageConfig reads ctr's OCI configuration into the struct the
// comparison runs over.
//
// Every read names the field it was reading. A publish that cannot read an
// image's configuration is a rare failure and an opaque one — the caller has
// seven API calls and one message — so the field is worth the words.
func readImageConfig(ctx context.Context, ctr *dagger.Container) (imageConfig, error) {
	env, err := containerEnv(ctx, ctr)
	if err != nil {
		return imageConfig{}, fmt.Errorf("environment: %v", err)
	}
	user, err := ctr.User(ctx)
	if err != nil {
		return imageConfig{}, fmt.Errorf("user: %v", err)
	}
	entrypoint, err := ctr.Entrypoint(ctx)
	if err != nil {
		return imageConfig{}, fmt.Errorf("entrypoint: %v", err)
	}
	workdir, err := ctr.Workdir(ctx)
	if err != nil {
		return imageConfig{}, fmt.Errorf("working directory: %v", err)
	}
	args, err := ctr.DefaultArgs(ctx)
	if err != nil {
		return imageConfig{}, fmt.Errorf("default arguments: %v", err)
	}
	ports, err := ctr.ExposedPorts(ctx)
	if err != nil {
		return imageConfig{}, fmt.Errorf("exposed ports: %v", err)
	}
	exposed := make([]string, 0, len(ports))
	for i := range ports {
		number, err := ports[i].Port(ctx)
		if err != nil {
			return imageConfig{}, fmt.Errorf("exposed ports: %v", err)
		}
		protocol, err := ports[i].Protocol(ctx)
		if err != nil {
			return imageConfig{}, fmt.Errorf("exposed ports: %v", err)
		}
		exposed = append(exposed, fmt.Sprintf("%d/%s", number, strings.ToLower(string(protocol))))
	}
	sort.Strings(exposed)
	labels, err := ctr.Labels(ctx)
	if err != nil {
		return imageConfig{}, fmt.Errorf("labels: %v", err)
	}
	named := make(map[string]string, len(labels))
	for i := range labels {
		name, err := labels[i].Name(ctx)
		if err != nil {
			return imageConfig{}, fmt.Errorf("labels: %v", err)
		}
		value, err := labels[i].Value(ctx)
		if err != nil {
			return imageConfig{}, fmt.Errorf("labels: %v", err)
		}
		named[name] = value
	}
	return imageConfig{
		Env:          env,
		User:         user,
		Entrypoint:   entrypoint,
		WorkingDir:   workdir,
		DefaultArgs:  args,
		ExposedPorts: exposed,
		Labels:       named,
	}, nil
}

// diffImageConfig reports how an image's configuration differs from what this
// pipeline publishes, and reports nothing when it does not differ.
//
// One difference is reported rather than all of them, and it names the
// property and the value found — "sets its working directory to "/app"" and
// not "the configuration is wrong". A refusal arrives at a release pipeline,
// hours from anyone who can read the image, so it has to carry what the reader
// would otherwise have to go and look up.
//
// The environment goes through diffImageEnv rather than being folded in here,
// because its messages are the ones an operator has already seen and because
// "exactly this set, in both directions" is a rule the scalar fields do not
// have. Everything else is a comparison against a stated value, empty
// included: an empty expectation is checked exactly as hard as a non-empty
// one, which is the half that would otherwise rot the moment a base layer
// starts contributing a WORKDIR or a CMD nobody here wrote.
func diffImageConfig(got, want imageConfig) error {
	if err := diffImageEnv(got.Env, want.Env); err != nil {
		return err
	}
	if got.User != want.User {
		return fmt.Errorf("sets its user to %s, but every image this pipeline publishes sets its user to %s",
			describeImageValue(got.User), describeImageValue(want.User))
	}
	if !slices.Equal(got.Entrypoint, want.Entrypoint) {
		return fmt.Errorf("sets its entrypoint to %s, but every image this pipeline publishes execs the entry its payload declares, %s",
			describeImageList(got.Entrypoint), describeImageList(want.Entrypoint))
	}
	if got.WorkingDir != want.WorkingDir {
		return fmt.Errorf("sets its working directory to %s, but every image this pipeline publishes sets it to %s",
			describeImageValue(got.WorkingDir), describeImageValue(want.WorkingDir))
	}
	if !slices.Equal(got.DefaultArgs, want.DefaultArgs) {
		return fmt.Errorf("sets its default arguments to %s, but every image this pipeline publishes sets them to %s",
			describeImageList(got.DefaultArgs), describeImageList(want.DefaultArgs))
	}
	if !slices.Equal(got.ExposedPorts, want.ExposedPorts) {
		return fmt.Errorf("exposes %s, but every image this pipeline publishes exposes %s",
			describeImageList(got.ExposedPorts), describeImageList(want.ExposedPorts))
	}
	return diffImageLabels(got.Labels, want.Labels)
}

// diffImageLabels reports how an image's labels differ from the set this
// pipeline publishes.
//
// Equality in both directions, the same rule and for the same reason as
// diffImageEnv: a label nobody here wrote is the visible half of a base layer
// this module has not decided what to do with yet, and a published image is
// something other people read `org.opencontainers.image.*` out of.
func diffImageLabels(got, want map[string]string) error {
	for _, name := range sortedKeys(got) {
		expected, ok := want[name]
		if !ok {
			return fmt.Errorf("carries a label this pipeline never sets, %s=%q; %s",
				name, got[name], describeExpectedLabels(want))
		}
		if got[name] != expected {
			return fmt.Errorf("sets the label %s=%q, but every image this pipeline publishes carries %s=%q",
				name, got[name], name, expected)
		}
	}
	for _, name := range sortedKeys(want) {
		if _, ok := got[name]; !ok {
			return fmt.Errorf("carries no %s label", name)
		}
	}
	return nil
}

// describeExpectedLabels says what the label set is supposed to be, in a form
// that reads as a sentence whether it is empty or not. "a published image's
// labels are exactly " with nothing after it is the message this avoids.
func describeExpectedLabels(want map[string]string) string {
	if len(want) == 0 {
		return "a published image carries no labels"
	}
	return "a published image's labels are exactly " + strings.Join(sortedKeys(want), ", ")
}

// describeImageValue renders one configuration value for a refusal, naming
// emptiness rather than printing an empty pair of quotes. A message reading
// `want ""` leaves its reader unable to tell a promise of "empty" from a
// value the check failed to read.
func describeImageValue(v string) string {
	if v == "" {
		return "nothing"
	}
	return strconv.Quote(v)
}

// describeImageList is describeImageValue for the fields that are lists.
func describeImageList(v []string) string {
	if len(v) == 0 {
		return "nothing"
	}
	return fmt.Sprintf("%q", v)
}

// diffImageEnv reports how an image's environment differs from the
// standardized set, and reports nothing when it does not differ.
//
// It fails on an *unexpected* variable and not only on a missing one, which
// is the half that is easy to leave out and is the half that matters: a
// missing PATH breaks the plugin contract loudly, while a stray variable —
// a credential leaked in from a build step, a debug flag — ships silently
// inside something people pull. Checking equality rather than containment
// is what turns the doc comment on appPath into a guarantee.
//
// It is a free function over two maps rather than a method reading
// containers, and that is what makes the guarantee testable. Nothing
// caller-facing can put a variable on an image today, so driving the
// unexpected-variable branch through the public API is impossible and the
// strongest half of the check would be unexecutable — deletable tomorrow
// with every test still green. Split this way, ImageConfigSelfTest
// drives every branch in process, and readImageConfig is left with
// the part that genuinely needs a container: reading the environment.
//
// That argument is not the environment's alone, which is why diffImageConfig
// is shaped the same way around the rest of the image's configuration
// (devex#426).
//
// The branch stops being hypothetical as soon as an image has a base layer
// to inherit from, which is the direction this module is going.
func diffImageEnv(got, want map[string]string) error {
	for _, name := range sortedKeys(got) {
		expected, ok := want[name]
		if !ok {
			return fmt.Errorf(
				"carries an environment variable this pipeline never sets, %s; a published image's environment is exactly %s",
				name, strings.Join(sortedKeys(want), ", "))
		}
		if got[name] != expected {
			return fmt.Errorf("sets %s=%q, but every image this pipeline publishes carries %s=%q", name, got[name], name, expected)
		}
	}
	for _, name := range sortedKeys(want) {
		if _, ok := got[name]; !ok {
			return fmt.Errorf("carries no %s", name)
		}
	}
	return nil
}

// sortedKeys returns m's keys in a stable order, so two runs that fail for
// the same reason fail with the same message.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// platformNames is the platforms this app was built for, as strings, for
// the messages and the provenance predicate.
func (a *App) platformNames() []string {
	out := make([]string, 0, len(a.Variants))
	for _, v := range a.Variants {
		out = append(out, string(v.Platform))
	}
	return out
}
