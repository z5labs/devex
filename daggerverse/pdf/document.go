package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"dagger/pdf/internal/dagger"
)

// Document is one PDF bound to the toolchain, together with what is needed to
// open it and how much of it to look at.
//
// It is immutable: every With* returns a copy, so one bound document branches
// into several outputs without the branches interfering — a text extraction and
// a render off the same Document, or two page ranges of it, are independent.
type Document struct {
	// +private
	Pdf *Pdf
	// +private
	Source *dagger.File
	// +private
	UserPassword *dagger.Secret
	// +private
	OwnerPassword *dagger.Secret
	// +private
	FirstPage int
	// +private
	LastPage int
	// HasRange distinguishes a range that was set from one that was not, which
	// a zero FirstPage cannot do on its own: WithPageRange(0, 5) is a caller
	// error to be reported, and no WithPageRange at all is the whole document.
	// +private
	HasRange bool
}

// WithUserPassword supplies the password an encrypted document is opened with.
//
// This is the *user* password — the one that grants reading. A document
// encrypted at all needs one of these two passwords before any of poppler's
// tools will look at it, and without one they report `Incorrect password` and
// exit non-zero, having tried the empty password on the caller's behalf.
//
// It is a *dagger.Secret rather than a string, and reaches the tool as an
// environment variable the shell expands rather than as a `-upw` argv word,
// because argv is visible in every Dagger trace and in every error message
// quoting the failed command. Same reasoning as mounting apk credentials as a
// netrc file rather than putting userinfo in a repository URL.
//
// Only the first 32 bytes of the password are used. That is poppler's limit and
// not this module's: it truncates a password to the 32 bytes PDF revisions
// before 2.0 allowed, even for an AES-256 document whose own limit is 127. A
// document encrypted under a longer password by a tool that honours the full
// length — qpdf does — is therefore one poppler reports `Incorrect password`
// for no matter what is passed here, and no argument to this function can open
// it. Truncating the password before it reaches this module makes no difference
// for the same reason.
func (d *Document) WithUserPassword(
	// The document's user password.
	password *dagger.Secret,
) *Document {
	out := d.clone()
	out.UserPassword = password
	return out
}

// WithOwnerPassword supplies the document's owner password, which opens an
// encrypted document as fully as the user password does and additionally
// clears the permission bits poppler otherwise honours.
//
// Reach for it when a document permits opening but forbids extraction: the
// user password gets a reader past the encryption and then leaves
// `copy:no` in force, and the owner password is what a caller entitled to
// ignore that has. Setting both is fine and is what an owner-held document
// carrying a separate reading password needs.
//
// Poppler's 32-byte password limit applies here too. See WithUserPassword.
func (d *Document) WithOwnerPassword(
	// The document's owner password.
	password *dagger.Secret,
) *Document {
	out := d.clone()
	out.OwnerPassword = password
	return out
}

// WithPageRange narrows every conversion to a span of pages.
//
// Bounds are 1-based and inclusive, matching poppler's own `-f` and `-l`, so
// WithPageRange(2, 3) is the second and third pages and WithPageRange(1, 1) is
// the first page alone. A zero last means "to the last page", which is the
// only way to spell an open-ended range without knowing the page count.
//
// The bounds are checked against the document rather than handed straight to
// poppler, and the check is deferred to the conversion that reads them: it
// needs the page count, which needs the document opened, which needs the
// passwords this builder cannot see the future of. A first page past the end of
// the document is therefore reported by name, at the point of use, instead of
// silently rendering nothing.
//
// It narrows the *text* outputs and the *raster* outputs alike. It does not
// narrow Info or PageCount, which report on the document rather than on a
// conversion of it.
func (d *Document) WithPageRange(
	// First page to convert, 1-based and inclusive.
	first int,
	// Last page to convert, 1-based and inclusive. Zero means the last page of
	// the document.
	last int,
) *Document {
	out := d.clone()
	out.FirstPage = first
	out.LastPage = last
	out.HasRange = true
	return out
}

// Info returns everything pdfinfo reports about the document: its page count,
// its page size in points and in a named paper size, its PDF version, whether
// it is encrypted and under which algorithm, and the metadata it carries.
//
// It describes the whole document regardless of any WithPageRange, because a
// page range is a property of a conversion and this is not one.
func (d *Document) Info(ctx context.Context) (string, error) {
	out, err := d.run(ctx, "Info", d.command("pdfinfo", nil, sourcePath))
	if err != nil {
		return "", err
	}
	return strings.TrimRight(out.stdout, "\n"), nil
}

