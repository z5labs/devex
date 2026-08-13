package main

import (
	"context"
	"crypto/sha1" //nolint:gosec // SPDX 2.3 mandates SHA-1 for analyzed files and for the package verification code
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spdx/tools-golang/spdx/v2/common"
	"github.com/spdx/tools-golang/spdx/v2/v2_3"

	"dagger/z-5-labs/internal/dagger"
)

// Content with no ecosystem still has to arrive described.
//
// A Go binary has a module graph, so `daggerverse/go` can produce a document
// about it without asking the caller anything. A CA bundle, a template
// directory or an HTML tree has none of that: no module graph, no manifest,
// no licence text in a place anything knows to look. The party contributing
// it is the only one who knows what it is.
//
// The risk that shapes these two functions is not that such content is
// undescribable — SPDX describes a file as a package with a checksum and
// NOASSERTION, which is exact and honest. The risk is **burden**. If
// producing a document for a certificate bundle is a chore, callers will
// supply whatever satisfies the signature, and a worthless document is worse
// than an absent one because nothing downstream can tell the two apart. So
// the module supplies the helper, it computes everything it can compute, and
// the only thing it asks the caller for is the one thing it cannot know.
//
// # What it asks for, and what it refuses to guess
//
// A licence, optionally. Nothing else. Names come from the content, digests
// are computed, and the supplier is NOASSERTION because a path is not an
// organisation.
//
// A licence that is *not* given becomes NOASSERTION in both formats, with a
// comment saying no licence was stated. That is deliberately distinguishable
// from a resolved one: `daggerverse/go` publishes a classifier's finding, and
// a caller's silence must not read the same way as an analysis that ran.
//
// A licence that *is* given becomes both the declared and the concluded
// licence. This is the one place the two differ from the go module's
// treatment, and the reason is that the roles differ. There, a classifier's
// guess is discounted until it covers essentially the whole file, because the
// classifier is a third party inferring what the module meant. Here the
// contributor *is* the supplier of the bytes and the author of the document,
// so there is no third-party inference to discount — declaring one thing and
// concluding another would express a doubt nobody holds.
//
// The value is checked against the SPDX licence-expression charset rather
// than being passed through. That does not make it a valid expression, and
// it is not presented as validation; it is there so that a sentence of prose
// cannot be published in a field a consumer reads as an identifier.

// licenseExpressionChars is the charset of an SPDX licence expression:
// identifiers, the WITH/AND/OR operators, parentheses, and the ":" that
// LicenseRef- and DocumentRef- carry.
const licenseExpressionChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-+()_: "

// FileDocument produces the SPDX document required to contribute file to an
// image, for content whose ecosystem has no module able to produce one.
//
// The document describes exactly one package — the file — with the SHA-256
// of its bytes. Files are not enumerated inside it, because the package is
// one file and a file list would restate the checksum that is already there;
// what the image-level document then contains is the file, named and hashed.
//
// license is an SPDX licence expression such as MIT, Apache-2.0 or
// "Apache-2.0 WITH LLVM-exception". Leaving it out publishes NOASSERTION,
// which is the honest answer and is not the same answer as a licence that was
// looked for and not found.
//
// name overrides how the contribution is identified in the image's document.
// It defaults to the file's own name, which is usually right; supply one when
// the file's name is a build artifact rather than a description of it.
//
// version is what the contributed content is a version of. There is no
// default: content with no ecosystem usually has no version, and inventing
// one would put a value in a field consumers compare.
//
// +cache="session"
func (m *Z5labs) FileDocument(
	ctx context.Context,
	// The file the document describes.
	file *dagger.File,
	// An SPDX licence expression for the content, e.g. MIT or Apache-2.0.
	//
	// +optional
	license string,
	// How the contribution is named in the image's document. Defaults to
	// the file's own name.
	//
	// +optional
	name string,
	// The version of the contributed content, if it has one.
	//
	// +optional
	version string,
) (*dagger.File, error) {
	if file == nil {
		return nil, fmt.Errorf("fileDocument requires a file to describe")
	}
	if err := validateLicenseExpression(license); err != nil {
		return nil, err
	}
	if name == "" {
		fileName, err := file.Name(ctx)
		if err != nil {
			return nil, fmt.Errorf("read the file's name: %v", err)
		}
		name = fileName
	}
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("fileDocument requires a name: the file has none, so pass one")
	}

	work, err := os.MkdirTemp("", "z5labs-document-*")
	if err != nil {
		return nil, fmt.Errorf("create work dir: %v", err)
	}
	defer os.RemoveAll(work)
	path := filepath.Join(work, "content")
	if _, err := file.Export(ctx, path); err != nil {
		return nil, fmt.Errorf("export %s: %v", name, err)
	}
	sum256, _, err := fileDigests(path)
	if err != nil {
		return nil, err
	}

	return renderContributionDocument(bomPackage{
		Name:       name,
		Version:    version,
		Sha256:     sum256,
		Supplier:   noAssertion,
		Purpose:    "FILE",
		SourceInfo: "contributed to the image as a file; no ecosystem metadata was available to describe it further",
	}, license)
}

