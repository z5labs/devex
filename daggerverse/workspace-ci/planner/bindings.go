package planner

import (
	"path"
	"strings"
	"unicode"
)

// bindingExt suffixes every generated dependency binding dagger emits.
const bindingExt = ".gen.go"

// coreBindingStem is the binding for the module's own core API, the one that is
// attributable to no toolchain at all.
const coreBindingStem = "dagger"

// BindingDir returns the directory the root module's generated dependency
// bindings live in. rootSource is the root module's source subpath from its
// dagger.json ("ci" in this repo, "" or "." when the module is the repository
// root itself).
func BindingDir(rootSource string) string {
	dir := path.Join(cleanRootSource(rootSource), "internal", "dagger")
	return dir + "/"
}

// CoreBinding returns the root module's own core binding path.
func CoreBinding(rootSource string) string {
	return BindingDir(rootSource) + coreBindingStem + bindingExt
}

func cleanRootSource(rootSource string) string {
	s := strings.Trim(path.Clean(rootSource), "/")
	if s == "." {
		return ""
	}
	return s
}

// AggregatorBindings maps each per-toolchain aggregator binding path under the
// root module to the toolchain module directory it is generated from, so that
// regenerating a single binding — which repo convention requires whenever a
// toolchain's sources shift line numbers — is attributed to that toolchain
// instead of tripping Attribute's root-module fail-safe and forcing everything
// to run (#179).
//
// toolchains maps a toolchain name to its source directory, straight out of the
// root module's dagger.json. The binding's file name is the toolchain name after
// dagger's kebab-casing (toolchain z5labs-tests -> internal/dagger/z-5-labs-tests.gen.go),
// which is why Kebab lives in this package: this is now the only copy of that
// rule, where the workflow's jq and the check-name prefix each used to hold one.
//
// The mapping deliberately covers nothing else. Any other path under the root
// module's source — including the core binding, which no single toolchain owns —
// is absent here and so still runs everything.
func AggregatorBindings(rootSource string, toolchains map[string]string) map[string]string {
	dir := BindingDir(rootSource)
	out := make(map[string]string, len(toolchains))
	ambiguous := map[string]bool{}
	for name, src := range toolchains {
		if name == "" || src == "" {
			continue
		}
		p := dir + Kebab(name) + bindingExt
		if prev, seen := out[p]; seen && prev != src {
			// Two toolchains cannot share a binding; if the config says
			// otherwise, something is off — fail open rather than guess.
			ambiguous[p] = true
		}
		out[p] = src
	}
	for p := range ambiguous {
		delete(out, p)
	}
	delete(out, CoreBinding(rootSource))
	return out
}

// Kebab applies dagger's kebab-casing to a name: camel-case boundaries and
// letter<->digit boundaries both become hyphens, and the result is lowercased.
// It is how a toolchain name (z5labs-tests) becomes the stem of the binding
// dagger generates for it (z-5-labs-tests.gen.go).
func Kebab(name string) string {
	var b strings.Builder
	runes := []rune(name)
	for i, r := range runes {
		var next rune
		if i+1 < len(runes) {
			next = runes[i+1]
		}
		if i > 0 && needsHyphen(runes[i-1], r, next) {
			b.WriteRune('-')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// needsHyphen reports whether a hyphen belongs before cur, given the rune before
// it and the one after. An existing separator never gets a second one, and the run
// of capitals inside an acronym is not split — only a lower->upper transition, a
// digit boundary, and the tail of an acronym (HTTPServer -> http-server, where the
// S is upper, follows an upper, and is followed by a lower) do.
func needsHyphen(prev, cur, next rune) bool {
	if prev == '-' || cur == '-' {
		return false
	}
	switch {
	case unicode.IsDigit(cur) != unicode.IsDigit(prev):
		return unicode.IsLetter(prev) || unicode.IsLetter(cur)
	case !unicode.IsUpper(cur):
		return false
	case !unicode.IsUpper(prev):
		return true
	default:
		return unicode.IsLower(next)
	}
}
