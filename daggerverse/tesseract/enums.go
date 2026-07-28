package main

import "strings"

// PageSegMode is tesseract's page-segmentation mode (`--psm`): how much layout
// analysis to do before recognising anything. The enum value maps to the CLI's
// number internally, so an out-of-range mode is unrepresentable through the SDK.
//
// Mode 2 ("automatic page segmentation, but no OSD or OCR") is deliberately
// absent: upstream never implemented it, so it would ship as an always-useless
// member.
//
// Note on rendered names: the Dagger Go SDK derives each GraphQL enum member
// from the *constant identifier* in SCREAMING_SNAKE_CASE, so these surface as
// `OSD_ONLY`, `SINGLE_BLOCK_VERT_TEXT`, and so on.
type PageSegMode string

const (
	// PageSegModeOsdOnly detects orientation and script and recognises
	// nothing. Osd is the ergonomic path to this mode; selecting it here
	// makes the text outputs return the OSD report instead of recognised text.
	PageSegModeOsdOnly PageSegMode = "OSD_ONLY"
	// PageSegModeAutoOsd is full automatic segmentation with orientation and
	// script detection.
	PageSegModeAutoOsd PageSegMode = "AUTO_OSD"
	// PageSegModeAuto is full automatic segmentation without OSD, and is
	// tesseract's default.
	PageSegModeAuto PageSegMode = "AUTO"
	// PageSegModeSingleColumn assumes a single column of text of variable
	// sizes.
	PageSegModeSingleColumn PageSegMode = "SINGLE_COLUMN"
	// PageSegModeSingleBlockVertText assumes a single uniform block of
	// vertically aligned text.
	PageSegModeSingleBlockVertText PageSegMode = "SINGLE_BLOCK_VERT_TEXT"
	// PageSegModeSingleBlock assumes a single uniform block of text.
	PageSegModeSingleBlock PageSegMode = "SINGLE_BLOCK"
	// PageSegModeSingleLine treats the image as a single text line.
	PageSegModeSingleLine PageSegMode = "SINGLE_LINE"
	// PageSegModeSingleWord treats the image as a single word.
	PageSegModeSingleWord PageSegMode = "SINGLE_WORD"
	// PageSegModeCircleWord treats the image as a single word in a circle.
	PageSegModeCircleWord PageSegMode = "CIRCLE_WORD"
	// PageSegModeSingleChar treats the image as a single character.
	PageSegModeSingleChar PageSegMode = "SINGLE_CHAR"
	// PageSegModeSparseText finds as much text as possible in no particular
	// order.
	PageSegModeSparseText PageSegMode = "SPARSE_TEXT"
	// PageSegModeSparseTextOsd is sparse text with orientation and script
	// detection.
	PageSegModeSparseTextOsd PageSegMode = "SPARSE_TEXT_OSD"
	// PageSegModeRawLine treats the image as a single text line, bypassing
	// tesseract-specific hacks.
	PageSegModeRawLine PageSegMode = "RAW_LINE"
)

// pageSegTokens maps each mode onto the number `--psm` takes.
var pageSegTokens = map[PageSegMode]string{
	PageSegModeOsdOnly:             "0",
	PageSegModeAutoOsd:             "1",
	PageSegModeAuto:                "3",
	PageSegModeSingleColumn:        "4",
	PageSegModeSingleBlockVertText: "5",
	PageSegModeSingleBlock:         "6",
	PageSegModeSingleLine:          "7",
	PageSegModeSingleWord:          "8",
	PageSegModeCircleWord:          "9",
	PageSegModeSingleChar:          "10",
	PageSegModeSparseText:          "11",
	PageSegModeSparseTextOsd:       "12",
	PageSegModeRawLine:             "13",
}

// token returns the `--psm` number for this mode, and whether the mode is one
// this module knows.
func (m PageSegMode) token() (string, bool) {
	tok, ok := pageSegTokens[m]
	return tok, ok
}

// EngineMode is the OCR engine tesseract recognises with (`--oem`).
//
// All four modes are usable here because Alpine packages the *combined*
// tesseract-ocr/tessdata models, which carry legacy data alongside LSTM. A
// build against tessdata_fast or tessdata_best would leave LEGACY and
// LEGACY_LSTM failing at runtime.
type EngineMode string

