package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	cdx "github.com/CycloneDX/cyclonedx-go"
)

// cycloneDxSpecVersion is the revision this module targets. See the doc
// comment on Go.CycloneDx for why it is pinned rather than left to the
// library's default.
const cycloneDxSpecVersion = cdx.SpecVersion1_6

// cycloneDxDocument renders the resolved graph as CycloneDX 1.6 JSON.
//
// The mapping is deliberately the same information SPDX gets, expressed
// in CycloneDX's vocabulary: the same component set, the same versions,
// the same purls and the same licence findings. A test compares the two
// component sets, and it can only be meaningful if neither renderer
// resolves anything the other does not see.
func (gr *graph) cycloneDxDocument() ([]byte, error) {
	components := make([]cdx.Component, 0, len(gr.Components))
	deps := make([]string, 0, len(gr.Components))
	for _, comp := range gr.Components {
		components = append(components, comp.cycloneDxComponent())
		deps = append(deps, comp.purl())
	}

	subject := gr.cycloneDxSubject()
	dependencies := []cdx.Dependency{{Ref: subject.BOMRef, Dependencies: &deps}}

	bom := cdx.NewBOM()
	bom.SerialNumber = gr.serialNumber()
	bom.Metadata = &cdx.Metadata{
		Timestamp: gr.Created.Format(time.RFC3339),
		Tools: &cdx.ToolsChoice{Components: &[]cdx.Component{{
			Type:   cdx.ComponentTypeApplication,
			Name:   creatorTool,
			Author: "z5labs",
		}}},
		Component: &subject,
		Supplier:  &cdx.OrganizationalEntity{Name: "z5labs"},
	}
	bom.Components = &components
	bom.Dependencies = &dependencies

	// The library's own encoder rather than encoding/json: EncodeVersion
	// converts the BOM to the target revision and stamps the matching
	// $schema and specVersion. Marshalling the struct directly would emit
	// whatever revision NewBOM happens to default to under a schema URL
	// naming a different one.
	var buf bytes.Buffer
	enc := cdx.NewBOMEncoder(&buf, cdx.BOMFileFormatJSON)
	enc.SetPretty(true)
	if err := enc.EncodeVersion(bom, cycloneDxSpecVersion); err != nil {
		return nil, fmt.Errorf("encode cyclonedx document: %v", err)
	}
	return buf.Bytes(), nil
}

// serialNumber derives the BOM's urn:uuid from the artifact's digest
// rather than from a random source, so two documents describing the same
// bytes differ only in their timestamp. The variant and version bits are
// set so the result is a well-formed RFC 4122 UUID, which is what
// consumers validate.
func (gr *graph) serialNumber() string {
	raw, err := hex.DecodeString(gr.BinarySha256)
	if err != nil || len(raw) < 16 {
		sum := sha256.Sum256([]byte(gr.BinarySha256))
		raw = sum[:]
	}
	b := raw[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return "urn:uuid:" + hex.EncodeToString(b[0:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" + hex.EncodeToString(b[10:16])
}

// cycloneDxSubject is the metadata component: the built artifact, hashed.
func (gr *graph) cycloneDxSubject() cdx.Component {
	version := gr.MainVersion
	if version == "" {
		version = noAssertion
	}
	subject := component{Path: gr.MainPath, Version: version}
	return cdx.Component{
		BOMRef:     subject.purl(),
		Type:       cdx.ComponentTypeApplication,
		Name:       gr.BinaryName,
		Version:    version,
		PackageURL: subject.purl(),
		Supplier:   &cdx.OrganizationalEntity{Name: "z5labs"},
		Hashes: &[]cdx.Hash{{
			Algorithm: cdx.HashAlgoSHA256,
			Value:     gr.BinarySha256,
		}},
	}
}

// cycloneDxComponent renders one linked module.
//
// The licence acknowledgement is where the classifier's confidence
// survives the format change. CycloneDX 1.6 distinguishes a licence a
// component declares about itself from one an analysis concluded, which
// is the same distinction SPDX draws with declared vs concluded — so a
// match below the confidence threshold is published as *declared*, and a
// match at or above it as *concluded*. A component whose licence file
// matched nothing carries no licence entry at all, because CycloneDX has
// no NOASSERTION and inventing one would assert something.
func (c component) cycloneDxComponent() cdx.Component {
	out := cdx.Component{
		BOMRef:     c.purl(),
		Type:       cdx.ComponentTypeLibrary,
		Name:       c.Path,
		Version:    c.Version,
		PackageURL: c.purl(),
		Supplier:   &cdx.OrganizationalEntity{Name: noAssertion},
	}
	if c.License.Declared != noAssertion {
		acknowledgement := cdx.LicenseAcknowledgementDeclared
		if c.License.Concluded != noAssertion {
			acknowledgement = cdx.LicenseAcknowledgementConcluded
		}
		// A LicenseChoice carries either a single licence or an
		// expression, never both. A file the classifier matched more than
		// one licence text in is an expression by definition.
		if strings.Contains(c.License.Declared, " ") {
			out.Licenses = &cdx.Licenses{{
				Expression:      c.License.Declared,
				Acknowledgement: &acknowledgement,
			}}
		} else {
			out.Licenses = &cdx.Licenses{{License: &cdx.License{
				ID:              c.License.Declared,
				Acknowledgement: acknowledgement,
			}}}
		}
	}
	if sum := goSumToHex(c.Sum); sum != "" {
		out.Hashes = &[]cdx.Hash{{Algorithm: cdx.HashAlgoSHA256, Value: sum}}
	}
	if c.ReplacedFrom != "" {
		out.Description = "replaces " + c.ReplacedFrom
	}
	return out
}
