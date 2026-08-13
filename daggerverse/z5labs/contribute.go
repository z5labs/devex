package main

import (
	"context"
	"fmt"
	imagepath "path"
	"strings"

	"dagger/z-5-labs/internal/dagger"
)

// The "path" package is imported under a name of its own because both helpers
// take a parameter called path, and a parameter's name is the CLI flag callers
// type. Renaming the parameter to free the identifier would rename --path.

// Contributing content to an image, and the two rules that shape it.
//
// An App used to be exactly what a language chain built, which meant an
// application whose image needs anything beyond its executable had nowhere to
// put it — the gap that made two adopters abandon this archetype and hand-roll
// their image build, giving up the SBOMs, the signed provenance and the
// annotations along with it. WithFile and WithDirectory are the seam that
// closes it, and they are declarative on purpose: the rejected alternative was
// a callback taking the built container and handing one back, which can swap
// the base off scratch, add a shell or drop the non-root user, and which the
// module could only detect by auditing whatever came back.
//
// # Permissions and ownership are the module's
//
// Neither helper takes an owner or a permission argument, and both override
// whatever the supplied content carried. A caller contributing a
// world-writable file, or one owned by root, is describing a property of the
// tree they happened to have rather than of the image they want, and an image
// this pipeline publishes has one answer to both questions.
//
// # Bytes arrive described or they do not arrive
//
// Every contribution carries an SPDX document, because the image's SBOM has to
// account for the whole image rather than only the binary a chain built. A
// helper that admitted undescribed content would make the publish contract —
// an SBOM per platform, and no way to publish without them — true by the
// letter and false in substance, in a way no consumer can detect. sbom.go
// carries the assembly; Z5labs.FileDocument and Z5labs.DirectoryDocument
// produce a document for content whose ecosystem has no module able to.
//
// What is enforced is that a document arrives, that it parses as SPDX 2.3, that
// it describes exactly one thing, and that the thing it describes is *these*
// bytes: a publish hashes the contributed content and refuses a document naming
// any other digest. That last check was not here at first — the limit stated in
// this comment was that a caller passing the wrong document published an SBOM
// about bytes that are not in the image — and devex#410 closed it, because an
// asserted document is only worth something if something can tell it apart from
// a worthless one. digest.go carries the rule, and why it runs at publish time.
//
// What is still not enforced is what is *inside* a document. Nothing here can
// re-derive an arbitrary ecosystem's dependency graph, so a supplier can
// overstate what their own artifact is made of. That is a claim someone made,
// in a signed artifact, checkable by anyone who pulls the image — a different
// thing from a document about some other artifact entirely, which is no longer
// publishable.
//
// # The PATH is composition's, and nothing may be contributed onto it
//
// Neither helper may put anything in, under or over any directory the image's
// PATH resolves against. App.WithApp is what puts an executable there — in
// /usr/local/bin, the plugin directory, see compose.go — and the reason is the
// one prebuilt.go states about executables generally: a *dagger.File and a
// *dagger.Directory carry no architecture, and both helpers contribute the same
// bytes to every variant, so a contribution into a directory the PATH searches
// is a way to leave an arm64 image holding an amd64 executable that something
// found by name. Composition carries a platform on every byte, pairs the
// variants platform by platform, and execs the entry before a byte is
// published.
//
// The rule is the whole PATH and not the plugin directory alone, because the
// hazard is discovery rather than convention: /usr/local/bin is one of the six
// directories appPath names, and a tree at /usr/bin would be found by name
// exactly as one at /usr/local/bin would. pathDirs derives the set from appPath
// so that adding a directory to the PATH protects it.
//
// It was refused by *accident* before devex#427, and only half of it: a
// contributed file lands 0444 and is therefore not executable, while a
// contributed tree lands 0555 throughout and is — so the rule the mode policy
// enforced by omission was bypassed by wrapping the same binary in a directory.
// A rule that depends on a mode bit is not a rule, so it is stated here and
// enforced on the path instead.
//
// The executable bit a 0555 tree carries *off* the PATH stays, and is accepted
// rather than worked around: nothing discovers it, because every directory the
// PATH searches is one no contribution can reach, so an executable contributed
// at /opt/thing/run is reachable only by something that already knows that path
// — the caller's own arrangement inside their own image, and not a contract
// this module advertises.
//
// # A symbolic link is not content
//
// A contributed tree may not carry one, nor a device, a pipe or a socket. The
// refusal is walkTree's, so it applies to the tree the module actually copies
// rather than to a claim about the tree a caller passed: Z5labs.DirectoryDocument
// refuses to describe such a tree, and a publish refuses it again when it
// recomputes the digest, which is the path a caller who brought their own
// document takes.
//
// Every other rule in this file works on path strings, and a link is the one
// kind of content that walks past all of them. The mode above is set on the
// copied tree, and a mode on a link is not a mode on what it resolves to.
// overlappingPath refuses a contribution that collides with the entrypoint or
// with something already contributed by comparing cleaned paths, and a link
// names a path that comparison never sees. And the digest, which is the control
// digest.go exists to provide, is taken over a list of regular files — so a
// link contributed nothing to it, and a tree with one hashed the same as the
// tree without it. That last one made the claim in digest.go false, in the
// exact mechanism devex#410 added to make an asserted document checkable.
//
// Four things were measured against Dagger v0.21.8 before choosing, because
// the answer depends on them (devex#428):
//
//   - Directory.Export preserves a link as a link, relative and absolute
//     alike. So the export the digest is computed over really does carry the
//     link, and the walk really does skip it.
//   - The copy into a container preserves it too, unchanged. So the image
//     really does end up holding a link nothing described. Its mode is not the
//     module's to set and never was: a symlink's permission bits are 0777 on
//     Linux and cannot be changed, which is the sense in which the
//     normalization above does not reach it.
//   - An absolute link exported to this module's filesystem points into *this
//     module's* filesystem. /etc/passwd in a contributed tree is one path
//     during the export and a different file in the image, which is why
//     "allowed when it resolves inside the contributed tree" is not a rule that
//     can be checked where the module can check anything.
//   - WithFile needs no rule of its own, which is the one measurement that
//     closed a seam rather than opening one. Directory.File on a link resolves
//     it: what comes back is the target's bytes as an ordinary file, so the
//     digest and the image get the same content and there is nothing for a link
//     to hide behind. A *dagger.File is bytes, and a link is not bytes.
//
// Composition needs no rule of its own either. App.WithApp carries the inner
// application's contributions into the outer image as contributions, tree
// handle and all, so the outer publish walks them again — see compose.go.
//
// The alternatives were to follow links, to keep skipping them, or to admit
// them and describe them. Following one puts a digest for bytes that are not in
// the image into the document, and the third measurement above says which
// filesystem's bytes those would be. Skipping is the status quo and is the hole
// itself. Describing them means inventing a checksum for a thing that has none
// and teaching every consumer of these documents what it meant, for content a
// scratch image has almost no use for. Silently dropping them from the copy was
// rejected for the reason every other silent repair here is: an image missing
// something the caller put in it, with a document that agrees, is the
// undetectable incompleteness this whole file is written against.
//
// So the caller contributes what the link pointed at, at the path it should
// have in the image, and everything downstream describes bytes that are really
// there. If a real need for links inside an image ever arrives — a base layer
// makes that likelier — it arrives as a seam that names them, not as content
// that slipped past the seams.
//
// # The directories on the way are the image's, not the contribution's
//
// Contributing at /etc/ssl/certs/ca-certificates.crt on a scratch image brings
// /etc, /etc/ssl and /etc/ssl/certs into existence. Those are the image's
// structure rather than the caller's content: they are created root-owned and
// 0755, which is the conventional layout an image with a real base layer would
// already have, and which is deliberately *not* writable by the non-root user
// the application runs as. They carry no bytes, so there is nothing for a
// checksum to describe and nothing an SBOM omits by not listing them — the
// contribution documents enumerate files for the same reason. Only the
// contributed path itself, and everything under it, is the module's to own.
//
// # There is no environment helper
//
// Deliberately, and permanently: see the package doc's "Environment is a
// runtime concern" section, which records why every category of variable has
// an owner other than the caller.