// PageCount returns how many pages the document has, by reading the `Pages:`
// line out of what Info reports.
//
// It is the page count of the whole document, not of a WithPageRange — which
// is what makes it usable as the bound a range is checked against.
func (d *Document) PageCount(ctx context.Context) (int, error) {
	info, err := d.Info(ctx)
	if err != nil {
		return 0, err
	}
	count, err := parsePageCount(info)
	if err != nil {
		return 0, fmt.Errorf("PageCount: %s", err.Error())
	}
	return count, nil
}

// Fonts returns what pdffonts reports about every face the document's pages
// name: the face's name, its type, its encoding, whether it is embedded in the
// file, whether it is a subset, whether it carries a ToUnicode map, and the
// object it lives in.
//
// The `emb` column is the one to read. A PDF that names a font without
// embedding one — the usual shape for anything not born as a scan — is drawn by
// asking fontconfig for a substitute, and a substitute poppler cannot find is
// not an error: the glyphs are simply absent from the rendered page and the
// command exits 0. This report is what that failure looks like before it
// happens, which makes it the diagnostic to reach for when a render comes out
// blank or a face comes out wrong. `no` in that column means the render depends
// on the image's own fonts — the packaged family, or one supplied through
// WithFonts.
//
// The report is pdffonts' own table, verbatim. Parsing it into structured
// values is deliberately not this module's job: the format is stable but wide,
// and a caller who wants one column can read it out of these lines more cheaply
// than this module can model all of them.
//
// WithPageRange narrows it, because which faces a document needs is a question
// about pages: a face used only on page 40 is absent from a report of pages 1
// through 3. Which substitute poppler would actually choose for an unembedded
// face is a different question, answered by `pdffonts -subst`, and reachable
// through Container.
func (d *Document) Fonts(ctx context.Context) (string, error) {
	if err := d.validateRange(ctx); err != nil {
		return "", err
	}
	out, err := d.run(ctx, "Fonts", d.command("pdffonts", d.rangeArgs(), sourcePath))
	if err != nil {
		return "", err
	}
	return strings.TrimRight(out.stdout, "\n"), nil
}

// Metadata returns the document's XMP packet: the RDF/XML block a producer
// writes its own record of the document into — title, creator tool, rights,
// modification history — and the one place a workflow's own custom properties
// can live inside a PDF.
//
// It is the packet and nothing else, which is what makes it worth having next to
// Info. The two carry different things and disagree in the wild: Info reports the
// PDF's Info dictionary, whose handful of keys a producer may have left behind
// when it rewrote the XMP, and the packet is the record a downstream asset
// system actually reads. The packet is returned exactly as it sits in the file,
// so a caller can hand it to an XML parser or grep a property out of it.
//
// A document carrying no packet is not an error and does not return nothing:
// poppler prints nothing at all and exits 0, and an empty string is
// indistinguishable from a function that never ran. It returns a line saying
// there is no XMP and naming Info as the report that does carry this document's
// metadata.
//
// WithPageRange does not narrow it, the packet being a property of the document
// rather than of any page — the same reason it does not narrow Info.
func (d *Document) Metadata(ctx context.Context) (string, error) {
	out, err := d.run(ctx, "Metadata", d.command("pdfinfo", []string{metadataFlag}, sourcePath))
	if err != nil {
		return "", err
	}
	packet := strings.TrimRight(out.stdout, "\n")
	if strings.TrimSpace(packet) == "" {
		return noMetadataReport, nil
	}
	return packet, nil
}

// Signatures returns what pdfsig reports about the document's digital
// signatures: one block per signature naming its field, its signer, when it was
// signed, which algorithm signed it, how much of the document it covers, and
// whether the signature validates.
//
// A document with no signatures — which is nearly every document — gets the
// report saying so rather than an error. That is the point of the function
// being callable on an arbitrary PDF: "is this signed, and does it check out" is
// a question asked *before* the answer is known, so the unsigned answer has to
// come back as a result. poppler exits 2 for it, and this is the one place the
// module reads a non-zero exit as a report rather than a failure.
//
// It reports and does not gate. A signature that fails to validate is a report
// saying so — `Signature Validation: Signature is Invalid.` — and not an error,
// for the same reason: a caller asking whether the signatures hold needs the
// answer, and one that wants to stop on a bad signature reads the verdict out of
// the report. Only a run that got no report at all — an unreadable file, a
// document whose password is missing or wrong — fails.
//
// What the report can say about a signer's *certificate* is limited by the image
// rather than by the document. Certificate validation needs a trust database,
// this image carries none, and pdfsig says so on stderr — which is not part of
// this report. A caller who needs a validated chain supplies one with
// `-nssdir`, through Container.
//
// Neither WithPageRange nor anything else narrows it: a signature covers byte
// ranges of the file rather than pages, and pdfsig has no page bounds at all.
// The passwords do reach it, an encrypted document being one it has to open like
// any other.
func (d *Document) Signatures(ctx context.Context) (string, error) {
	res, code, err := d.capture(ctx, d.command("pdfsig", nil, sourcePath))
	if err != nil {
		return "", err
	}
	report := strings.TrimRight(res.stdout, "\n")
	// A non-zero exit that printed a report is an answer about the document —
	// unsigned, or signed by something that did not validate — and only a
	// non-zero exit that printed nothing is a failed run.
	if code != 0 && !isSignatureReport(report) {
		return "", d.failure("Signatures", code, res.stdout, res.stderr)
	}
	return report, nil
}