// DirectoryDocument produces the SPDX document required to contribute dir to
// an image, for content whose ecosystem has no module able to produce one.
//
// Unlike FileDocument this one enumerates: the package it describes carries
// one SPDX file element per file in the tree, each with its SHA-256 and the
// SHA-1 the format's verification code is computed from. A directory summed
// as a single opaque blob would satisfy "the contribution is described" and
// leave "every file in the image is accounted for" false one level down,
// which is the same failure this whole mechanism exists to close.
//
// The package's own checksum is a digest over the sorted list of the tree's
// paths and their digests, so two directories with the same name and version
// and different contents are different packages rather than one.
//
// A tree carrying a symbolic link is **refused**, and so is one carrying a
// device, a pipe or a socket. A link is not bytes and cannot be enumerated,
// digested or given the mode the module sets, so a document that skipped it
// would describe a tree the image does not have — see the "A symbolic link is
// not content" section in contribute.go. The refusal names the link and what
// it points at, so it can be found and replaced with the thing itself.
//
// name is required, because a directory has no name of its own to fall back
// to. See FileDocument for license and version.
//
// +cache="session"
func (m *Z5labs) DirectoryDocument(
	ctx context.Context,
	// The directory the document describes.
	dir *dagger.Directory,
	// How the contribution is named in the image's document.
	name string,
	// An SPDX licence expression for the content, e.g. MIT or Apache-2.0.
	//
	// +optional
	license string,
	// The version of the contributed content, if it has one.
	//
	// +optional
	version string,
) (*dagger.File, error) {
	if dir == nil {
		return nil, fmt.Errorf("directoryDocument requires a directory to describe")
	}
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("directoryDocument requires a name for the contribution")
	}
	if err := validateLicenseExpression(license); err != nil {
		return nil, err
	}

	work, err := os.MkdirTemp("", "z5labs-document-*")
	if err != nil {
		return nil, fmt.Errorf("create work dir: %v", err)
	}
	defer os.RemoveAll(work)
	root := filepath.Join(work, "content")
	if _, err := dir.Export(ctx, root); err != nil {
		return nil, fmt.Errorf("export %s: %v", name, err)
	}
	files, err := walkTree(root)
	if err != nil {
		// A refused entry is not a failure to read the tree, and sending it
		// to the reader as one would put "read /srv/templates:" in front of a
		// sentence that already says exactly what is wrong and where.
		var bad *unsupportedEntry
		if errors.As(err, &bad) {
			return nil, fmt.Errorf("%s cannot be described: %v", name, err)
		}
		return nil, fmt.Errorf("read %s: %v", name, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%s holds no files, so there is nothing for a document to describe", name)
	}

	return renderContributionDocument(bomPackage{
		Name:       name,
		Version:    version,
		Sha256:     treeDigest(files),
		Supplier:   noAssertion,
		Purpose:    "FILE",
		SourceInfo: "contributed to the image as a directory; no ecosystem metadata was available to describe it further",
		Files:      files,
	}, license)
}