// appOwner owns every byte a caller contributes to an image.
//
// It is numeric rather than a name because a scratch image has no
// /etc/passwd for a name to resolve against, and 65532 specifically because
// that is the uid distroless nonroot images use — the number a base layer
// would agree with if one of these images ever gains one. The runtime user
// itself is devex#399's; this is only who the files belong to.
const appOwner = "65532:65532"

// The modes a contribution lands with. Read-only, because content
// contributed at build time is not content the application rewrites at
// runtime, and an image whose files it can rewrite is one whose published
// digest stops describing what is running.
//
// The directory mode is the residue worth stating rather than hiding. The
// permission is applied to the copied directory *and its contents*, so the
// mode is uniform: a directory needs 0555 to be traversable, which drags the
// files inside it to 0555 too. That is accepted rather than worked around,
// because the alternative is rebuilding the tree file by file at 0444 under a
// 0555 parent, which hits the chained-WithFile fold ceiling on a large
// directory — a tree of ten thousand templates is a use case.
//
// What the residue is *not* is a way onto the PATH. It used to be exactly that,
// which is what devex#427 closed: nothing may be contributed at, under or over
// any directory the image's PATH resolves against, so an executable a tree
// carries is discovered by nothing and can only be run by something that
// already knows its absolute path. That is the caller arranging their own
// image, rather than this module admitting an executable whose architecture
// nobody stated.
const (
	contributedFileMode      = 0o444
	contributedDirectoryMode = 0o555
)