const (
	// EngineModeLegacy uses the pre-4.0 pattern-matching engine only.
	EngineModeLegacy EngineMode = "LEGACY"
	// EngineModeLstm uses the LSTM neural-network engine only.
	EngineModeLstm EngineMode = "LSTM"
	// EngineModeLegacyLstm runs both engines and combines their results.
	EngineModeLegacyLstm EngineMode = "LEGACY_LSTM"
	// EngineModeDefault lets tesseract pick based on what the language data
	// provides, and is what an unset `--oem` gets.
	EngineModeDefault EngineMode = "DEFAULT"
)

// engineTokens maps each mode onto the number `--oem` takes.
var engineTokens = map[EngineMode]string{
	EngineModeLegacy:     "0",
	EngineModeLstm:       "1",
	EngineModeLegacyLstm: "2",
	EngineModeDefault:    "3",
}

// token returns the `--oem` number for this mode, and whether the mode is one
// this module knows.
func (m EngineMode) token() (string, bool) {
	tok, ok := engineTokens[m]
	return tok, ok
}

// Format is an output renderer, which tesseract selects with a trailing
// CONFIGFILE word rather than a flag. Export takes a set of these because one
// recognition pass can drive several renderers at once.
type Format string

const (
	// FormatTxt is plain UTF-8 text, one line per recognised text line.
	FormatTxt Format = "TXT"
	// FormatHocr is hOCR: HTML carrying per-word bounding boxes and
	// confidences.
	FormatHocr Format = "HOCR"
	// FormatAlto is ALTO XML, the library-and-archive layout schema.
	FormatAlto Format = "ALTO"
	// FormatTsv is tab-separated rows, one per layout element, ending in the
	// word level.
	FormatTsv Format = "TSV"
	// FormatPdf is a searchable PDF: the source image with an invisible text
	// layer behind it.
	FormatPdf Format = "PDF"
	// FormatPage is PAGE XML, the PRImA layout-analysis schema.
	FormatPage Format = "PAGE"
)

// formatSpec is the pair of CLI facts each renderer needs: the CONFIGFILE word
// that turns it on, and the extension it appends to the output base.
type formatSpec struct {
	config string
	ext    string
}

// formatTable is the legal Format set. ALTO and PAGE both emit XML but land on
// different names (`result.xml` vs `result.page.xml`), so requesting both in
// one Export does not collide.
var formatTable = map[Format]formatSpec{
	FormatTxt:  {config: "txt", ext: ".txt"},
	FormatHocr: {config: "hocr", ext: ".hocr"},
	FormatAlto: {config: "alto", ext: ".xml"},
	FormatTsv:  {config: "tsv", ext: ".tsv"},
	FormatPdf:  {config: "pdf", ext: ".pdf"},
	FormatPage: {config: "page", ext: ".page.xml"},
}

// The training-adjacent renderers are deliberately not Format members, and so
// are reachable only through the function named after each.
//
// Export's promise is that a set of formats is one recognition pass producing
// one artifact per format, and none of these three keeps it. makebox and
// get.images describe the recognition rather than reporting it — a caller
// asking for TXT and PDF wants two results, not a debugging aid alongside
// them — and lstm.train is not an output of recognition at all: it needs
// ground truth the other renderers have no use for, which is an argument a
// member of an enum cannot carry.
var (
	// boxSpec is the character-level box file: one row per recognised
	// character with the box it was found in.
	boxSpec = formatSpec{config: "makebox", ext: boxExt}
	// processedImagesSpec is the image tesseract actually recognised, after
	// its own binarization and deskewing.
	processedImagesSpec = formatSpec{config: "get.images", ext: processedImagesExt}
	// lstmTrainSpec is one LSTM training sample: the line image paired with
	// the text it renders.
	lstmTrainSpec = formatSpec{config: "lstm.train", ext: lstmfExt}
)

// formatOrder fixes the order formats are rendered and reported in, so an
// Export's argv — and therefore its cache key — does not depend on the order
// the caller happened to list them.
var formatOrder = []Format{FormatTxt, FormatHocr, FormatAlto, FormatTsv, FormatPdf, FormatPage}

// formatNames lists every legal Format, for the error message a rejection
// carries.
func formatNames() string {
	names := make([]string, 0, len(formatOrder))
	for _, f := range formatOrder {
		names = append(names, string(f))
	}
	return strings.Join(names, ", ")
}