// renderContributionDocument writes the one-package SPDX document both
// helpers produce. It goes through the same spdxPackage the assembler uses,
// so a helper-produced document and an assembled one are the same shape and
// the parser has one case to handle.
func renderContributionDocument(pkg bomPackage, license string) (*dagger.File, error) {
	raw, err := contributionDocument(pkg, license)
	if err != nil {
		return nil, err
	}
	return writeWorkdirFile(sanitizeDocumentName(pkg.Name)+".spdx.json", raw)
}

// contributionDocument is renderContributionDocument without the module
// boundary, so the rendering can be exercised without a Dagger call.
func contributionDocument(pkg bomPackage, license string) ([]byte, error) {
	pkg.LicenseDeclared = noAssertion
	pkg.LicenseConcluded = noAssertion
	if strings.TrimSpace(license) != "" {
		pkg.LicenseDeclared = strings.TrimSpace(license)
		pkg.LicenseConcluded = strings.TrimSpace(license)
	} else {
		pkg.LicenseComment = "the contributor stated no licence for this content; " +
			"NOASSERTION here means nothing was said, not that a licence was looked for and not found"
	}

	id := pkg.elementID()
	files, contains := spdxFileElements(pkg, id)
	doc := &v2_3.Document{
		SPDXVersion:       spdxVersion,
		DataLicense:       spdxDataLicense,
		SPDXIdentifier:    common.ElementID(spdxDescribesFromDocDoc),
		DocumentName:      pkg.Name,
		DocumentNamespace: documentNamespaceBase + "contribution/" + sha1Hex(pkg.key()),
		CreationInfo: &v2_3.CreationInfo{
			Creators: []common.Creator{
				{CreatorType: "Tool", Creator: imageDocumentCreator},
				{CreatorType: "Organization", Creator: creatorOrganization},
			},
			Created: time.Now().UTC().Format(time.RFC3339),
		},
		Packages: []*v2_3.Package{spdxPackage(pkg, id)},
		Files:    files,
		Relationships: append([]*v2_3.Relationship{{
			RefA:         common.MakeDocElementID("", spdxDescribesFromDocDoc),
			RefB:         common.MakeDocElementID("", id),
			Relationship: "DESCRIBES",
		}}, contains...),
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode document for %s: %v", pkg.Name, err)
	}
	return append(raw, '\n'), nil
}

// sanitizeDocumentName reduces a contribution name to something safe to use
// as a file name. The name may be a path — a file is contributed at one —
// and writeWorkdirFile would otherwise create a directory tree from it.
func sanitizeDocumentName(name string) string {
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, name)
	out = strings.Trim(out, "-.")
	if out == "" {
		return "contribution"
	}
	return out
}

// validateLicenseExpression rejects a value that cannot be a licence
// expression at all.
//
// This is not a parser and does not claim the value is a valid expression —
// writing one would mean owning the SPDX expression grammar here. It is a
// charset check, and what it buys is that a sentence of prose, a URL or a
// paragraph of licence text cannot land in a field a consumer reads as an
// identifier and matches against a policy.
func validateLicenseExpression(license string) error {
	trimmed := strings.TrimSpace(license)
	if trimmed == "" {
		return nil
	}
	for _, r := range trimmed {
		if !strings.ContainsRune(licenseExpressionChars, r) {
			return fmt.Errorf(
				"%q is not an SPDX licence expression: it carries %q, and an expression is licence identifiers "+
					"joined by AND, OR and WITH — pass the identifier (MIT, Apache-2.0, LicenseRef-Custom) rather than licence text, "+
					"or leave it out to publish NOASSERTION",
				license, string(r))
		}
	}
	return nil
}