// normalizedTree is dir with every file and directory in it set to the module's
// mode, ready to be copied into an image.
//
// The normalization is a step of its own because of where the permission is
// honoured, which is measured rather than assumed (Dagger v0.21.8):
// Directory.withDirectory applies `permissions` to the copied directory and
// everything under it, and Container.withDirectory silently ignores it —
// a tree copied straight into a container keeps the modes it arrived with,
// 0755 and 0644 for anything built with dag.Directory(). Only `owner` is
// applied on the container side, which is why that half stays there. A
// contribution whose mode was the caller's would make "the module decides the
// mode" a comment rather than a property, and nothing about the resulting image
// would say so.
//
// The tree is normalized under a name and then taken back out of it, rather
// than being written at the root: the root of a Directory is not something the
// copy sets a mode on, so the contributed tree's own directory would keep 0755
// while everything inside it changed.
//
// What the normalization does not reach is a symbolic link — a mode on a link
// is not a mode on what it resolves to, and the copy carries the link through
// unchanged. That is one of the three reasons a tree carrying one is refused
// outright rather than normalized; see the file comment above.
func normalizedTree(dir *dagger.Directory) *dagger.Directory {
	const at = "content"
	return dag.Directory().
		WithDirectory(at, dir, dagger.DirectoryWithDirectoryOpts{Permissions: contributedDirectoryMode}).
		Directory(at)
}

// WithFile contributes a file to every platform's image at path.
//
// The file lands at path in each variant, mode 0444, owned by the image's
// non-root user. Neither the mode nor the owner is a caller's to state: see
// the file comment above.
//
// document is an SPDX 2.3 JSON document describing what is in the file, and it
// is required — content arrives described or it does not arrive, because the
// documents a publish attaches describe the whole image rather than the binary
// a chain built. Z5labs.FileDocument produces one for content with no
// ecosystem, computing the digests itself; content that *has* an ecosystem
// should carry that ecosystem's document, the way a Go binary carries
// dag.Go().Spdx.
//
// The bytes are platform-neutral by construction: one *dagger.File is
// contributed to every variant. That is why there is no WithExecutable beside
// this — a raw file carries no platform, so a helper landing one in the
// executable directory would silently admit a binary built for the wrong
// architecture — and it is why a path at, under or over any directory the
// image's PATH resolves against is refused here rather than merely being
// useless. Platform-specific executables arrive as an App instead: App.WithApp
// composes one, matched platform by platform, and lands its entry in the plugin
// directory.
//
// +cache="session"
func (a *App) WithFile(
	ctx context.Context,
	// The absolute path in the image to contribute the file at.
	path string,
	// The file to contribute.
	file *dagger.File,
	// An SPDX 2.3 JSON document describing the file's contents.
	document *dagger.File,
) (*App, error) {
	if file == nil {
		return nil, fmt.Errorf("withFile requires a file to contribute")
	}
	clean, err := a.acceptContribution(ctx, "withFile", path, document)
	if err != nil {
		return nil, err
	}
	for _, v := range a.Variants {
		v.Container = v.Container.WithFile(clean, file, dagger.ContainerWithFileOpts{
			Owner:       appOwner,
			Permissions: contributedFileMode,
		})
		v.Contributions = append(v.Contributions, contribution{Name: clean, Path: clean, File: document, Content: file})
	}
	a.ContributedPaths = append(a.ContributedPaths, occupied{Path: clean, Holder: contributedHolder})
	return a, nil
}

