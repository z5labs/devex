// Package main implements the pdf Dagger module: reading a PDF's text,
// rendering its pages, and reporting what it is made of, as a `dagger call`
// instead of the usual hand-rolled Dockerfile with poppler-utils in it plus a
// shell script to rescue the output.
//
// The text path and the OCR path are different tools for different documents,
// and this module is the one to try first. pdftotext reads the text layer a
// PDF already carries — exact, instant, no recognition involved. A scan
// carries no text layer at all, and that is the case the tesseract module
// exists for; this module's job for a scan is to produce the page images OCR
// runs on. Convert.Text returning nothing is exactly the signal to hand
// Convert.Png's output to tesseract.
//
// There is no official poppler container image, so the module assembles its
// own the way tesseract and qemu do: a module-pinned Alpine plus
// `apk add poppler-utils ttf-liberation`. The font is not decoration. Poppler
// renders a PDF that names a base-14 font without embedding one — the usual
// shape for anything not born as a scan — by asking fontconfig for a
// substitute, and with no font installed there is nothing to substitute: the
// page renders blank, recognition finds nothing, and the command exits 0.
// Liberation is the family to install for it, being metric-compatible with
// Helvetica, Times and Courier, so substituted text keeps the positions the
// document was written with.
//
// Assembling the image means two fetches, not one, and a network that cannot
// reach the public internet has to be told about both. New's registry argument
// moves the *image*; WithApkRepository, WithApkKey and WithApkAuth move the
// *packages*, and are what an air-gapped run needs — a mirrored Alpine image
// still runs `apk add` against dl-cdn.alpinelinux.org otherwise.
//
// No cache directive appears anywhere in this module. Every function is a pure
// transform of its inputs, so Dagger's 7-day default is correct — the same
// posture as tesseract and kicad. (The directive is deliberately not spelled
// out here: a `+key="value"` in doc-comment prose is stripped as a directive
// and mangles the generated description.)
//
// Three renderers sit behind the outputs, chosen per format rather than
// uniformly. The raster family stays on pdftoppm because it alone offers -mono
// and -gray, which is what the OCR path wants. The vector family — SVG, EPS and
// PostScript — is pdftocairo's, that being the only one of the two that emits
// them. HTML is pdftohtml's, which is not a renderer at all so much as a
// re-layout, and shares no options with either.
//
// Four reports describe the document instead of converting it, one per tool:
// Info is pdfinfo, Fonts is pdffonts, Metadata is `pdfinfo -meta` and Signatures
// is pdfsig. Each returns its tool's own report as text. Parsing those into
// structured values is deliberately out of scope — the formats are stable but
// wide, and a caller who wants one field can read it out of these lines more
// cheaply than this module can model all of them.
//
// Two functions change the document's page structure rather than reading it or
// drawing it: Split takes it apart a page per file, Merge concatenates several
// into one. They are lossless where every conversion is not — the pages carry
// their own objects across — and they are the pair with a limitation nothing
// else here has: pdfseparate and pdfunite accept no password, so an encrypted
// document has to be decrypted before either will touch it.
//
// File map (all `package main`, surfaced as one Dagger module):
//
//   - enums.go    — ColorMode and LayoutMode plus the tables mapping them onto
//     poppler's flags, and the internal raster- and per-page-format tables.
//   - document.go — *Document, one bound PDF: its passwords, its page range,
//     and the four reports about it — what pdfinfo says, which fonts it needs,
//     its XMP metadata, and its signatures.
//   - convert.go  — *Convert, the render options and the nine outputs that
//     read them.
//   - pages.go    — Split and Merge, the two page-structure operations. They
//     sit together rather than with their receivers because they are one
//     story: the page-naming contract one writes is the order the other
//     reads.
package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/pdf/internal/dagger"
)