// walkTree lists every regular file under root with its digests, paths
// relative to root and sorted, so the document is a pure function of the
// tree.
//
// Anything that is neither a directory nor a regular file is **refused**, and
// a symbolic link is the case that matters. It used to be skipped, which made
// this walk describe a tree the image did not have: a link contributed nothing
// to the file list, so it was in no document, and — because treeDigest is a
// digest of that list — two trees differing only in their links had the same
// digest. See the "A symbolic link is not content" section in contribute.go
// for the decision and the measurements behind it.
//
// The refusal lives here because this is the one place the module reads a
// contributed tree, and both readers need it: Z5labs.DirectoryDocument, so an
// adopter on the paved path is refused at the call that produced the document,
// and contentDigest at publish time, so a caller who brought a document of
// their own is refused before the first byte is pushed.
func walkTree(root string) ([]bomFile, error) {
	var out []bomFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return unsupportedEntryAt(root, path, d)
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		sum256, sum1, err := fileDigests(path)
		if err != nil {
			return err
		}
		out = append(out, bomFile{Path: filepath.ToSlash(rel), Sha1: sum1, Sha256: sum256})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// unsupportedEntry is a tree entry that is neither a directory nor a regular
// file, and so cannot be contributed to an image.
//
// It is a type rather than a string because its two readers word the refusal
// differently — DirectoryDocument is answering "why can I not have a
// document?" and a publish is answering "why was my image refused?" — and
// because a message this specific must not be mistaken for the I/O errors the
// same walk can return.
type unsupportedEntry struct {
	// Path is relative to the tree's root, which is what a caller recognises;
	// the absolute one names a temporary directory inside this module.
	Path string
	// What the entry is, as a noun phrase read after the path.
	Kind string
	// Target is where a symbolic link points, verbatim. It is carried because
	// it is the whole content of the refusal for the case that matters: a
	// caller who did not know their tree held a link needs to be told what it
	// pointed at to find it.
	Target string
}

func (e *unsupportedEntry) Error() string {
	what := e.Path + " is " + e.Kind
	if e.Target != "" {
		what += " to " + strconv.Quote(e.Target)
	}
	return what + ", and that is not content this module can contribute: " +
		"it is copied into the image as it stands, the mode this module sets does not reach through it, the rules " +
		"that refuse a contribution landing on top of something never see the path it names, and it is in no " +
		"document and no digest — so a tree carrying one is indistinguishable from the same tree without it. " +
		"Contribute what it points at instead, at the path it should have in the image"
}

// unsupportedEntryAt describes the entry the walk refused.
//
// The link target is read with os.Readlink rather than resolved: what the
// refusal has to name is what the caller wrote, and resolving it here would
// resolve it against this module's own filesystem — which the measurement in
// contribute.go shows is not the filesystem the link will land in.
func unsupportedEntryAt(root, path string, d fs.DirEntry) error {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	bad := &unsupportedEntry{Path: filepath.ToSlash(rel), Kind: "an entry of a kind an image cannot carry"}
	switch {
	case d.Type()&fs.ModeSymlink != 0:
		bad.Kind = "a symbolic link"
		if target, err := os.Readlink(path); err == nil {
			bad.Target = target
		}
	case d.Type()&fs.ModeDevice != 0:
		bad.Kind = "a device node"
	case d.Type()&fs.ModeNamedPipe != 0:
		bad.Kind = "a named pipe"
	case d.Type()&fs.ModeSocket != 0:
		bad.Kind = "a socket"
	}
	return bad
}

// fileDigests returns a file's SHA-256 and SHA-1, streamed rather than read
// into memory: a contributed directory may hold something large and this
// runs in the module's own process.
func fileDigests(path string) (sum256, sum1 string, err error) {
	f, err := os.Open(path) //nolint:gosec // path is derived from this module's own temp dir
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	h256 := sha256.New()
	h1 := sha1.New() //nolint:gosec // SPDX 2.3 mandates SHA-1 here
	if _, err := io.Copy(io.MultiWriter(h256, h1), f); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(h256.Sum(nil)), hex.EncodeToString(h1.Sum(nil)), nil
}

// treeDigest is a content identity for a whole tree: the SHA-256 of every
// path and digest in it, sorted. It gives a directory package the checksum
// SPDX wants without claiming the tree is one blob anybody can hash.
func treeDigest(files []bomFile) string {
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s %s\n", f.Path, f.Sha256)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sha1Hex is the SHA-1 of s, which SPDX's package verification code is
// defined in terms of.
func sha1Hex(s string) string {
	sum := sha1.Sum([]byte(s)) //nolint:gosec // SPDX 2.3 defines the verification code as SHA-1
	return hex.EncodeToString(sum[:])
}
