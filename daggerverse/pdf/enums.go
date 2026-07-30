package main

import "strings"

// No const in this file aliases an enum member. An alias reads to the
// generator as an extra member of the same enum carrying the aliased member's
// *value*, which breaks codegen in a module that depends on this one rather
// than in this one — so it passes `dagger develop` here and fails in the
// dependent's.

// ColorMode is the colour space pages are rendered in, and maps onto
// pdftoppm's `-gray` and `-mono`.
//
// It changes the pixels rather than the file format: poppler's PNG writer
// emits 8-bit RGB whatever mode was asked for, so a grayscale render is an
// RGB image whose channels are equal and a monochrome one an RGB image whose
// pixels are pure black or pure white. That is worth knowing before writing a
// bit-depth assertion, and irrelevant to why the modes exist — binarized or
// grayscale input is exactly what OCR preprocessing wants.
//
// Note on rendered names: the Dagger Go SDK derives each GraphQL enum member
// from the *constant identifier* in SCREAMING_SNAKE_CASE, so these surface as
// `COLOR`, `GRAY` and `MONO`.
type ColorMode string

const (
	// ColorModeColor renders in full colour, and is pdftoppm's default.
	ColorModeColor ColorMode = "COLOR"
	// ColorModeGray renders in 8-bit grayscale (`-gray`).
	ColorModeGray ColorMode = "GRAY"
	// ColorModeMono renders bilevel (`-mono`): every pixel pure black or pure
	// white, with flat tones dithered rather than averaged.
	ColorModeMono ColorMode = "MONO"
)

// colorFlags maps each mode onto the pdftoppm flag that selects it. Colour is
// the default and takes no flag, which is why the value is empty rather than
// absent — presence in this table is what makes a mode legal.
var colorFlags = map[ColorMode][]string{
	ColorModeColor: nil,
	ColorModeGray:  {"-gray"},
	ColorModeMono:  {"-mono"},
}

// colorOrder fixes the order modes are listed in a rejection message, so the
// message does not depend on Go's map iteration order.
var colorOrder = []ColorMode{ColorModeColor, ColorModeGray, ColorModeMono}

// flags returns the pdftoppm flags for this mode, and whether the mode is one
// this module knows.
func (m ColorMode) flags() ([]string, bool) {
	f, ok := colorFlags[m]
	return f, ok
}

// LayoutMode is how much of the page's physical arrangement survives into the
// extracted text, and maps onto pdftotext's `-layout` and `-raw`.
//
// The three are genuinely different answers rather than degrees of one: a
// table wants PHYSICAL, prose in columns wants READING, and a caller
// reconstructing the content stream wants RAW.
type LayoutMode string

const (
	// LayoutModeReading undoes the physical layout and emits text in reading
	// order — each column of a multi-column page read to its end before the
	// next one begins. It is pdftotext's default.
	LayoutModeReading LayoutMode = "READING"
	// LayoutModePhysical preserves the page's arrangement (`-layout`), padding
	// with spaces so columns stay side by side and a table's cells stay in
	// their rows.
	LayoutModePhysical LayoutMode = "PHYSICAL"
	// LayoutModeRaw emits text in content-stream order (`-raw`): the order the
	// document draws it in, which for a two-column page is usually neither of
	// the above.
	LayoutModeRaw LayoutMode = "RAW"
)

// layoutFlags maps each mode onto the pdftotext flag that selects it. Reading
// order is the default and takes no flag.
var layoutFlags = map[LayoutMode][]string{
	LayoutModeReading:  nil,
	LayoutModePhysical: {"-layout"},
	LayoutModeRaw:      {"-raw"},
}

// layoutOrder fixes the order modes are listed in a rejection message.
var layoutOrder = []LayoutMode{LayoutModeReading, LayoutModePhysical, LayoutModeRaw}

// flags returns the pdftotext flags for this mode, and whether the mode is one
// this module knows.
func (m LayoutMode) flags() ([]string, bool) {
	f, ok := layoutFlags[m]
	return f, ok
}

// rasterFormat is the image format a render produces. It is deliberately not a
// Dagger enum: Png, Jpeg and Tiff are three named functions, so a caller never
// spells a format at all and an enum would only add a way to spell it wrongly.
type rasterFormat struct {
	// flag is the pdftoppm switch selecting this format.
	flag string
	// ext is the extension pdftoppm appends to the output base for it. These
	// are poppler's own choices and not this module's: `-jpeg` writes `.jpg`
	// and `-tiff` writes `.tif`, so the page-naming contract is
	// `page-0001.jpg` and `page-0001.tif` respectively.
	ext string
}

var (
	formatPng  = rasterFormat{flag: "-png", ext: "png"}
	formatJpeg = rasterFormat{flag: "-jpeg", ext: "jpg"}
	formatTiff = rasterFormat{flag: "-tiff", ext: "tif"}
)

// colorNames lists every legal ColorMode, for the message a rejection carries.
func colorNames() string {
	names := make([]string, 0, len(colorOrder))
	for _, m := range colorOrder {
		names = append(names, string(m))
	}
	return strings.Join(names, ", ")
}

// layoutNames lists every legal LayoutMode, for the message a rejection
// carries.
func layoutNames() string {
	names := make([]string, 0, len(layoutOrder))
	for _, m := range layoutOrder {
		names = append(names, string(m))
	}
	return strings.Join(names, ", ")
}