const (
	// alpineImagePath is the repository under the configured registry. Only
	// the registry prefix is caller-overridable, for an image mirrored into a
	// private registry; the path is fixed so every conversion runs on a known
	// toolchain. Where the packages come from is a separate question,
	// answered by WithApkRepository.
	alpineImagePath = "library/alpine"
	defaultRegistry = "docker.io"

	// defaultAlpineTag pins the base image. Alpine v3.24's main repository
	// carries poppler-utils 25.12.0.
	defaultAlpineTag = "3.24"

	// popplerPkg carries all thirteen pdf* binaries this module reaches
	// through Container, of which nine are wrapped: pdftotext, pdftoppm,
	// pdfinfo, pdftocairo, pdftohtml, pdffonts, pdfsig, pdfseparate and
	// pdfunite.
	popplerPkg = "poppler-utils"

	// fontPkg is the substitute font family poppler draws with when a PDF
	// names one of the base-14 fonts without embedding it. See the package
	// comment for why it is not optional. It also brings fontconfig, which is
	// what makes WithFonts work at all.
	fontPkg = "ttf-liberation"

	// apkRepositoriesFile is the list `apk add` resolves packages from.
	// WithApkRepository overwrites it rather than appending to it, so a caller
	// who names a mirror gets an image that cannot quietly fall back to the
	// CDN the mirror exists to replace.
	apkRepositoriesFile = "/etc/apk/repositories"

	// apkKeysDir is where apk looks up the public key an index's signature
	// names. A repository whose key is not here is untrusted, and apk refuses
	// its index rather than installing from it — which is why a private
	// mirror needs WithApkKey and not just WithApkRepository.
	apkKeysDir = "/etc/apk/keys"

	// apkNetrcPath is where WithApkAuth's credentials are mounted and
	// apkNetrcEnv the variable pointing apk at them. Alpine 3.24 ships
	// apk-tools 3.0, whose built-in libfetch reads HTTP credentials from the
	// netrc file `$NETRC` names and from nowhere else that keeps them out of
	// sight: userinfo in the repository URL would land in the repositories
	// file and in every error message quoting it, and HTTP_AUTH would land in
	// the image's environment. The file is mounted rather than written so it
	// is not a layer either.
	apkNetrcPath = "/run/apk/netrc"
	apkNetrcEnv  = "NETRC"

	// fontsDir is where WithFonts mounts the caller's faces. It sits under
	// /usr/share/fonts because that is the one directory
	// /etc/fonts/fonts.conf tells fontconfig to scan, and fontconfig is what
	// poppler asks for a substitute — so a face dropped anywhere else is a
	// face poppler cannot find.
	fontsDir = "/usr/share/fonts/module"

	// workDir holds the mounted document; outputDir is the writable staging
	// area conversions write into.
	workDir    = "/work"
	sourcePath = workDir + "/source.pdf"
	outputDir  = "/out"

	// textOutputPath is the file Convert.Txt writes, psOutputPath the one
	// Convert.Ps writes, and pageBase the OUTPUTBASE handed to pdftoppm — which
	// appends a page number and the format's extension to it.
	textOutputPath = outputDir + "/document.txt"
	psOutputPath   = outputDir + "/document.ps"
	pageBase       = outputDir + "/page"

	// pageNamePrefix is the leading part of a rendered page's file name, and
	// has to agree with pageBase: the normalization pass strips it to read the
	// page number back out.
	pageNamePrefix = "page-"

	// minPageNumberWidth is the digit count every rendered page's number is
	// padded to. See normalizeScript for what it buys.
	minPageNumberWidth = 4

	// defaultDpi is the resolution pages are rendered at when the caller names
	// none, and is pdftoppm's own default. It is a screen-reading resolution
	// rather than an OCR one: a caller handing these pages to the tesseract
	// module wants WithDpi(300), which is the long-standing recommendation in
	// tesseract's own quality documentation.
	defaultDpi = 150

	// userPasswordEnv and ownerPasswordEnv name the variables a bound
	// document's passwords reach the tool through. They are read by the shell
	// out of the process environment rather than passed as a `-upw` argv word,
	// because argv is visible in every Dagger trace and error message.
	userPasswordEnv  = "PDF_USER_PASSWORD"
	ownerPasswordEnv = "PDF_OWNER_PASSWORD"

	// endOfDocument is the WithPageRange `last` that means "to the last page",
	// and the zero value a Document that was never given a range carries.
	endOfDocument = 0

	// metadataFlag is what makes pdfinfo print the document's XMP packet, and
	// only the packet: the ordinary report is suppressed, so a document with no
	// XMP produces no output at all.
	metadataFlag = "-meta"

	// noMetadataReport is what Metadata answers for a document carrying no XMP
	// packet. It is a report and not an error, that document being a perfectly
	// good one, and it is not the empty string poppler produces because an empty
	// string is indistinguishable from a function that never ran.
	noMetadataReport = "No XMP metadata: this document carries no XMP packet. " +
		"Info reports the metadata in its Info dictionary."

	// noSignaturesMarker and signatureInfoMarker are the two openings pdfsig's
	// report comes in — one for a document carrying no signatures, one for a
	// document carrying some — and recognising either is what lets Signatures
	// tell a report from a failed run. pdfsig exits non-zero for both an
	// unsigned document (2) and a signature that did not validate (1), and 1 is
	// also what it exits for a document it could not open at all.
	noSignaturesMarker  = "does not contain any signatures"
	signatureInfoMarker = "Digital Signature Info of:"

	// incorrectPasswordMarker is the text poppler's own message carries when a
	// document is encrypted and the password it was given — including the
	// empty one it tries when given none — does not open it. Recognising it is
	// what lets this module report the encryption rather than passing on a
	// message about a flag the caller never set.
	incorrectPasswordMarker = "Incorrect password"
)

