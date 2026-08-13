package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/z-5-labs/internal/dagger"
)

// Building an App out of executables this module did not compile.
//
// GoChain.App was the only way to get an App, which left three things with no
// way in: a prebuilt or vendor executable, a language whose chain nobody has
// written yet, and the payload shapes the contribution seam has to handle but
// which no Go build produces. Z5labs.App is the seam for all three, and
// GoChain.App is built on it rather than beside it — so the generic path is
// exercised by every Go build and there is no second code path to drift.
//
// The consequence of that is accepted knowingly and is worth stating plainly:
// anything this constructor cannot express, no language chain can express
// either.
//
// # Why this is a builder, when devex#404 deleted the last one
//
// One file is one platform and Dagger takes no map parameters, so a
// multi-platform App cannot arrive in a single call. That leaves a
// constructor, a repeated per-platform contribution and a terminal — a builder
// in all but name — and it is chosen rather than fallen into.
//
// The alternative was Z5labs.App returning an *App directly, with WithVariant
// hanging off App beside WithFile and WithDirectory. It is rejected because an
// App is the thing that has variants: WithFile applies content to every
// variant there is, so a contribution made before the first variant would land
// on nothing at all, silently, and an App with no variants would exist for as
// long as the caller kept chaining. AppBuilder makes the zero-variant state
// unrepresentable in an App — Build is the only way to get one and it refuses
// an empty set — which is also the only place "constructing with zero variants
// fails" can be true.
//
// The intermediate is therefore a distinct registered type, and its name is
// AppBuilder. The generated schema names it Z5LabsAppBuilder; neither
// dependency of this module (go, oci) owns that name, and Build returns an
// object rather than a scalar, so it emits no lowercased cache field to
// collide with a Go keyword. Both of those are codegen landmines
// daggerverse/CLAUDE.md records rather than things worth rediscovering.
//
// # Build identity is not an argument, here or anywhere
//
// AppBuilder takes no commit, no source URI and no build time. That is not a
// gap in what it can express: a build identity a caller could have supplied
// identifies nothing, which is the same rule that keeps a repository parameter
// off the provenance. A language chain records those facts because it observed
// them — GoChain read HEAD out of the tree it compiled — so it sets them
// through an unexported seam that no caller can reach. An App assembled from
// prebuilt executables observed nothing, so its provenance records the publish
// and the image and says nothing about a source, which is the honest answer.
//
// # Documents are asserted, and checked against the bytes
//
// A Go binary's document is derived from the artifact by
// `dag.Go().Spdx`, so it cannot disagree with what it describes. A document
// handed to WithVariant can. The mitigation is the same one every contribution
// now carries: the document has to name the SHA-256 of the content it
// accompanies, and a publish verifies it. See digest.go.

// AppBuilder assembles an App from executables somebody else built: one
// entry per platform, each with the document describing it, terminated by
// Build.
//
// Construct it with Z5labs.App. The file comment above records why the
// intermediate type exists at all and why it is this shape.
type AppBuilder struct {
	// Version is what every image will be published under. It is validated
	// by Build rather than by the constructor, because the constructor has
	// no way to report an error.
	//
	// +private
	Version string
	// Commit, SourceURI, Created and Pkg are the build facts a language
	// chain observed. They are empty for an App assembled from prebuilt
	// executables and there is deliberately no caller-facing way to set
	// them — see the file comment.
	//
	// +private
	Commit string
	// +private
	SourceURI string
	// +private
	Created string
	// +private
	Pkg string
	// Variants are the entries contributed so far, in the order they were
	// contributed. The +private is load bearing: pendingVariant is an
	// unexported type, so exposing this field would ask the generator to
	// register a schema object it cannot build.
	//
	// +private
	Variants []*pendingVariant
}

// pendingVariant is one platform's executable, waiting for Build to package
// it. Its fields are exported because the round trip across a call boundary
// is encoding/json and an unexported field comes back as its zero value.
type pendingVariant struct {
	Platform dagger.Platform
	// Name is the entry's file name, which becomes the last segment of the
	// image's entrypoint. It is read from the file rather than taken as an
	// argument: the file already has a name, and a second one to keep in
	// agreement with it is a way for the two to disagree.
	Name     string
	Entry    *dagger.File
	Document *dagger.File
}