// isSignatureReport reports whether pdfsig got far enough to say something about
// the document, which is what separates its two meanings of a non-zero exit.
//
// It matches on what was printed rather than on the code, because the codes are
// ambiguous where the output is not: pdfsig exits 1 both for a document it could
// not open — nothing on stdout — and for one whose signature did not validate,
// having printed the whole report first.
func isSignatureReport(out string) bool {
	return strings.Contains(out, noSignaturesMarker) || strings.Contains(out, signatureInfoMarker)
}

// Convert opens the conversion namespace: the render options, and the nine
// outputs that read them.
//
// It is a separate object rather than more methods on Document because the
// render options belong to a conversion and not to the document — WithDpi says
// nothing about the PDF — and because it is what lets one bound document, with
// its passwords and its page range settled, fan out into several differently
// configured conversions.
func (d *Document) Convert() *Convert {
	return &Convert{Document: d}
}

// clone copies the document for a builder method. Every field is a value or a
// handle, so the shallow copy is the deep one.
func (d *Document) clone() *Document {
	out := *d
	return &out
}

// container is the toolchain image with the document mounted and its passwords
// placed in the environment, ready for any of poppler's tools.
func (d *Document) container() *dagger.Container {
	ctr := d.Pdf.Container().
		WithMountedFile(sourcePath, d.Source).
		WithWorkdir(workDir)
	if d.UserPassword != nil {
		ctr = ctr.WithSecretVariable(userPasswordEnv, d.UserPassword)
	}
	if d.OwnerPassword != nil {
		ctr = ctr.WithSecretVariable(ownerPasswordEnv, d.OwnerPassword)
	}
	return ctr
}

// passwordArgs returns the flags naming the document's passwords. The value of
// each is a shell parameter expansion rather than the password itself, which is
// the whole point: the command line carries the variable's name and the
// contents never leave the process environment.
func (d *Document) passwordArgs() []string {
	var args []string
	if d.UserPassword != nil {
		args = append(args, "-upw", `"$`+userPasswordEnv+`"`)
	}
	if d.OwnerPassword != nil {
		args = append(args, "-opw", `"$`+ownerPasswordEnv+`"`)
	}
	return args
}

// hasPassword reports whether the caller supplied either password, which is
// what distinguishes "encrypted and you gave nothing" from "encrypted and what
// you gave was wrong" in the message a failure carries.
func (d *Document) hasPassword() bool {
	return d.UserPassword != nil || d.OwnerPassword != nil
}

// rangeArgs returns the page-bound flags, omitting either bound the caller left
// open. An absent `-f` is page one and an absent `-l` is the last page, which
// is exactly what an unset bound means here.
func (d *Document) rangeArgs() []string {
	if !d.HasRange {
		return nil
	}
	var args []string
	if d.FirstPage > 0 {
		args = append(args, "-f", strconv.Itoa(d.FirstPage))
	}
	if d.LastPage != endOfDocument {
		args = append(args, "-l", strconv.Itoa(d.LastPage))
	}
	return args
}

// validateRange checks a requested page range against the document, and is
// called by every conversion before it runs.
//
// The self-consistent bounds are checked first and without opening the
// document, so a caller who inverted the two learns that rather than learning
// their password is wrong. Only then is the page count fetched, and only when
// a range was actually requested — a caller converting a whole document pays
// nothing for this.
func (d *Document) validateRange(ctx context.Context) error {
	if !d.HasRange {
		return nil
	}
	if d.FirstPage < 1 {
		return fmt.Errorf(
			"WithPageRange: first must be at least 1, got %d: pages are numbered from 1",
			d.FirstPage)
	}
	if d.LastPage < endOfDocument {
		return fmt.Errorf(
			"WithPageRange: last must not be negative, got %d: pass a page number, or 0 for the last page of the document",
			d.LastPage)
	}
	if d.LastPage != endOfDocument && d.LastPage < d.FirstPage {
		return fmt.Errorf(
			"WithPageRange: last (%d) must not precede first (%d)",
			d.LastPage, d.FirstPage)
	}

	count, err := d.PageCount(ctx)
	if err != nil {
		return err
	}
	if d.FirstPage > count {
		return fmt.Errorf(
			"WithPageRange: first (%d) is past the end of the document, which has %d pages",
			d.FirstPage, count)
	}
	if d.LastPage > count {
		return fmt.Errorf(
			"WithPageRange: last (%d) is past the end of the document, which has %d pages",
			d.LastPage, count)
	}
	return nil
}