// Pdf is the root namespace for every exported function in this module. It
// carries the image coordinates and the fonts the image is built with;
// Document hangs off it so the generated SDK surfaces conversion under
// `dag.Pdf().Document(...)`.
type Pdf struct {
	// +private
	Registry string
	// +private
	AlpineTag string
	// +private
	Fonts *dagger.Directory
	// +private
	ApkRepositories []string
	// +private
	ApkKeys []*dagger.File
	// +private
	ApkAuth *dagger.Secret
}

// New returns a Pdf module backed by <registry>/library/alpine:<tag> with
// poppler-utils and a substitute font family installed.
func New(
	// Container registry hosting the alpine image. This moves the image only:
	// see WithApkRepository for where the packages installed onto it are
	// fetched from.
	// +default="docker.io"
	registry string,
	// Tag of the alpine image the toolchain is assembled on.
	// +default="3.24"
	alpineTag string,
) (*Pdf, error) {
	if registry == "" {
		registry = defaultRegistry
	}
	if alpineTag == "" {
		alpineTag = defaultAlpineTag
	}
	return &Pdf{Registry: registry, AlpineTag: alpineTag}, nil
}

// WithApkRepository points package installation at an Alpine repository other
// than the one the base image ships, which is what makes this module work on a
// network that cannot reach dl-cdn.alpinelinux.org.
//
// New's registry argument is not enough on its own and never was: it moves the
// *image*, and the packages are still fetched by `apk add` from whatever
// /etc/apk/repositories carries — so mirroring Alpine into a private registry
// buys a container that then fails on its first `apk add`, or, where the CDN
// is blackholed rather than refused, hangs until it times out.
//
// Repeatable, in preference order. The first call replaces the image's list
// rather than appending to it, because the air-gapped case needs the
// unreachable defaults gone rather than merely deprioritized: a repository apk
// still consults is a repository apk still waits for.
//
// The URL is the repository base, spelled the way it would be spelled in
// /etc/apk/repositories — `https://mirror.example.com/alpine/v3.24/main`, one
// call per component. A repository's index is signed, so pair this with
// WithApkKey unless the mirror is signed by a key the base image already
// trusts.
func (p *Pdf) WithApkRepository(
	// Base URL of an Alpine repository to resolve packages from.
	url string,
) *Pdf {
	url = strings.TrimSpace(url)
	if url == "" {
		return p
	}
	out := p.clone()
	if !contains(out.ApkRepositories, url) {
		out.ApkRepositories = append(out.ApkRepositories, url)
	}
	return out
}

// WithApkKey trusts a repository's signing key by dropping it into
// /etc/apk/keys, which is the other half of WithApkRepository: a private
// mirror's index is signed by a key the stock Alpine image has never heard of,
// and apk refuses an index it cannot verify rather than installing from it.
//
// The file keeps its own name, because the name is load-bearing — an index
// signature names the key file it was made with, and apk looks that exact file
// up in the keys directory. A key exported from `abuild-keygen` is already
// named correctly; renaming it renames the key.
//
// Repeatable, for a mirror set signed by more than one key. It is a *File
// rather than a *Secret because a public key is not a credential: it is meant
// to be in the image, and WithApkAuth is the option for the part that is not.
func (p *Pdf) WithApkKey(
	// Public key file trusting a repository's index signature.
	key *dagger.File,
) *Pdf {
	out := p.clone()
	out.ApkKeys = append(out.ApkKeys, key)
	return out
}

// WithApkAuth supplies credentials for a repository that requires them.
//
// The secret's contents are a netrc file — `machine mirror.example.com login
// USER password PASS`, one stanza per host — which is what apk-tools 3's
// built-in libfetch reads when a repository answers 401. Alpine 3.24 ships
// apk-tools 3.0; see NETRC in `apk(8)`. Hosts are matched by name only, so a
// stanza covers a mirror on any port.
//
// It is a *dagger.Secret rather than a string, and is mounted rather than
// written, so the credentials stay out of the cache key, out of argv, out of
// the image's environment and out of any layer a caller exports. That is also
// why the credentials are not simply userinfo in the WithApkRepository URL,
// which would put them in /etc/apk/repositories and in every apk error
// message that quotes it.
func (p *Pdf) WithApkAuth(
	// netrc-formatted credentials for the configured repositories.
	credentials *dagger.Secret,
) *Pdf {
	out := p.clone()
	out.ApkAuth = credentials
	return out
}

