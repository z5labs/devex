package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/spdx/tools-golang/spdx/v2/common"
	"github.com/spdx/tools-golang/spdx/v2/v2_3"
)

// spdxVersion and spdxDataLicense are fixed by the spec revision this
// module targets. The data licence is not a choice: SPDX requires
// CC0-1.0 for the document's own metadata.
const (
	spdxVersion     = "SPDX-2.3"
	spdxDataLicense = "CC0-1.0"
)

// documentNamespaceBase is the prefix of every document's namespace URI.
// SPDX requires the namespace to be unique per document; appending the
// artifact's own digest makes it unique without a random component, so
// two documents describing the same bytes are byte-identical apart from
// their timestamp.
const documentNamespaceBase = "https://z5labs.github.io/devex/spdxdocs/"

// spdxSubjectID is the element id of the package the document is about.
// It is a constant rather than derived so a consumer can find the subject
// without first parsing the relationships.
const spdxSubjectID = "SPDXRef-Binary"

// creatorTool is how the document names what produced it. SPDX wants a
// "Tool: name-version" string; the version is the module's own, which is
// the commit of this repository as far as a consumer is concerned.
const creatorTool = "daggerverse-go-sbom"

// spdxDocument renders the resolved graph as SPDX 2.3 JSON.
//
// Every element the NTIA minimum-elements guidance requires is populated
// rather than left to the library's zero value: supplier, component name,
// version, a unique identifier, the dependency relationships, the author
// of the SBOM data and the timestamp. A library can guarantee the syntax
// of a document; only the mapping can guarantee it says anything.
func (gr *graph) spdxDocument() ([]byte, error) {
	packages := []*v2_3.Package{gr.spdxSubjectPackage()}
	relationships := []*v2_3.Relationship{{
		RefA:         common.MakeDocElementID("", "DOCUMENT"),
		RefB:         common.MakeDocElementID("", spdxSubjectID),
		Relationship: "DESCRIBES",
	}}
	for _, comp := range gr.Components {
		packages = append(packages, comp.spdxPackage())
		relationships = append(relationships, &v2_3.Relationship{
			RefA:         common.MakeDocElementID("", spdxSubjectID),
			RefB:         common.MakeDocElementID("", comp.spdxIdentifier()),
			Relationship: "DEPENDS_ON",
		})
	}

	doc := &v2_3.Document{
		SPDXVersion:       spdxVersion,
		DataLicense:       spdxDataLicense,
		SPDXIdentifier:    common.ElementID("DOCUMENT"),
		DocumentName:      gr.BinaryName,
		DocumentNamespace: documentNamespaceBase + gr.BinaryName + "-" + gr.BinarySha256,
		CreationInfo: &v2_3.CreationInfo{
			Creators: []common.Creator{
				{CreatorType: "Tool", Creator: creatorTool},
				{CreatorType: "Organization", Creator: "z5labs"},
			},
			Created: gr.Created.Format(time.RFC3339),
		},
		Packages:      packages,
		Relationships: relationships,
	}
	return mustJSON(doc)
}

// spdxSubjectPackage is the package describing the built artifact. Its
// checksum is the artifact's own, which is what makes the document
// verifiable against the bytes rather than merely plausible beside them.
//
// FilesAnalyzed is false: the document enumerates the modules linked into
// the binary, not the files inside it, and SPDX requires a verification
// code whenever files were analyzed. Claiming otherwise would be claiming
// a file-level analysis that did not happen.
func (gr *graph) spdxSubjectPackage() *v2_3.Package {
	version := gr.MainVersion
	if version == "" {
		version = noAssertion
	}
	return &v2_3.Package{
		PackageName:               gr.BinaryName,
		PackageSPDXIdentifier:     common.ElementID(spdxSubjectID[len("SPDXRef-"):]),
		PackageVersion:            version,
		PackageSupplier:           &common.Supplier{SupplierType: "Organization", Supplier: "z5labs"},
		PackageDownloadLocation:   noAssertion,
		FilesAnalyzed:             false,
		IsFilesAnalyzedTagPresent: true,
		PackageChecksums: []common.Checksum{
			{Algorithm: common.SHA256, Value: gr.BinarySha256},
		},
		PackageLicenseConcluded: noAssertion,
		PackageLicenseDeclared:  noAssertion,
		PackageCopyrightText:    noAssertion,
		PackageSourceInfo:       fmt.Sprintf("built by %s from %s", gr.GoVersion, gr.MainPath),
		PrimaryPackagePurpose:   "APPLICATION",
		PackageExternalReferences: []*v2_3.PackageExternalReference{{
			Category: "PACKAGE-MANAGER",
			RefType:  "purl",
			Locator:  component{Path: gr.MainPath, Version: version}.purl(),
		}},
	}
}

// spdxPackage renders one linked module.
//
// The supplier is NOASSERTION rather than a guess derived from the module
// path. A path's first element is a hostname, not an organisation, and a
// document that asserts "Organization: github.com" is worse than one that
// declines to say.
func (c component) spdxPackage() *v2_3.Package {
	pkg := &v2_3.Package{
		PackageName:               c.Path,
		PackageSPDXIdentifier:     common.ElementID(c.spdxIdentifier()[len("SPDXRef-"):]),
		PackageVersion:            c.Version,
		PackageSupplier:           &common.Supplier{Supplier: noAssertion},
		PackageDownloadLocation:   noAssertion,
		FilesAnalyzed:             false,
		IsFilesAnalyzedTagPresent: true,
		PackageLicenseConcluded:   c.License.Concluded,
		PackageLicenseDeclared:    c.License.Declared,
		PackageCopyrightText:      noAssertion,
		PrimaryPackagePurpose:     "LIBRARY",
		PackageExternalReferences: []*v2_3.PackageExternalReference{{
			Category: "PACKAGE-MANAGER",
			RefType:  "purl",
			Locator:  c.purl(),
		}},
	}
	if c.License.Declared != noAssertion && c.License.Concluded == noAssertion {
		pkg.PackageLicenseComments = fmt.Sprintf(
			"licence classifier matched %s over %.0f%% of the licence file, below the %.0f%% this document requires to conclude a licence",
			c.License.Declared, c.License.Coverage*100, licenseCoverageThreshold*100)
	}
	if sum := goSumToHex(c.Sum); sum != "" {
		pkg.PackageChecksums = append(pkg.PackageChecksums, common.Checksum{
			Algorithm: common.SHA256,
			Value:     sum,
		})
	}
	if c.ReplacedFrom != "" {
		pkg.PackageSourceInfo = "replaces " + c.ReplacedFrom
	}
	return pkg
}

// goSumToHex converts a module's dirhash ("h1:" + base64 of a SHA-256)
// into the hex SPDX wants. A sum in any other form is not a SHA-256 and
// is dropped rather than mangled into one.
func goSumToHex(sum string) string {
	encoded, ok := strings.CutPrefix(sum, "h1:")
	if !ok {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) != sha256.Size {
		return ""
	}
	return hex.EncodeToString(raw)
}
