package planner

import (
	"path"
	"sort"
	"strings"
)

// bindingExt suffixes every generated dependency binding dagger emits.
const bindingExt = ".gen.go"

// coreBindingStem is the binding for the module's own core API.
const coreBindingStem = "dagger"

// BindingDir returns the directory the root module's generated dependency
// bindings live in. rootSource is the root module's source subpath from its
// config ("ci" in this repo, "" or "." when the module is the repository root
// itself).
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

// GeneratedFor returns the source directory a committed generated file was
// written into by codegen, repo-relative and "." for the repository root: the
// directory holding a dagger.gen.go, or the one above an internal/dagger/*.gen.go.
// That directory is a module's source subpath, which is where the Go SDK writes
// every file it generates. ok is false for a path of neither shape.
func GeneratedFor(p string) (dir string, ok bool) {
	p = strings.TrimPrefix(path.Clean(p), "/")
	parent := path.Dir(p)
	switch {
	case strings.HasSuffix(p, bindingExt) && (parent == "internal/dagger" || strings.HasSuffix(parent, "/internal/dagger")):
		return path.Dir(path.Dir(parent)), true
	case path.Base(p) == coreBindingStem+bindingExt:
		return parent, true
	}
	return "", false
}

// Unswept returns, sorted, the committed generated files that belong to no swept
// module: those whose GeneratedFor directory is not the source subpath of any
// module in sourceDirs.
//
// It is the check that a freshness sweep looked at everything, and its whole value
// is that its two inputs are independent. generated comes from globbing the
// workspace for the files codegen writes; sourceDirs comes from the module
// discovery that drove the sweep. A module discovery does not recognise — a config
// shape it does not know, a marker it was never taught — still has its generated
// files committed, so it shows up here instead of being skipped in silence.
//
// Ownership is by exact source directory and never by prefix or by source
// context. The root module owns every path by prefix, and a module's context can
// contain a nested module's files, so either test would let the root or a parent
// quietly account for a module nobody swept.
func Unswept(generated, sourceDirs []string) []string {
	swept := make(map[string]bool, len(sourceDirs))
	for _, dir := range sourceDirs {
		dir = strings.Trim(path.Clean(dir), "/")
		if dir == "" {
			dir = "."
		}
		swept[dir] = true
	}
	var out []string
	for _, p := range generated {
		dir, ok := GeneratedFor(p)
		if ok && swept[dir] {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