// WithFonts adds a directory of font files to the image, which is the only way
// to reach a face Alpine has no package for. It is this module's WithTessdata.
//
// It matters for exactly the reason the packaged font does. Poppler draws a
// PDF that names a font without embedding it by asking fontconfig for a
// substitute, and a substitute it cannot find is not an error: the glyphs are
// simply absent from the rendered page, and the render succeeds. A document
// that names a face outside the base-14 set — a corporate font, a CJK family
// — therefore renders wrong rather than failing, and supplying the face here
// is the fix.
//
// The directory is mounted under /usr/share/fonts, the one tree
// /etc/fonts/fonts.conf tells fontconfig to scan, so the faces in it are
// indistinguishable from packaged ones as far as poppler is concerned. It is
// mounted rather than copied so a large family does not become an image layer.
func (p *Pdf) WithFonts(
	// Directory of font files to make available for substitution.
	dir *dagger.Directory,
) *Pdf {
	out := p.clone()
	out.Fonts = dir
	return out
}

// Container returns the assembled toolchain image. This is the escape hatch
// for everything this module does not wrap: poppler-utils ships thirteen
// binaries and nine of them are wrapped here, so pdfimages, pdfattach,
// pdfdetach and pdftops stay reachable via `container with-exec` — as do the
// flags of a wrapped tool that this module does not surface, `pdffonts -subst`
// among them.
func (p *Pdf) Container() *dagger.Container {
	ctr := p.base().WithExec([]string{"apk", "add", "--no-cache", popplerPkg, fontPkg})
	// Mounted after the install because the install is what brings
	// fontconfig, and because a mount applied before it would be shadowed by
	// nothing the package manager writes there anyway.
	if p.Fonts != nil {
		ctr = ctr.WithMountedDirectory(fontsDir, p.Fonts)
	}
	return ctr
}

// Version returns the poppler release the assembled image ships, as the bare
// version number reported by `pdftotext -v`.
func (p *Pdf) Version(ctx context.Context) (string, error) {
	// poppler's tools print their version banner on stderr, not stdout, and
	// still exit 0. Reading stdout here would return the empty string for
	// every image ever built.
	out, err := p.Container().
		WithExec([]string{"pdftotext", "-v"}).
		Stderr(ctx)
	if err != nil {
		return "", err
	}
	// The first line is `pdftotext version <version>`; the rest is copyright.
	first := strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
	return strings.TrimSpace(strings.TrimPrefix(first, "pdftotext version")), nil
}

// Document binds one PDF to the toolchain.
//
// The boundary input is a *dagger.File rather than a *dagger.Directory: a PDF
// resolves nothing relative to its own location, so one file is the whole unit
// of work however many pages it carries.
func (p *Pdf) Document(
	// The PDF to read or render.
	source *dagger.File,
) *Document {
	return &Document{Pdf: p, Source: source}
}

func (p *Pdf) image() string {
	return fmt.Sprintf("%s/%s:%s", p.Registry, alpineImagePath, p.AlpineTag)
}

// clone copies the module's configuration for a builder method, deep enough
// that the copy's slices are its own: a builder that appended in place would
// let a second call off the same value overwrite the first one's addition.
func (p *Pdf) clone() *Pdf {
	out := *p
	out.ApkRepositories = append([]string(nil), p.ApkRepositories...)
	out.ApkKeys = append([]*dagger.File(nil), p.ApkKeys...)
	return &out
}

// base is the module's Alpine image with package installation configured, and
// is what the toolchain's `apk add` starts from.
func (p *Pdf) base() *dagger.Container {
	return p.withApkConfig(dag.Container().From(p.image()))
}

// withApkConfig applies the caller's repositories, keys and credentials to a
// container, immediately before the `apk add` that reads them.
//
// Each half is applied only when it was asked for. That is not tidiness: with
// none of the options set the container is byte-identical to the one this
// module assembles without them, so a caller who never touches these options
// sees no cache-key churn from their existence.
func (p *Pdf) withApkConfig(ctr *dagger.Container) *dagger.Container {
	if len(p.ApkRepositories) > 0 {
		ctr = ctr.WithNewFile(apkRepositoriesFile, strings.Join(p.ApkRepositories, "\n")+"\n")
	}
	if len(p.ApkKeys) > 0 {
		ctr = ctr.WithFiles(apkKeysDir, p.ApkKeys)
	}
	if p.ApkAuth != nil {
		ctr = ctr.
			WithMountedSecret(apkNetrcPath, p.ApkAuth).
			WithEnvVariable(apkNetrcEnv, apkNetrcPath)
	}
	return ctr
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