// entryMode is the mode an application's own executable lands with.
//
// Read-and-execute, never write, and the same read-only rule every
// contribution follows — an image whose files the application can rewrite is
// one whose published digest stops describing what is running. It is 0555
// rather than the 0444 a contributed file gets for the obvious reason: this
// one is exec'd.
const entryMode = 0o555

// App begins an application assembled from executables this module did not
// build: a prebuilt binary, a vendor's CLI, or the output of a language whose
// chain nobody has written yet.
//
// What comes back is a builder. Add one variant per platform with WithVariant
// and finish with Build, which is what refuses an empty set. The App that
// comes out is the same App a language chain produces — the same publish, the
// same hardening, the same annotations, the same SBOMs and the same signed
// provenance — because GoChain.App is built on this constructor rather than
// beside it.
//
// version is the version every image is published under, and the same rules
// apply as to GoChain.App's: it has to be usable as an OCI tag, and SemVer
// build metadata is refused rather than mangled. It is validated by Build.
//
// # What this seam is for, and what it is not
//
// It is for packaging bytes somebody else produced with the hardening, the
// multi-platform publish, the annotations and the attestations this pipeline
// gives everything else. It is not a way to hand this module an image: the
// module still builds the image around the executable, applies the modes and
// the ownership and pins the layout, so what a caller supplies is bounded and
// every part of it is reachable by an exec of the entry. A caller-supplied
// *container* is a different and unbounded thing, and is refused — see
// contribute.go, which turned down the same offer for the same reason.
//
// # Its documents are asserted rather than derived
//
// Say this out loud rather than leaving it to be found. A Go binary's SBOM is
// derived from the compiled artifact, so it cannot disagree with what it
// describes. A document handed to WithVariant is a claim its supplier made.
// What keeps the claim honest is that it has to name the SHA-256 of the
// executable it accompanies and a publish checks it, so a document about other
// bytes fails the publish instead of shipping. What it cannot check is whether
// the components listed inside that document are the ones really linked into
// the executable; that is the supplier's assertion, in a signed artifact,
// checkable by anyone who pulls the image.
//
// +cache="session"
func (m *Z5labs) App(
	// The version every image is published under. Any OCI-tag-safe string;
	// SemVer build metadata is refused.
	version string,
) *AppBuilder {
	return newAppBuilder(version)
}

// newAppBuilder is the constructor GoChain.App shares with Z5labs.App, so
// that "the Go chain is built on the generic constructor" is a property of
// the code rather than a claim in a comment.
func newAppBuilder(version string) *AppBuilder {
	return &AppBuilder{Version: version}
}

// withBuildFacts records what a language chain observed about the build.
//
// Unexported, and that is the whole point: nothing here is a caller's to
// state. GoChain read these out of the working tree it compiled, so it may
// say them; a caller with a prebuilt executable observed nothing and would be
// asserting a provenance nobody can check.
func (b *AppBuilder) withBuildFacts(facts gitState, pkg string) *AppBuilder {
	b.Commit = facts.SHA
	b.SourceURI = facts.SourceURI
	b.Created = facts.Created
	b.Pkg = pkg
	return b
}