// WithDirectory contributes a directory tree to every platform's image at
// path.
//
// The tree lands at path in each variant, mode 0555 throughout, owned by the
// image's non-root user. The mode is uniform — every file inside it is 0555
// too, rather than 0444 under a traversable parent — and the comment on
// contributedDirectoryMode above records why that is accepted rather than
// worked around.
//
// An executable inside such a tree is therefore executable, and that is not a
// way to extend the image: like WithFile, this refuses a path at, under or over
// any directory the image's PATH resolves against, so nothing a caller
// contributes is ever discovered on the PATH by name. Wrapping a binary in a
// directory used to be exactly that bypass — see the file comment above and
// App.WithApp, which is the seam that names the platform of every byte it
// brings.
//
// The tree may hold directories and regular files and nothing else. A
// symbolic link in it is refused — when the document is produced, and again at
// publish time for a caller who brought one of their own — because a link is
// the one kind of content none of the rules here can see: it is not given the
// mode, the path it names is not compared against anything, and it is in no
// document and no digest. The file comment above carries the decision and what
// was measured to reach it; contribute what the link pointed at instead.
//
// document is an SPDX 2.3 JSON document describing the tree, and it is
// required for the reason WithFile's is. Z5labs.DirectoryDocument produces one
// for content with no ecosystem, and it enumerates: the document it writes
// carries one file element per file in the tree, because "the contribution is
// described" and "every file in the image is accounted for" are different
// promises and only the second one is the point.
//
// +cache="session"
func (a *App) WithDirectory(
	ctx context.Context,
	// The absolute path in the image to contribute the directory at.
	path string,
	// The directory to contribute.
	dir *dagger.Directory,
	// An SPDX 2.3 JSON document describing the directory's contents.
	document *dagger.File,
) (*App, error) {
	if dir == nil {
		return nil, fmt.Errorf("withDirectory requires a directory to contribute")
	}
	clean, err := a.acceptContribution(ctx, "withDirectory", path, document)
	if err != nil {
		return nil, err
	}
	tree := normalizedTree(dir)
	for _, v := range a.Variants {
		v.Container = v.Container.WithDirectory(clean, tree, dagger.ContainerWithDirectoryOpts{
			Owner: appOwner,
		})
		v.Contributions = append(v.Contributions, contribution{Name: clean, Path: clean, File: document, Tree: dir})
	}
	a.ContributedPaths = append(a.ContributedPaths, occupied{Path: clean, Holder: contributedHolder})
	return a, nil
}

// acceptContribution validates everything both helpers share and returns the
// path the content will actually land at.
//
// The document is checked here rather than at publish time as well as at
// publish time. Both are required arguments, so the schema refuses a null
// before this runs and this branch is unreachable from the CLI; what it buys
// is that a *module* calling these — devex#401's composition, anything future
// — is refused at the call it got wrong rather than at a publish minutes
// later, and resolveContributions still refuses the same thing at the far end
// because that is the last point before bytes are pushed.
func (a *App) acceptContribution(ctx context.Context, method, raw string, document *dagger.File) (string, error) {
	if document == nil {
		return "", fmt.Errorf(
			"%s requires a document describing what is being contributed: every byte that enters an image arrives with one, "+
				"and fileDocument and directoryDocument produce one for content whose ecosystem has none", method)
	}
	clean, err := validateContributionPath(method, raw)
	if err != nil {
		return "", err
	}
	taken, err := a.occupiedPaths(ctx)
	if err != nil {
		return "", err
	}
	if why := overlappingPath(clean, taken); why != "" {
		return "", fmt.Errorf(
			"%s cannot contribute at %s: %s, and content that landed on top of it would leave the image's documents "+
				"describing bytes that are not in it", method, clean, why)
	}
	return clean, nil
}

// occupied is one path something in the image already holds, and what holds
// it — a noun phrase, because it is read as the subject of the refusal.
//
// The holder is carried rather than derived because it is the whole content of
// the message: "something is already contributed at /app/hello" is a confusing
// thing to tell a caller about their own application's binary, and the
// entrypoint collision is the one an adopter hits first.
type occupied struct {
	Path   string
	Holder string
}

