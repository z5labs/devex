package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A document is a claim, and this is what makes the claim checkable.
//
// Some documents cannot disagree with what they describe: `dag.Go().Spdx`
// derives an SBOM from the compiled binary through debug/buildinfo, so the
// document and the artifact come from one read. Every other document this
// module accepts is asserted — a caller's SPDX for a certificate bundle, a
// vendor's for a prebuilt CLI — and an asserted document about the wrong bytes
// is well-formed, attaches cleanly, and is indistinguishable from a right one.
// That is the concern devex#409 left open and devex#410 closes.
//
// The mitigation is deliberately the cheapest one that is worth anything:
// **require the document to name the SHA-256 of the content it accompanies,
// and verify it.** It applies to every contribution by the same rule — the
// executable a variant is built around, a contributed file, a contributed tree
// — and it applies to `dag.Go().Spdx` too, which already names the binary's
// digest, so the paved path needs no exception.
//
// # What it establishes, and what it does not
//
// It establishes that the document is about *these* bytes. It does not
// establish that the components listed inside it are the ones really in those
// bytes: nothing here can re-derive an arbitrary ecosystem's dependency graph,
// and a module that pretended to would be inventing an answer. So a supplier
// can still overstate what is inside their own artifact. What they can no
// longer do is hand over a document about something else entirely — which was
// the case nothing downstream could even ask about, because the document named
// no digest to compare and the image named no document to compare it to.
//
// # Why the check runs at publish rather than at the contributing call
//
// Verifying means hashing the content, and hashing means resolving it. A
// language chain's contribution is a binary that has not been compiled yet, so
// a check at the contributing call would force every platform's cross-compile
// inside App — turning a constructor that costs nothing into one that costs a
// full build, and taking with it the property that "an app nobody publishes
// costs no scan".
//
// resolveContributions is where it goes instead: the one place that already
// reads and parses every document, before the first byte is pushed, for
// exactly the reason this check exists. It is not a weaker position. There is
// no route to a registry that does not pass through it, and everything
// fallible about assembly failing in one call is what keeps a half-attested
// image from being possible at all.
//
// # A directory's digest is this module's convention
//
// A file has one obvious digest and a tree does not, so the digest a directory
// contribution's document must name is the one Z5labs.DirectoryDocument
// computes: the SHA-256 over the sorted list of the tree's paths and their
// SHA-256s, which is treeDigest. It is stable, it is a pure function of the
// tree, and two directories that differ anywhere differ in it. A document
// produced somewhere else with some other notion of a tree checksum will be
// refused, and the refusal says what was expected — which is the right outcome
// for a value nothing else could interpret.

// verifyContributionDigest refuses a contribution whose document is about
// bytes other than the ones entering the image.
//
// The two failures are kept apart because they send the reader to different
// places: a document that names no digest is a document that cannot be checked
// at all, usually produced by hand or by a tool that does not fill the field,
// while a digest that disagrees is a document about the wrong artifact.
func verifyContributionDigest(ctx context.Context, c contribution, subject bomPackage) error {
	want := strings.ToLower(strings.TrimSpace(subject.Sha256))
	if want == "" {
		return fmt.Errorf(
			"names no SHA-256 for what it describes, so nothing connects it to the bytes in the image; "+
				"a contribution's document has to name the digest of the content it accompanies, and %s produces one that does",
			documentHelperFor(c))
	}
	got, err := contentDigest(ctx, c)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf(
			"describes other bytes: it names SHA-256 %s and what entered the image hashes to %s",
			want, got)
	}
	return nil
}

// documentHelperFor names the helper that would have produced a checkable
// document for this contribution, so the refusal ends somewhere useful rather
// than in a restatement of the rule.
func documentHelperFor(c contribution) string {
	if c.Tree != nil {
		return "directoryDocument"
	}
	return "fileDocument"
}

// contentDigest is the digest a contribution's document has to name: the
// SHA-256 of a file's bytes, or this module's tree digest for a directory.
//
// The content is exported to the module's own filesystem and hashed there,
// which is the runtime-I/O pattern the rest of this module uses and is the
// same thing FileDocument and DirectoryDocument already do to *produce* a
// document. Reading the digest off Dagger instead is not an option: a
// Directory's or File's Dagger digest identifies a node in the build graph and
// is not the SHA-256 of anything an SPDX document could name.
func contentDigest(ctx context.Context, c contribution) (string, error) {
	work, err := os.MkdirTemp("", "z5labs-verify-*")
	if err != nil {
		return "", fmt.Errorf("create work dir: %v", err)
	}
	defer os.RemoveAll(work)
	path := filepath.Join(work, "content")

	switch {
	case c.Content != nil:
		if _, err := c.Content.Export(ctx, path); err != nil {
			return "", fmt.Errorf("read the contributed bytes to check the document against them: %v", err)
		}
		sum256, _, err := fileDigests(path)
		if err != nil {
			return "", fmt.Errorf("hash the contributed bytes: %v", err)
		}
		return sum256, nil
	case c.Tree != nil:
		if _, err := c.Tree.Export(ctx, path); err != nil {
			return "", fmt.Errorf("read the contributed tree to check the document against it: %v", err)
		}
		files, err := walkTree(path)
		if err != nil {
			return "", fmt.Errorf("hash the contributed tree: %v", err)
		}
		if len(files) == 0 {
			return "", fmt.Errorf("holds no files, so there is nothing for a document to describe")
		}
		return treeDigest(files), nil
	}
	// Unreachable through the public API: every method that appends a
	// contribution carries the content beside the document. It is an error
	// rather than a silent pass because the alternative is a way for a future
	// contribution path to opt out of the check by forgetting to fill a
	// field, which is precisely the kind of hole this file exists to close.
	return "", fmt.Errorf(
		"entered the image with no content recorded beside its document, so the document cannot be checked against it; " +
			"this is a defect in the module rather than in the contribution")
}
