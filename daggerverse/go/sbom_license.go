package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dagger/go/internal/dagger"

	"github.com/google/licensecheck"
)

// licenseIndex maps "<module path>@<version>" to what the classifier made
// of that module's licence file.
type licenseIndex map[string]license

// lookup returns the finding for one module, or an unasserted licence
// when the module was not in the resolved set. A module in the binary but
// not in the module graph is possible — a `go mod tidy` between build and
// document, most obviously — and is a gap in knowledge rather than an
// error to fail the whole document over.
func (idx licenseIndex) lookup(path, version string) license {
	if found, ok := idx[path+"@"+version]; ok {
		return found
	}
	return license{Declared: noAssertion, Concluded: noAssertion}
}

// collectLicensesScript gathers, for every module in the build list, its
// coordinate and its licence file.
//
// The file names are the ones the Go ecosystem actually uses; the list is
// ordered so that a plain LICENSE wins over an appended NOTICE-style file
// when a module ships both. Modules are numbered rather than named after
// their path because a module path carries "/" and mixed case, and the
// escaping scheme that survives both is the module cache's own — which is
// exactly the thing this avoids having to reimplement.
//
// `|| true` on the classifier-facing half only: a module with no licence
// file is normal and must not fail the collection, whereas a failure to
// list the build list at all is a real error and `set -e` catches it.
const collectLicensesScript = `
set -e
mkdir -p /licenses
go mod download all
n=0
go list -m -f '{{.Path}}|{{.Version}}|{{.Dir}}' all | while IFS='|' read -r p v d; do
  [ -n "$v" ] || continue
  n=$((n+1))
  o="/licenses/m$n"
  mkdir -p "$o"
  printf '%s\n%s\n' "$p" "$v" > "$o/module"
  [ -n "$d" ] || continue
  for f in LICENSE LICENSE.txt LICENSE.md LICENCE LICENCE.txt LICENCE.md COPYING COPYING.txt COPYING.md LICENSE-MIT LICENSE-APACHE; do
    if [ -f "$d/$f" ]; then cp "$d/$f" "$o/text"; break; fi
  done
done
`

// resolveLicenses classifies the licence file of every module in source's
// build list.
//
// The work splits across the boundary the way it has to: the module cache
// only exists inside a toolchain container, so the *collection* is a
// container exec, while the *classification* is pure Go in this module —
// licensecheck is a Go library and shelling out to a scanner image to
// re-derive what a library call answers is the pattern this repo's SBOM
// conventions exist to rule out.
func (g *Go) resolveLicenses(ctx context.Context, source *dagger.Directory, work string) (licenseIndex, error) {
	ctr, err := g.Container(ctx, source)
	if err != nil {
		return nil, err
	}
	dir := ctr.WithExec([]string{"sh", "-c", collectLicensesScript}).Directory("/licenses")

	root := filepath.Join(work, "licenses")
	if _, err := dir.Export(ctx, root); err != nil {
		return nil, fmt.Errorf("collect module licences: %v", err)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read collected licences: %v", err)
	}
	idx := licenseIndex{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		modPath := filepath.Join(root, entry.Name(), "module")
		coordinate, err := os.ReadFile(modPath) //nolint:gosec // path is this function's own temp dir
		if err != nil {
			return nil, fmt.Errorf("read %s: %v", modPath, err)
		}
		lines := strings.Split(strings.TrimSpace(string(coordinate)), "\n")
		if len(lines) != 2 {
			return nil, fmt.Errorf("malformed module coordinate in %s: %q", modPath, coordinate)
		}
		key := strings.TrimSpace(lines[0]) + "@" + strings.TrimSpace(lines[1])

		text, err := os.ReadFile(filepath.Join(root, entry.Name(), "text")) //nolint:gosec // same
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("read licence text for %s: %v", key, err)
			}
			idx[key] = license{Declared: noAssertion, Concluded: noAssertion}
			continue
		}
		idx[key] = classify(text)
	}
	return idx, nil
}

// classify runs the licence classifier over one licence file and maps its
// coverage report onto SPDX's declared/concluded pair.
//
// The classifier reports which known licence texts it matched and how
// much of the file those matches account for. That is a measurement, not
// a verdict, and the two SPDX fields are how a document says so: the best
// match is always declared, and it is only concluded when the coverage
// clears licenseCoverageThreshold. A file the classifier does not
// recognise at all declares NOASSERTION rather than guessing.
//
// Multiple matches are joined with SPDX's " AND " because a file carrying
// two licence texts imposes both; a dual-licensed module says so in prose
// the classifier cannot read, and asserting OR from coverage alone would
// be the optimistic reading of an ambiguous file.
func classify(text []byte) license {
	cov := licensecheck.Scan(text)
	ids := make([]string, 0, len(cov.Match))
	seen := map[string]bool{}
	for _, match := range cov.Match {
		if match.ID == "" || seen[match.ID] {
			continue
		}
		seen[match.ID] = true
		ids = append(ids, match.ID)
	}
	if len(ids) == 0 {
		return license{Declared: noAssertion, Concluded: noAssertion}
	}
	declared := strings.Join(ids, " AND ")
	coverage := cov.Percent / 100
	out := license{Declared: declared, Concluded: noAssertion, Coverage: coverage}
	if coverage >= licenseCoverageThreshold {
		out.Concluded = declared
	}
	return out
}