// contributedHolder is what WithFile and WithDirectory record as holding the
// path they landed at. Composition records something more specific — which
// application brought the bytes — which is the reason the holder is carried
// with the path rather than assumed by whatever reads the list.
const contributedHolder = "content already contributed"

// occupiedPaths is every path in the image that something already holds: the
// entrypoint each variant runs, and everything contributed before now.
//
// The entrypoint is read from the containers rather than recomputed, because
// App does not know what built it and deliberately holds no binary name. It is
// an image *config* read — no build is solved to answer it — and Dagger serves
// the repeat within a session from cache, so a chain of contributions does not
// pay for it once per call. It is deliberately not captured into a field on
// App: the containers are what the entrypoint is a property of, and a copy in
// App state is a value that can disagree with them.
func (a *App) occupiedPaths(ctx context.Context) ([]occupied, error) {
	out := make([]occupied, 0, len(a.ContributedPaths)+len(a.Variants))
	out = append(out, a.ContributedPaths...)
	for _, v := range a.Variants {
		entrypoint, err := v.Container.Entrypoint(ctx)
		if err != nil {
			return nil, fmt.Errorf("read the %s image's entrypoint: %v", v.Platform, err)
		}
		for _, arg := range entrypoint {
			// The entrypoint is one absolute path in every image this module
			// builds. Anything else in it is an argument rather than a path,
			// and treating an argument as a reserved path would refuse a
			// contribution for no reason.
			if strings.HasPrefix(arg, "/") {
				out = append(out, occupied{
					Path:   imagepath.Clean(arg),
					Holder: "the application's own binary, which the image runs",
				})
			}
		}
	}
	return out, nil
}

// validateContributionPath refuses a path that cannot describe a place in the
// image, and returns the cleaned form of one that can.
//
// Absolute only. A relative path would resolve against the container's
// working directory, which this module never sets — so "etc/hosts" would land
// at /etc/hosts today and somewhere else the moment an image gains a workdir,
// with nothing in the document to say which.
//
// TMPDIR's directory is refused, and it is the one refusal here that is not
// about the path being unusable. /tmp is a mount point the deployment fills —
// the image ships no /tmp at all, see the package doc — so content contributed
// under it would be in the published image, described by its document, and
// invisible at runtime behind whatever gets mounted over it. That is the same
// undetectable incompleteness the overlap rules exist for, arriving from the
// deployment's side instead of from another contribution's.
//
// Every directory the image's PATH resolves against is refused too, in all
// three directions — the directory itself, anything under it, and anything that
// would contain it. App.WithApp is what puts an executable on the PATH; the
// file comment above carries why a contribution there is a wrong-architecture
// executable waiting to be discovered by name, and why the rule is the whole
// PATH rather than the plugin directory alone. The containing direction is
// refused without looking inside the tree, exactly as overlappingPath refuses a
// contribution that would contain something already in the image: a tree at
// /usr/local is refused whether or not it happens to carry a bin/ today,
// because whether it does is a property of the caller's tree rather than of the
// call, and a rule that has to read the content is one that changes its mind
// between builds.
//
// The image's root is refused before any of that, by the check above, which is
// what keeps "/" out of the containing direction — a candidate of "/" would
// make the prefix test look for "//".
//
// HOME's directory is *not* refused, and the asymmetry is deliberate.
// /home/nonroot is in the image and read-only, and a deployment mounts over it
// only in the one case where an application genuinely needs a writable home —
// so read-only content under it, a default configuration an operator can
// override by mounting one, is content that is normally there at runtime.
//
// The path is cleaned — "/srv/./templates//index.html" and a trailing slash
// are normalized — and it is otherwise taken literally. In particular
// surrounding whitespace is *not* stripped: " /srv/data" is refused for not
// being absolute rather than silently accepted, and "/srv/data " lands at
// "/srv/data ", which is a legal path on Linux and is what the caller asked
// for. Trimming instead would put content somewhere other than where the
// caller said, with nothing in the image or the document to say so, and that
// is the failure this whole file is written to avoid — arriving through a
// convenience.
func validateContributionPath(method, raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("%s requires a path in the image to contribute at", method)
	}
	if !strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf(
			"%s: %q is not an absolute path, and a relative one would resolve against a working directory this pipeline never sets",
			method, raw)
	}
	clean := imagepath.Clean(raw)
	if clean == "/" {
		return "", fmt.Errorf("%s: the image's root is not a path to contribute at", method)
	}
	if clean == appTmpDir || strings.HasPrefix(clean, appTmpDir+"/") {
		what := appTmpDir + " is the scratch space TMPDIR names"
		if clean != appTmpDir {
			what = clean + " is inside " + appTmpDir + ", the scratch space TMPDIR names"
		}
		return "", fmt.Errorf(
			"%s: %s, and the image deliberately does not carry it — the deployment mounts a tmpfs or an emptyDir "+
				"there, and anything contributed under it disappears the moment it does",
			method, what)
	}
	if where := pathDirCollision(clean); where != "" {
		return "", fmt.Errorf(
			"%s: %s, and nothing may be contributed to a directory the image's PATH resolves against — a contributed "+
				"file or tree carries no architecture and lands in every variant, so content that something finds on "+
				"the PATH by name is a way to leave an arm64 image running an amd64 executable; withApp composes "+
				"another application's payload, which is matched platform by platform and run before anything is "+
				"published, and lands its entry in %s",
			method, where, appPluginDir)
	}
	return clean, nil
}

