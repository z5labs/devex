package main

import (
	"fmt"
	"strings"
)

// No const in this file aliases an enum member. An alias reads to the
// generator as an extra member of the same enum carrying the aliased member's
// *value*, which breaks codegen in a module that depends on this one rather
// than in this one — so it passes `dagger develop` here and fails in the
// dependent's.

// BuildMode is what `go build -buildmode` produces: the kind of artifact the
// linker emits, which for most of these is not an executable at all. It is an
// enum rather than a string because the set is closed and each member has a
// different output shape a caller has to be ready for.
//
// Two of `go build`'s modes are deliberately absent. `default` is what
// omitting this input already means, so a member for it would be a second
// spelling of the same request. `shared` is only half a feature without a
// `-linkshared` counterpart on the consuming build, which Build does not have
// — use Container() if you are building a shared std.
//
// Note on rendered names: the Dagger Go SDK derives each GraphQL enum member
// from the *constant identifier* in SCREAMING_SNAKE_CASE, so these surface as
// `ARCHIVE`, `C_ARCHIVE`, `C_SHARED`, `EXE`, `PIE` and `PLUGIN`. That is why
// the `go build` spelling (`c-archive`) lives in buildModeFlags below rather
// than in the identifier: a hyphen cannot appear in a Go identifier, so the
// mapping has to be explicit.
type BuildMode string

const (
	// BuildModeArchive builds the listed non-main packages into `.a` files
	// (`archive`). Main packages are ignored, so pointing this at one
	// produces nothing.
	BuildModeArchive BuildMode = "ARCHIVE"
	// BuildModeCArchive builds the listed main package into a C archive
	// (`c-archive`). Only the functions carrying a cgo `//export` comment are
	// callable, and it is those exports rather than the mode that need cgo —
	// so this is not rejected alongside disableCgo the way race is. With cgo
	// off, a package whose exports live in cgo files fails to build at all
	// (`build constraints exclude all Go files`), and a pure-Go main package
	// still produces an archive, but one exporting nothing and carrying no
	// generated header. The archive/header pair is a consequence of having
	// cgo exports, not of asking for this mode.
	BuildModeCArchive BuildMode = "C_ARCHIVE"
	// BuildModeCShared builds the listed main package into a C shared
	// library (`c-shared`) — the same exported surface as C_ARCHIVE, linked
	// dynamically instead, and with the same relationship to cgo.
	BuildModeCShared BuildMode = "C_SHARED"
	// BuildModeExe builds the listed main packages into executables
	// (`exe`), forcing a position-dependent executable on a toolchain whose
	// default for the target is PIE.
	BuildModeExe BuildMode = "EXE"
	// BuildModePie builds the listed main packages into position
	// independent executables (`pie`), which is what a hardened runtime
	// wanting ASLR requires.
	BuildModePie BuildMode = "PIE"
	// BuildModePlugin builds the listed main packages into a shared library
	// loadable at run time with `plugin.Open` (`plugin`). The plugin and
	// its host have to be built by the same toolchain from the same
	// dependency versions or the load fails.
	BuildModePlugin BuildMode = "PLUGIN"
)

// buildModeFlags maps each member onto the value `go build -buildmode=` takes.
// Presence in this table is what makes a mode legal, so a member added above
// without an entry here is rejected rather than silently dropped.
var buildModeFlags = map[BuildMode]string{
	BuildModeArchive:  "archive",
	BuildModeCArchive: "c-archive",
	BuildModeCShared:  "c-shared",
	BuildModeExe:      "exe",
	BuildModePie:      "pie",
	BuildModePlugin:   "plugin",
}

// buildModeOrder fixes the order modes are listed in a rejection message, so
// the message does not depend on Go's map iteration order.
var buildModeOrder = []BuildMode{
	BuildModeArchive,
	BuildModeCArchive,
	BuildModeCShared,
	BuildModeExe,
	BuildModePie,
	BuildModePlugin,
}

// flag returns the `-buildmode=` value for mode, or an error naming the legal
// set. The empty mode is the caller not asking for one, which is not this
// function's business to decide — Build checks for it before calling.
func (mode BuildMode) flag() (string, error) {
	if f, ok := buildModeFlags[mode]; ok {
		return f, nil
	}
	legal := make([]string, 0, len(buildModeOrder))
	for _, m := range buildModeOrder {
		legal = append(legal, string(m))
	}
	return "", fmt.Errorf("Build: unknown buildmode %q (want one of %s)", string(mode), strings.Join(legal, ", "))
}
