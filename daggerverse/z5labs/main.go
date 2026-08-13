// Package main implements the z5labs daggerverse module: opinionated CI
// and release pipelines for Go projects.
//
// There are two entry points. Z5labs.Go is the Go language chain: it carries
// the standardized checks over a source tree and its terminal Ci runs them,
// while its other terminal, App, cross-compiles the application, packages one
// image per platform and hands back an App whose Publish pushes them. A
// library is a source tree you never call App on, which is why there is no
// library archetype.
//
// Z5labs.App is the other, and it is the general one: an App assembled from
// executables this module did not build. See "Prebuilt executables, and
// languages with no chain" below. Z5labs.Go is built on it rather than beside
// it, so there is one image build, one variant-set validation and one publish.
//
// # The image contract
//
// Every image this module builds carries the same environment, and it is
// exactly three variables:
//
//	PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
//	HOME=/home/nonroot
//	TMPDIR=/tmp
//
// with /usr/local/bin as the directory an extension's executables land in.
// Every value is fixed by the module and no caller-facing method can move any
// of them, because a published image is something other people write `FROM`
// and `COPY` lines against: a PATH that varied per app would make "put your
// plugin on the PATH" a per-image question, and moving the directory later
// would break every line already written. The PATH value is the conventional
// default a container runtime injects when an image sets none, so an image
// that later gains a real base layer behaves the way its base expects.
//
// Exactly one seam puts an executable in that directory, and it is App.WithApp
// — see "Composing one application into another" below. Contributing at, under
// or over it is refused, and so is contributing onto any of the other five
// directories that PATH names: a contributed file or tree states no
// architecture and lands in every variant, so content something finds on the
// PATH by name is a way to leave an arm64 image running an amd64 executable.
// "An extension's executables land here" therefore means composition, and
// nothing else (devex#427).
//
// The application's own entrypoint does not rely on any of that. It is an
// absolute path — /app/<binary> — so the app runs whatever the PATH says.
// PATH exists for what an extension adds, not for finding the app itself.
// /app itself is root-owned and 0755: the directory the entrypoint sits in is
// what decides whether the binary can be unlinked and replaced, and one the
// application could write is one whose published digest stops describing what
// is running.
//
// The rest of the OCI configuration is part of the contract too. One field
// carries a value and the rest are empty:
//
//	User          65532:65532 — see "# The image runs as 65532:65532" below
//	WorkingDir    nothing — see "# No working directory" below
//	Cmd           no default arguments
//	ExposedPorts  none
//	Labels        none — the per-platform OCI *annotations* are a separate field
//
// Empty is a promise rather than an omission, and so is the one value: it is
// all asserted the same way, by a publish that reads back the whole
// configuration of every variant and refuses one that does not match, in both
// directions. Each of the empty fields would otherwise be inherited, silently,
// from the first base layer this module builds on, and the User is the field
// where "not set" is not neutral (devex#426, devex#399).
//
// # The image runs as 65532:65532
//
// Every image this module publishes sets its User to 65532:65532 — a uid and a
// gid, written as numbers. It is the same identity every byte in the image is
// owned by, which is one decision rather than two: a uid that owns nothing it
// runs, or files owned by a uid nothing runs as, is a pair somebody has to keep
// in agreement by hand.
//
// Numbers rather than a name, because a scratch image has no /etc/passwd for a
// name to resolve against — a User of "nonroot" is a string nothing in the
// image can turn into a uid — and because the two places the number is read
// again are a long way from here: a `COPY --chown=65532:65532` in a derived
// Dockerfile, and runAsUser in a Kubernetes securityContext. Neither of those
// can ask the image what its user is; both are written against a number
// somebody wrote down. 65532 specifically because that is distroless's
// `nonroot`, and because z5labs/avroc and Zaba505/cpybkc already pin it in
// their hand-rolled image builds — moving onto this archetype should not also
// be a change to a contract they have published.
//
// It is not overridable at build time, and no application gets root by asking.
// There is no WithUser on App and there will not be one, for the reason there
// is no way to turn provenance off: a hardening default that can be switched
// off is hardening nobody downstream can rely on, and "runs as non-root" is
// precisely the kind of claim an admission policy is written against. An
// application that genuinely needs uid 0 is out of scope for this archetype
// rather than a case it configures for.
//
// A *deployment* may still pick a different uid, and that is an ordinary
// configuration rather than a workaround. Every file this module writes is
// world-readable, every directory world-traversable, and the entrypoint is
// 0555, so `docker run --user $(id -u):$(id -g)`, or a securityContext naming a
// uid a cluster allocated, runs the same image the same way.
//
// What an override to any other *non-root* uid does not buy is a writable
// image: /app is root-owned 0755, HOME is root-owned 0555, and contributed
// content is read-only. An override back to uid 0 does buy one, because root
// bypasses the permission check — that is the same exception HOME's paragraph
// below names, and it is now something a deployment has to ask for rather than
// something it gets by default.
//
// # Running as 65532 is behaviour-affecting
//
// This changed images that already existed, and it changed them in the
// direction that fails rather than the direction that warns. Before it, an
// application ran as uid 0, which bypasses the permission check on every mode
// in the image — so it could write anywhere at all, the read-only paths this
// module deliberately builds included. Under 65532 those same writes fail with
// EACCES, reported by the application as "permission denied" or whatever its
// own error handling makes of one.
//
// That message cannot be improved from here. The write is the application's own
// syscall, and nothing in this module is in the process to intercept it or to
// wrap what it returns. So the explanation lives where somebody debugging one
// can reach it: this section, arrived at from the running image itself, whose
// org.opencontainers.image.source annotation names this repository — that is
// the path from a container that stopped working back to the decision, and it
// is why that annotation is on every variant rather than only on the index.
//
// Three things worth knowing on arrival, in the order they are likely to be
// what happened:
//
//   - A write to the image's own paths now fails, and this is the most likely
//     breakage. /app and HOME are root-owned and read-only by design (see
//     below), which stopped nothing while the application was root and stops it
//     now. An application writing beside its own binary or into its home
//     directory was relying on being uid 0, whether or not anyone knew it.
//   - A write anywhere else lands in the container's writable layer, and uid
//     65532 cannot create an entry at its root. Mount storage at the path the
//     application writes — a tmpfs, an emptyDir, a volume — and give it to
//     65532:65532. That is the same thing TMPDIR's paragraph below already asks
//     a deployment to do for scratch space, applied to whatever other path the
//     application chose.
//   - If the application picked its path from the environment, HOME and TMPDIR
//     are the two this image pins, and both are covered below.
//
// What is deliberately *not* offered is a way to turn this off; see the section
// above. A deployment that must have the old behaviour can override the user
// back to uid 0, which is a thing it has to write down in a manifest somebody
// reviews, rather than a default nobody sees.
//
// # HOME is a directory; TMPDIR is a mount point
//
// HOME and TMPDIR are on that list for a reason PATH's paragraph does not
// cover, and it is not that an application here needs either of them. The
// alternative to setting them is not "no value" — it is the *runtime's* value.
// A process reads a home directory and a scratch directory out of its
// environment whether or not the image set them, and what it reads when the
// image sets neither is the engine's choice: measured under podman 5.8.4 on an
// image with exactly this layout, an unset HOME leaves os.UserHomeDir failing
// with "$HOME is not defined" under uid 0 and returning "/" — a root-owned
// directory nothing can write — under a uid override, from one digest.
// Pinning them makes the answer a property of the image, which is what the
// rest of this contract is for (devex#424).
//
// What the image carries behind the two values is deliberately different.
//
// HOME names a directory the image really has: /home/nonroot, owned by root,
// mode 0555, and shipped empty. A read under it fails ENOENT and a write fails
// EACCES, the same way on every runtime, instead of succeeding into a writable
// layer under one and failing under another. /home/nonroot is the conventional
// home of uid 65532, which is who these images run as and who their files
// belong to, so it is also the path a deployment mounts a volume at in the case
// where an application genuinely needs a writable home. Nothing in the image
// needs one: an application that writes to its home directory is making its own
// state part of a digest that is supposed to describe what is running.
//
// The one user that mode does not stop is root, which bypasses the permission
// check — so "a write fails" is a promise under every user except uid 0. No
// image published here runs as uid 0 (see "# The image runs as 65532:65532"),
// so that exception is now reachable only by a deployment that deliberately
// overrides the user back to root. Owning the directory as root rather than as
// the application's user is what makes the promise hold for every other choice
// a deployment can make, rather than only for the default one.
//
// A caller may contribute read-only content under it — a default
// configuration an operator can override by mounting one — and that is
// contributed content like any other, landing owned by the application's user
// and read-only. "Empty" is what the image ships, not a rule about what may go
// there; see contribute.go.
//
// TMPDIR names /tmp, and the image does not contain /tmp. An application that
// needs scratch space therefore fails — os.CreateTemp with "no such file or
// directory" — until the deployment mounts something there, a tmpfs or an
// emptyDir. That is a runtime failure an adopter cannot discover by looking at
// the image, which is why it is written here rather than left to be found. The
// image ships no /tmp because a directory baked into one has no size to set:
// it is the container's writable layer, unbounded, and gone the moment anyone
// turns on readOnlyRootFilesystem. Only whoever runs the image can size
// scratch space, so only they can supply it — naming the path is the
// archetype's job and supplying the storage behind it is the deployment's,
// which is this file's own division of labour rather than an exception to it.
// Contributing content under /tmp is refused for the other half of the same
// reason: the mount the deployment makes would shadow it.
//
// # No working directory
//
// Nothing sets one, and this is the decision rather than an omission. It is
// worth stating beside HOME because a working directory is the natural first
// guess at the problem HOME solves, and it does not solve it: os.UserHomeDir
// reads $HOME and never falls back to the working directory, so a WORKDIR
// would change only what relative paths resolve against. The two are
// orthogonal.
//
// Leaving it unset is also load bearing elsewhere. validateContributionPath
// refuses a relative contribution path on the grounds that it "would resolve
// against a working directory this pipeline never sets" — see contribute.go —
// and that reasoning holds only while nothing sets one. Setting a working
// directory later means deciding what happens to that refusal, not merely
// adding a line to the image config.
//
// # Environment is a runtime concern
//
// No method on App sets the image's environment, and none will. There is no
// WithEnvVariable here, and its absence is a decision rather than an omission:
// every category of variable has an owner other than the caller of a build.
// A language chain owns the runtime search paths it created the layout for —
// CLASSPATH, PYTHONPATH. This archetype owns PATH, HOME and TMPDIR, because
// they are the contract other people write FROM and COPY lines against, and it
// sets all three on every image it builds — the section above is what they are
// and why each one is pinned rather than left to the runtime. The
// source owns its own baked defaults, GOMEMLIMIT among them, where a constant
// in the program says it better than a variable outside it. A CA bundle
// contributed where the library already looks needs no variable at all. And
// the deployment — the Kubernetes manifest, the compose file, the systemd unit
// — owns everything left, which is most of it: an environment is how one image
// is run in two places, so baking one in is answering a question at build time
// that is asked at run time.
//
// The half of this that is not an opinion: because nothing caller-facing can
// set a variable, an image's environment is assertable *in full* rather than
// merely documented, and a publish asserts it — see App.Publish. The same
// argument, and the same check, covers the rest of the image's configuration.
// A caller seam would make that check unverifiable in principle, because
// "exactly this set" stops being a property of the pipeline the moment anyone
// can add to it.
//
// The check is a publish-time gate and App.Container deliberately does not run
// it: an inspection seam has to hand back the image that is wrong as readily as
// the one that is right. It gates this module's publish path rather than the
// bytes — a caller who takes the container away and pushes it themselves goes
// round this check exactly as they go round the annotations, the SBOMs, the
// provenance and the signature — which is the sense in which a z5labs image is
// one that App.Publish published.
//
// The one case that genuinely cannot move is a variable whose value is a fact
// about the image's own filesystem layout, for a program whose source you do
// not control. That belongs to whoever assembled the payload it points at —
// the caller of Z5labs.App, who chose where the payload landed — and not to a
// helper here.
//
// Contributing files and directories to an image is a different question and
// has a different answer: App.WithFile and App.WithDirectory, in contribute.go.
// Putting another *application's* executable in one is a third: App.WithApp,
// in compose.go, which is what fills the plugin directory named above.
//
// # Prebuilt executables, and languages with no chain
//
// Z5labs.App builds an application out of executables this module did not
// compile. It is the seam for three things the Go chain cannot reach: a
// prebuilt or vendor executable, a language whose chain nobody has written
// yet, and a payload shape — an entry plus the files it needs to run — that no
// single Go binary produces.
//
//	dagger call app --version=v1.2.3 \
//	  with-variant --platform=linux/amd64 --entry=./dist/app-amd64 \
//	    --document=./dist/app-amd64.spdx.json --name=app \
//	  with-variant --platform=linux/arm64 --entry=./dist/app-arm64 \
//	    --document=./dist/app-arm64.spdx.json --name=app \
//	  build \
//	  with-registry --address=ghcr.io --username=... --auth=env:TOKEN \
//	  publish --repositories=owner/app
//
// The `--name` is doing real work there and is the flag most likely to be left
// out. A release pipeline names its cross-compiled artifacts per platform, and
// the entry has to land at one path in every variant or the entrypoint differs
// per architecture — so a set contributed with per-platform file names and no
// `--name` is refused rather than resolved by picking one. Leave it out only
// when the file is already called what the application should be called.
//
// What comes back from Build is the same App a language chain produces, so
// everything above and below this section applies to it unchanged: the same
// PATH and executable directory, the same non-root ownership and read-only
// modes, the same per-platform annotations, the same assembled SBOMs and the
// same signed provenance. Nothing about supplying the executable yourself is
// an exemption — the module still builds the image around it.
//
// Three properties of it are worth stating rather than leaving to be found.
//
// **Every variant names its platform, and nothing is inferred.** A
// *dagger.File carries no architecture, so there is deliberately no helper
// that takes an executable and works out where it belongs: one that did would
// admit an index whose arm64 manifest holds an amd64 binary, which fails at
// exec time with the kernel's message and for nobody here. A single-platform
// application is expressible and is not a degenerate case.
//
// **Its documents are asserted rather than derived.** A Go binary's SBOM comes
// out of the compiled artifact and cannot disagree with it; a document handed
// to WithVariant is a claim its supplier made. What keeps the claim honest is
// that it has to name the SHA-256 of the content it accompanies and a publish
// checks it, so a document about other bytes fails the publish rather than
// shipping. What that does not establish is whether the components listed
// inside it are really in the artifact — that remains the supplier's
// assertion, in a signed artifact, checkable by anyone who pulls the image.
// The same check now applies to WithFile and WithDirectory.
//
// **It records no build identity.** There is no commit, origin or build-time
// argument, because a build identity a caller could have supplied identifies
// nothing — the same rule that keeps a repository parameter off the
// provenance. So an application assembled this way carries the version
// annotation and no revision, source or created annotation, and its provenance
// records the publish and the image and no source tree. A language chain
// records those because it observed them.
//
// # Composing one application into another
//
// App.WithApp puts another application's payload into this one's image. It is
// what fills the plugin directory the image contract has been promising, and
// it is the shape a CLI with a generator per language actually ships as:
//
//	ghcr.io/z5labs/avroc              the base: the CLI, and nothing else
//	ghcr.io/z5labs/avroc-gen-go       the base plus /usr/local/bin/avroc-gen-go
//	ghcr.io/z5labs/avroc-gen-java     the base plus /usr/local/bin/avroc-gen-java
//
// Each derived image is published to a repository of its own — a repository is
// stated to Publish and is nothing to do with any binary's name — and every
// one of them is annotated, signed and attested exactly like the base.
//
//	base := dag.Z5Labs().Go(src).App("v1.2.3")
//	derived := base.WithApp(generator)
//
// The derived image is the same program as the base wearing one more
// executable, and everything about that sentence is enforced. It inherits the
// base's entrypoint, user, environment and executable directory rather than
// restating any of them, and it is published under the base's version, because
// the release it belongs to is the base's release — which is why the composed
// application's own version is recorded in the provenance predicate, as the
// only thing anywhere that says which release of the plugin shipped.
//
// What crosses is a *payload*: the paths that make an application runnable,
// plus which one is the entry, declared by whatever constructed it. The entry
// lands in the plugin directory under its own file name, so a base whose CLI
// discovers extensions on the PATH finds it. Everything else the composed
// application carries — its own contributed files and directories — lands at
// its own path, because a complete application is what may be composed rather
// than a slim carrier that only makes sense once merged.
//
// Composition never reads the entrypoint to decide what crosses, and that is
// the single most important thing about it: `ep[0]` is a PATH lookup rather
// than a path for an interpreted application, later elements can be paths that
// would be dropped, an application driven by Cmd has no `ep[0]` at all, and a
// CGO_ENABLED=1 binary needs a loader that lives in the image it came out of.
// Every one of those publishes cleanly, attests cleanly and dies on first
// exec. compose.go carries the whole rule.
//
// Four things are refused rather than resolved, each because the alternative
// is silent: a platform set that does not match exactly in both directions, a
// path collision with the base or with an application composed earlier, an
// environment variable the two sides disagree about, and — for now — a
// declared payload of more than one file. And because no API shape can
// establish that a payload is complete, Publish executes the entry of every
// composed payload in the derived image before the first byte is pushed: a
// payload that cannot run under the base's environment with nothing added
// fails the build rather than the consumer.
//
// An image built out of pipeline with a `FROM <published-ref>` line gets none
// of this, and the reason is precise rather than vague: a stranger's
// Dockerfile adds bytes with no document and has no mechanism to produce one,
// so that image's attestations describe the base and nothing else.
//
// # Versions
//
// The version is the caller's, passed to App and validated there: it has to
// be an OCI-tag-safe string, and SemVer build metadata is refused rather
// than mangled. See GoChain.App.
//
// # Verifying a published image, and finding what is attached to it
//
// A publish leaves more on the registry than the image: a cosign signature
// over every manifest beneath the tag, an SPDX and a CycloneDX SBOM per
// platform, and one signed SLSA provenance statement. The signature and the
// documents are discovered two different ways, and which way is not
// something an adopter can guess by looking at the image.
//
// The signature is written in cosign's own layout — a `sha256-<hex>.sig`
// tag beside each digest it signs — so checking it is a command a consumer
// already has:
//
//	cosign verify ghcr.io/<owner>/<app>:<version> \
//	  --certificate-identity-regexp '^https://github.com/<owner>/<repo>/\.github/workflows/' \
//	  --certificate-oidc-issuer https://token.actions.githubusercontent.com
//
// The documents are not. They are attached as OCI referrers of the
// published digest, and by referrers alone: this module writes no
// `sha256-<hex>.att` tag, so cosign's matching command finds nothing.
// Against an image published here,
//
//	cosign verify-attestation ghcr.io/<owner>/<app>:<version> \
//	  --type slsaprovenance1 \
//	  --certificate-identity-regexp '^https://github.com/<owner>/<repo>/\.github/workflows/' \
//	  --certificate-oidc-issuer https://token.actions.githubusercontent.com
//
// exits 1 with
//
//	Error: no matching attestations:
//
// and that message is why this section exists. It reads as "this image has
// no attestations" and it means "cosign looked under the tag convention and
// they are not there" — a distinction nothing in the output draws, so an
// adopter who runs the natural first command and stops concludes the
// publish attached nothing.
//
// # Listing the attached documents
//
// Finding them takes a client that falls back to the referrers tag scheme,
// which is a narrower thing than a client that speaks the referrers API.
// GHCR implements no referrers API and answers 404 there, so a tool that
// asks and stops — cosign, crane, a hand-written GET on
// /v2/<name>/referrers/<digest> — reports nothing attached, which is the
// same wrong conclusion by a second route. oras-go takes the fallback
// itself, so anything built on it needs no flag for it. Measured with oras
// v1.2.3 against ghcr.io on 2026-08-13, as is every command below.
//
//	oras discover ghcr.io/<owner>/<app>:<version>
//
// lists everything attached to the release. Against a private package,
// authenticate first — `oras login ghcr.io -u <user> --password-stdin` with
// a token carrying read:packages — because an unauthenticated discover
// fails on authorization rather than returning an empty list, which is one
// more failure that reads like "nothing is attached".
//
// # Reading one document, anchored to a verified digest
//
// Take the digest from the signature check rather than resolving the tag a
// second time. The tag is a mutable name, and the whole value of doing both
// checks is that they are about the same bytes; cosign prints the digest it
// verified in its JSON output.
//
//	repo=ghcr.io/<owner>/<app>
//	digest=$(cosign verify "$repo:<version>" \
//	  --certificate-identity-regexp '^https://github.com/<owner>/<repo>/\.github/workflows/' \
//	  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
//	  --output json | jq -r '.[0].critical.image["docker-manifest-digest"]')
//
//	referrer=$(oras discover "$repo@$digest" \
//	  --artifact-type application/vnd.in-toto+json \
//	  --format json | jq -r '.manifests[0].digest')
//	layer=$(oras manifest fetch "$repo@$referrer" | jq -r '.layers[0].digest')
//	oras blob fetch "$repo@$layer" --output - > provenance.intoto.jsonl
//
// A document is selected by its artifact type, which is what attaching them
// under distinct types is for: application/spdx+json and
// application/vnd.cyclonedx+json for the SBOMs, application/vnd.in-toto+json
// for the provenance. Each referrer manifest holds exactly one layer, and
// that layer is the document. On oras v1.2.3 `--format json` keys the list
// `manifests`; if these pipelines start yielding an empty `$referrer` under
// a newer client, that key is the first thing to re-check, because a
// renamed key fails as a null rather than as an error.
//
// The `[0]` above is correct only because there is one provenance statement
// per publish. Each SBOM type has one referrer per platform, so selecting an
// SBOM means picking on the layer's org.opencontainers.image.title
// annotation — `<app>-linux-amd64.spdx.json` and its CycloneDX and
// per-platform counterparts, where `<app>` is the last segment of the
// repository — which is `.layers[0].annotations` on each referrer's own
// manifest, not something `oras discover` prints. Taking `[0]` for an SBOM
// silently picks an arbitrary platform's.
//
// # What the SBOMs describe, and how many of them there are
//
// Two per platform, and they describe **the image**: every byte in it, not
// only the executable a language chain built. Their subject is the published
// digest, so a consumer who pulled `<repo>@<digest>` can check the document
// is about the bytes they have rather than taking it on trust.
//
// There are no other SBOMs on the digest and there is deliberately nothing
// per-file or per-contribution to go looking for. Each thing that enters an
// image arrives with its own SPDX document — that is how the image-level
// pair can exist at all without a scanner — but those are *inputs* to the
// assembly and are not published. One document of a given artifact type per
// platform is what makes selecting one unambiguous; several would leave a
// consumer picking, silently, between a complete document and a partial one.
//
// Read them the same way as the provenance above, with
// `--artifact-type application/spdx+json` or
// `--artifact-type application/vnd.cyclonedx+json`, then pick the platform
// off the title annotation. Inside, the image is the package the document
// DESCRIBES; each thing that was contributed to it is a package the image
// CONTAINS; and what each of those was built from hangs off it by
// DEPENDS_ON. CycloneDX says the same thing with the image as
// `metadata.component` and the same graph under `dependencies`.
//
// A publish that cannot assemble a complete document for every platform
// fails, and fails before pushing, exactly as one that cannot produce
// provenance does. An SBOM that accounts for some of an image is well
// formed, attaches cleanly and is indistinguishable from a complete one, so
// "attach what we have and warn" is not a behaviour this pipeline offers.
//
// # What can be checked about the envelope, and what cannot
//
// The provenance is a DSSE envelope. Its statement, the identity that signed
// it, and the transparency log entry that says when it was signed are all
// readable from those bytes alone:
//
//	jq -r .payload provenance.intoto.jsonl | base64 -d | jq .
//	jq -r '.signatures[0].cert' provenance.intoto.jsonl |
//	  openssl x509 -noout -text | grep -A1 'Subject Alternative Name'
//	jq -r '.signatures[0].bundle.Payload.body' provenance.intoto.jsonl |
//	  base64 -d | jq .
//
// The first prints the in-toto statement — subject digest, build type and
// predicate. The second prints the workflow identity out of the leaf of the
// Fulcio chain, which dsseEnvelope carries in cosign's `cert` extension
// field for exactly this reason. The third prints the log entry recorded for
// this envelope, countersigned by the log at upload time; the bundle around
// it is the same format the image signature's `dev.sigstore.cosign/bundle`
// annotation carries, one decode shallower because an envelope is JSON and
// an annotation is a string.
//
// `cert` holds the *whole* PEM chain, leaf first, rather than cosign's split
// of a leaf in `cert` and the intermediates in `chain` — dsseEnvelope records
// why. `openssl x509` above reads the first certificate, which is the leaf
// and the one carrying the identity, so the command is unaffected; a reader
// that requires exactly one certificate there is not.
//
// The statement is readable whatever signed it. The identity and the log
// entry are keyless properties: a publish signed with `--signing-key`
// contacts no CA and no log, so its envelope carries a bare `publicKey` in
// place of `cert` and no bundle at all, and how a verifier learns that key is
// the caller's to arrange.
//
// Checking the *signature* over that statement needs DSSE tooling rather
// than a cosign subcommand, because none of this is in cosign's attestation
// layout: the signature is over the DSSE pre-authentication encoding of the
// payload type and the payload, not over the payload bytes. verifyEnvelope
// in tests/attest.go is this repository's reference implementation of that
// check, and it is about thirty lines.
//
// # Tying the log entry to the envelope it travels with
//
// A bundle beside a signature proves nothing until you have checked it is a
// bundle for *that* signature. The entry is a `hashedrekord` over the SHA-256
// of the envelope's pre-authentication encoding, so the check is to rebuild
// that encoding from the envelope and compare:
//
//	env=provenance.intoto.jsonl
//	type=application/vnd.in-toto+json
//	jq -r .payload "$env" | base64 -d > statement.json
//	rebuilt=$({ printf 'DSSEv1 %d %s %d ' "${#type}" "$type" "$(wc -c < statement.json)"
//	            cat statement.json; } | sha256sum | cut -d' ' -f1)
//	logged=$(jq -r '.signatures[0].bundle.Payload.body' "$env" | base64 -d |
//	         jq -r .spec.data.hash.value)
//	[ "$rebuilt" = "$logged" ] && echo "the log entry is over this envelope"
//
// The `cut` is not decoration: `sha256sum` prints the hash, two spaces and
// the file name — `-` for stdin — so the two values are not comparable
// without it, and a snippet that has to be eyeballed is one that gets
// eyeballed wrongly. The comparison is the point, so it is the thing the
// snippet prints.
//
// If the two match, the entry the log countersigned is an entry over the
// signature in this envelope, made by the certificate in this envelope —
// which is what establishes that the certificate was inside its ten-minute
// validity window when it signed, the property sign.go's defaultRekorURL
// comment calls the difference between a trade and a hole. "The certificate
// in this envelope" is the leaf: the entry names one certificate and `cert`
// carries the chain, so the entry's
// `spec.signature.publicKey.content`, base64-decoded, is the *first*
// certificate in `cert` and not the whole field. Comparing it against all of
// `cert` will not match, by construction.
//
// The entry is a hashedrekord rather than Rekor's `intoto` type, which is the
// shape sigstore built for a DSSE envelope, and that is deliberate: an
// `intoto` upload sends the whole envelope, and a provenance statement names
// the repository, the commit and the workflow that produced it. The public
// log is permanent, so a private image's build history would be published by
// the act of anchoring its provenance. rekorBundle records the same
// proposition against a hash and sends nothing that has to be kept.
//
// What no command here does is check the log's countersignature itself —
// that the log really issued this timestamp. That needs the log's public key
// (`curl https://rekor.sigstore.dev/api/v1/log/publicKey`) and an ECDSA
// verification over the canonicalized entry, which is what sigstore's own
// verifiers do and what no cosign subcommand will do for this layout. Nothing
// in this module ships that verifier, and the suite's own log is
// countersigned by a key nobody trusts, so what this repository establishes
// is the encoding: that the entry travelling with an envelope is an entry
// over that envelope's signature, and that the log's answer reached the
// bundle unaltered.
//
// # Why the documents are referrers alone
//
// Writing them a second time under cosign's `.att` tag would make
// `cosign verify-attestation` work, and it is rejected. A duplicate costs
// registry storage on every publish and leaves two copies of one document
// with nothing downstream able to say which is right once they disagree.
//
// Which raises the obvious question, since the signature does copy cosign's
// tag layout rather than being written as a referrer. The layout decides
// different things in the two cases. A cosign signature means nothing
// outside cosign's layout: the signed payload is a layer and the signature
// sits in annotations beside it, in a shape cosign alone parses, so
// publishing it any other way means publishing a verifier alongside it —
// sign.go records the same reasoning from the signature's side. A DSSE
// envelope carries its own payload type, its own signature and its own
// certificate, so whatever can fetch the bytes can check them. For the
// signature the layout therefore decides whether verification is possible
// at all; for the documents it decides only how they are found. Where only
// discovery is at stake, the mechanism built for discovery wins over the
// workaround for registries that had none.
//
// # Lint
//
// The lint stage runs golangci-lint v2. A repository adopting this pipeline
// keeps its `.golangci.yml` in the v2 dialect — the file opens with
// `version: "2"` — because a v2 binary refuses a v1 file outright rather
// than ignoring it. Supplying no file at all takes the bundled policy in
// configs/golangci.yml, which is the v2 file to copy from. The release is
// pinned once, in the `go` module; GoChain.WithLint's version moves it,
// either major.
package main

import (
	_ "embed"

	"dagger/z-5-labs/internal/dagger"
)

//go:embed configs/golangci.yml
var defaultLintConfig []byte

// Z5labs is the root module type. Construct the Go language chain via Go;
// everything this module does is reached from there.
type Z5labs struct{}

// Go returns the Go language chain bound to source: the standardized
// checks — gofmt, go vet, golangci-lint and `go test -race` — over a Go
// source tree, configured by the chain's With* methods and run by its
// terminal Ci, plus the build terminal App.
//
// This is what a Go library needs and all it needs, which is why there is
// no library archetype: a library is a source tree you never call App on.
// An application starts here too; the build and the publish sit downstream
// of this chain rather than beside it.
//
// The returned object is GoChain rather than Go because the `go` module
// this one depends on already owns that name — see GoChain's doc comment.
//
// +cache="session"
func (m *Z5labs) Go(source *dagger.Directory) *GoChain {
	return &GoChain{Source: source}
}