// pathDirs is every directory the image's PATH resolves against, with the
// plugin directory first.
//
// It is derived from appPath rather than listed again, so the set the rule
// protects is the set the image really searches: a directory added to the PATH
// is protected by having been added, and one removed stops being protected the
// same way. Two lists would be two things to keep in agreement, and the way
// they would disagree is a directory on the PATH that a caller may contribute
// an executable of no stated architecture into.
//
// The plugin directory is first because it is the one a candidate is most
// likely to collide with and the only one anything fills, so it is the one a
// refusal should name when a path collides with more than one — /usr/local
// contains both it and /usr/local/sbin, and being told about the plugin
// directory is what points at withApp.
func pathDirs() []string {
	out := []string{appPluginDir}
	for _, dir := range strings.Split(appPath, ":") {
		if dir != appPluginDir && dir != "" {
			out = append(out, dir)
		}
	}
	return out
}

// pathDirCollision reports how clean collides with a directory the image's
// PATH resolves against, or "" when it collides with none of them.
//
// The rule is the PATH's rather than the plugin directory's alone, and that is
// the correction devex#427's review forced. The hazard is discovery by bare
// name: a contributed tree lands 0555 throughout and carries no architecture,
// so an executable in it is a wrong-architecture binary something can find by
// name — and /usr/local/bin is one of six directories the image's PATH names,
// not the only one. A rule that guarded the plugin directory alone would have
// left /usr/bin and /bin open while the comments above claimed nothing
// contributed is ever discovered on the PATH.
//
// The three directions are the three overlappingPath distinguishes, and they
// are separated here for the same reason: a caller told only that a path is
// refused has to work out for themselves whether they landed in the directory
// or over it.
func pathDirCollision(clean string) string {
	for _, dir := range pathDirs() {
		what := ", a directory the image's PATH resolves against"
		if dir == appPluginDir {
			what = ", the plugin directory an extension's executables land in"
		}
		switch {
		case clean == dir:
			return clean + " is" + strings.TrimPrefix(what, ",")
		case strings.HasPrefix(clean, dir+"/"):
			return clean + " is inside " + dir + what
		case strings.HasPrefix(dir, clean+"/"):
			return clean + " would contain " + dir + what
		}
	}
	return ""
}

// overlappingPath reports how the candidate collides with something already in
// the image, or "" when it collides with nothing.
//
// Overlap is the general form of two failures that look different and are the
// same: contributing twice at one path, where the second silently replaces the
// first, and contributing a tree over — or under — something already there.
// In every case the image ends up holding one thing while its documents
// describe two, which is precisely the undetectable incompleteness the whole
// contribution mechanism exists to prevent. A collision with the entrypoint is
// the same failure with the binary on the losing side, and it says so: what
// each collision is *with* is carried in the message, because a caller told
// only that a path is taken has to go and find out by what.
func overlappingPath(candidate string, taken []occupied) string {
	for _, other := range taken {
		switch {
		case candidate == other.Path:
			return other.Holder + " is already there"
		case strings.HasPrefix(candidate, other.Path+"/"):
			return "it is inside " + other.Path + ", which is " + other.Holder
		case strings.HasPrefix(other.Path, candidate+"/"):
			return "it would contain " + other.Path + ", which is " + other.Holder
		}
	}
	return ""
}