// WithVariant contributes one platform's executable, and the document
// describing it.
//
// platform is stated rather than inferred, and that is the rule the whole
// design turns on. A *dagger.File carries no architecture, so a helper that
// took an executable and worked out where it belonged would silently admit
// the failure devex#397 exists to refuse: an index whose arm64 manifest holds
// an amd64 binary, which fails at exec time with the kernel's message and for
// nobody here. Nothing in this module infers a platform from a file.
//
// entry becomes the image's entrypoint. It lands in the standardized
// executable directory, mode 0555, owned by the image's non-root user — the
// same treatment a compiled binary gets, because it goes through the same
// code.
//
// name is what it lands as, and it defaults to the file's own name. Supply one
// when the artifact's file name is not what the application should be called,
// which is the common case for prebuilt binaries: a release pipeline names its
// cross-compiled artifacts app-amd64 and app-arm64, and the entry has to be
// one path in every variant or the entrypoint differs per architecture. Given
// no name and per-platform file names, this refuses the set rather than
// picking one — so `--name` is how a normal `dist/` directory is contributed,
// and the default is for the case where the file is already called the right
// thing.
//
// document is an SPDX 2.3 JSON document describing the executable, and it is
// required for the reason every contribution's is: the SBOM a publish attaches
// accounts for the whole image, and a helper admitting undescribed content
// would make that contract true by the letter and false in substance.
// Z5labs.FileDocument produces one for an executable whose ecosystem has no
// module able to; a Go binary should carry dag.Go().Spdx instead.
//
// # What is refused, and why each one is a real failure
//
// A platform contributed twice, because the second would silently replace the
// first and the App would ship fewer architectures than it was asked for. A
// platform that is not GOOS/GOARCH, because it cannot name a manifest. And an
// entry whose file name differs from the entries already contributed, because
// the entrypoint would then be a different path per architecture — a consumer
// who overrides the entrypoint, or writes a COPY --from= line against the
// image, would be right on one platform and wrong on another with nothing in
// the manifest list to say so.
//
// A single-platform App is expressible and is not a degenerate case: one
// WithVariant and a Build is a complete application, published as one variant
// rather than as a multi-platform index pretending to be one.
//
// +cache="session"
func (b *AppBuilder) WithVariant(
	ctx context.Context,
	// The platform this executable was built for, e.g. linux/arm64.
	platform dagger.Platform,
	// The executable, which becomes the image's entrypoint.
	entry *dagger.File,
	// An SPDX 2.3 JSON document describing the executable.
	document *dagger.File,
	// What the executable is called in the image. Defaults to the file's
	// own name.
	//
	// +optional
	name string,
) (*AppBuilder, error) {
	if entry == nil {
		return nil, fmt.Errorf("withVariant requires an executable to package")
	}
	raw := name
	if raw == "" {
		fileName, err := entry.Name(ctx)
		if err != nil {
			return nil, fmt.Errorf("read the name of the executable contributed for %s: %v", platform, err)
		}
		raw = fileName
	}
	resolved, err := validateEntryName(raw)
	if err != nil {
		return nil, err
	}
	return b.withVariantNamed(platform, resolved, entry, document)
}

// withVariantNamed is WithVariant once the entry's name is known.
//
// The split exists for one reason and it is not tidiness. Reading a name off a
// *dagger.File resolves that file, and a language chain's entry is a binary
// that has not been compiled yet — so a chain going through WithVariant would
// force every platform's cross-compile inside App, turning a constructor that
// costs nothing into one that costs a full build. A chain already knows what
// its artifact is called (GoChain reads it out of go.mod), so it says so and
// keeps the laziness. Everything after this point is shared.
func (b *AppBuilder) withVariantNamed(platform dagger.Platform, name string, entry, document *dagger.File) (*AppBuilder, error) {
	if document == nil {
		return nil, fmt.Errorf(
			"withVariant requires a document describing the executable: every byte that enters an image arrives with one, " +
				"and fileDocument produces one for content whose ecosystem has none")
	}
	if _, _, err := parsePlatform(string(platform)); err != nil {
		return nil, fmt.Errorf("withVariant: %v", err)
	}
	if why := variantConflict(platform, name, b.variantKeys()); why != "" {
		return nil, fmt.Errorf("withVariant cannot contribute %s for %s: %s", name, platform, why)
	}
	b.Variants = append(b.Variants, &pendingVariant{
		Platform: platform,
		Name:     name,
		Entry:    entry,
		Document: document,
	})
	return b, nil
}