// command assembles a poppler invocation as a single shell command.
//
// It is a shell command rather than a bare argv because that is what lets a
// password reach the tool as `"$PDF_USER_PASSWORD"` — expanded by the shell out
// of the environment, so the secret is never a word on any command line. Every
// word joined here is generated by this module: a fixed path, a flag from a
// table, or an integer, so there is nothing for the join to quote.
func (d *Document) command(tool string, flags []string, trailing ...string) string {
	words := append([]string{tool}, flags...)
	words = append(words, d.passwordArgs()...)
	words = append(words, trailing...)
	return strings.Join(words, " ")
}

// popplerResult is a finished invocation's output, kept together because
// poppler splits its own messages across both streams: progress and content go
// to stdout, and every diagnostic — including the one that says the document is
// encrypted — goes to stderr.
type popplerResult struct {
	container *dagger.Container
	stdout    string
	stderr    string
}

// run executes a poppler invocation with no arguments of its own.
func (d *Document) run(ctx context.Context, label string, command string) (*popplerResult, error) {
	return d.runScript(ctx, label, command)
}

// runScript executes a shell script against the bound document and turns a
// non-zero exit into an error that says what actually went wrong. Anything
// after the script becomes `$0`, `$1` and so on, which is how a value the
// script needs reaches it without being interpolated into the script text.
func (d *Document) runScript(ctx context.Context, label string, script string, args ...string) (*popplerResult, error) {
	res, code, err := d.capture(ctx, script, args...)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, d.failure(label, code, res.stdout, res.stderr)
	}
	return res, nil
}

// capture runs a shell script against the bound document and returns its output
// and its exit code without judging either, which is what Signatures needs: for
// pdfsig a non-zero exit is as often an answer about the document as it is a
// failure to read it.
//
// Expect=ReturnTypeAny is what keeps a failed run on the value path so its
// output is still readable; without it the exit code is the error and poppler's
// own message about the document is lost inside Dagger's.
func (d *Document) capture(ctx context.Context, script string, args ...string) (*popplerResult, int, error) {
	return capture(ctx, d.container(), script, args...)
}

// capture is the same for a container that carries no bound document, which is
// what Merge runs in: its sources are several files and none of them is "the"
// document, so there is nothing for Document to be.
func capture(ctx context.Context, ctr *dagger.Container, script string, args ...string) (*popplerResult, int, error) {
	exec := ctr.WithExec(
		append([]string{"sh", "-c", script}, args...),
		dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny})

	code, err := exec.ExitCode(ctx)
	if err != nil {
		return nil, 0, err
	}
	stdout, err := exec.Stdout(ctx)
	if err != nil {
		return nil, 0, err
	}
	stderr, err := exec.Stderr(ctx)
	if err != nil {
		return nil, 0, err
	}
	return &popplerResult{container: exec, stdout: stdout, stderr: stderr}, code, nil
}

// failure builds the error a non-zero exit becomes.
//
// An encrypted document gets its own message. poppler reports every wrong
// password the same way — `Incorrect password`, whether one was given or not,
// because it tries the empty password when given none — so passing that
// through would tell a caller who supplied nothing that what they supplied was
// wrong. Naming the encryption and the builder that answers it is the
// difference between a message a caller can act on and one they have to
// interpret.
func (d *Document) failure(label string, code int, stdout, stderr string) error {
	out := strings.TrimSpace(strings.TrimSpace(stdout) + "\n" + strings.TrimSpace(stderr))
	if strings.Contains(stderr, incorrectPasswordMarker) {
		if d.hasPassword() {
			return fmt.Errorf(
				"%s: the document is encrypted and the supplied password did not open it: check the password passed to WithUserPassword or WithOwnerPassword\n%s",
				label, out)
		}
		return fmt.Errorf(
			"%s: the document is encrypted and no password was supplied: pass the document's password to WithUserPassword, or its owner password to WithOwnerPassword\n%s",
			label, out)
	}
	return fmt.Errorf("%s failed (exit %d):\n%s", label, code, out)
}

// parsePageCount pulls the page count out of pdfinfo's report, which spells it
// as a `Pages:` line among the rest of the document's properties.
func parsePageCount(info string) (int, error) {
	const key = "Pages:"
	for _, line := range strings.Split(info, "\n") {
		if !strings.HasPrefix(line, key) {
			continue
		}
		field := strings.TrimSpace(strings.TrimPrefix(line, key))
		count, err := strconv.Atoi(field)
		if err != nil {
			return 0, fmt.Errorf("pdfinfo reported an unreadable page count %q", field)
		}
		return count, nil
	}
	return 0, fmt.Errorf("pdfinfo reported no page count:\n%s", info)
}