// Build packages every contributed executable as an image and returns the
// application.
//
// This is where an empty variant set is refused. An App with no variants is
// publishable-looking and publishes nothing, and — because WithFile and
// WithDirectory apply content to every variant there is — it would swallow
// every contribution made to it without a word. Build existing is what keeps
// that state out of an App entirely.
//
// The version is validated here rather than by the constructor, which has no
// way to report an error. Publish validates it a third time, which is what
// keeps the refusal of SemVer build metadata a property of publishing rather
// than of one constructor.
//
// +cache="session"
func (b *AppBuilder) Build(ctx context.Context) (*App, error) {
	if err := validateVersion(b.Version); err != nil {
		return nil, err
	}
	if len(b.Variants) == 0 {
		return nil, fmt.Errorf(
			"build requires at least one variant: call withVariant with a platform, the executable built for it " +
				"and the document describing that executable")
	}
	// The annotations are a function of what was observed and of the version,
	// so an App assembled from prebuilt executables carries the version alone
	// rather than a revision and a build time nobody can check. ociAnnotations
	// omits what it was not given.
	annotations := ociAnnotations(gitState{
		SHA:       b.Commit,
		Created:   b.Created,
		SourceURI: b.SourceURI,
	}, b.Version)

	variants := make([]*variant, 0, len(b.Variants))
	for _, p := range b.Variants {
		variants = append(variants, &variant{
			Platform:  p.Platform,
			Container: imageForEntry(p.Platform, p.Name, p.Entry, annotations),
			// The executable is a contribution like any other: it is one of
			// the things in the image, and Publish assembles every
			// contribution into the documents it attaches. Carrying the entry
			// itself beside the document is what lets that publish check the
			// document is about these bytes — see digest.go.
			Contributions: []contribution{{Name: p.Name, File: p.Document, Content: p.Entry}},
		})
	}
	return &App{
		Version:   b.Version,
		Commit:    b.Commit,
		SourceURI: b.SourceURI,
		Pkg:       b.Pkg,
		Variants:  variants,
	}, nil
}

// variantKey is one contributed variant reduced to the two things the
// conflict rules read. It exists so those rules are a pure function over
// strings, which is what makes them drivable by a self-test without building
// an application per row.
type variantKey struct {
	Platform string
	Name     string
}

// variantKeys is the set contributed so far, in contribution order.
func (b *AppBuilder) variantKeys() []variantKey {
	out := make([]variantKey, 0, len(b.Variants))
	for _, p := range b.Variants {
		out = append(out, variantKey{Platform: string(p.Platform), Name: p.Name})
	}
	return out
}

// variantConflict reports why a variant cannot join the set, or "" when it
// can. The reason is a noun phrase, because it is read as the subject of the
// refusal.
//
// The duplicate platform is looked for across the whole set before any name
// is, so that a set which is wrong in both ways reports the one that is
// nearer the caller's mistake: contributing arm64 twice is a typo in the
// platform, and reporting it as a naming disagreement sends the reader to the
// wrong argument.
func variantConflict(platform dagger.Platform, name string, taken []variantKey) string {
	for _, other := range taken {
		if other.Platform == string(platform) {
			return "an executable for that platform was already contributed, and the second would replace it"
		}
	}
	for _, other := range taken {
		if other.Name != name {
			return fmt.Sprintf(
				"the executable contributed for %s is called %s, and an entrypoint that differs per architecture "+
					"is one a consumer cannot write a COPY --from= line or an entrypoint override against",
				other.Platform, other.Name)
		}
	}
	return ""
}

// validateEntryName refuses a file name that cannot be the last segment of an
// entrypoint, and returns the one that can.
//
// A name carrying a separator is the case worth naming: the entry lands at
// appDir plus its name, so "bin/hello" would put the executable a directory
// deeper than the layout this module promises, and "../hello" would put it
// somewhere else entirely. The name is either what the caller passed or what
// their file was called, so the message quotes it rather than naming an
// argument.
//
// Surrounding whitespace is refused rather than trimmed, and this is the one
// place that rule differs from the contribution paths in contribute.go — those
// take " /srv/data" literally because the caller typed a path and putting
// content somewhere other than where they said is the failure that file exists
// to avoid. Here the name is not a path a caller chose, it is the last segment
// of an entrypoint they cannot override, and "/app/ hello" is invisible in
// `docker inspect` and wrong in every COPY --from= line written against it.
// Neither trimming nor accepting is right; refusing is.
func validateEntryName(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf(
			"withVariant: the executable has no file name, and the image's entrypoint is named after it; " +
				"give the file a name or pass one to withVariant")
	}
	if name != strings.TrimSpace(name) {
		return "", fmt.Errorf(
			"withVariant: %q opens or closes with whitespace, and the entrypoint would be %s/%s — a path that is legal, "+
				"invisible in an image inspection, and wrong in every COPY --from= line written against it",
			name, appDir, name)
	}
	if strings.ContainsRune(name, '/') {
		return "", fmt.Errorf(
			"withVariant: %q is not a file name, and the executable lands in %s under its own name; "+
				"contribute the file itself rather than a path to it", name, appDir)
	}
	if name == "." || name == ".." {
		return "", fmt.Errorf("withVariant: %q is not a file name", name)
	}
	return name, nil
}
