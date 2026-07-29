package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/bruno/internal/dagger"
)

const (
	// specPath is where the OpenAPI document is mounted. It carries no
	// extension on purpose: unlike an environment file — where bru selects its
	// parser from the extension — `bru import` sniffs the document's contents,
	// so a YAML spec staged under any name is read as YAML.
	specPath = "/tmp/bruno-spec"

	// generatedDir is where `bru import` writes the collection. bru creates it,
	// and it is outside /bruno because that is root-owned while the image runs
	// as UID 1000.
	generatedDir = "/tmp/bruno-collection"

	// formatBru and formatOpenCollection are the two shapes `bru import`
	// writes. Only formatBru produces the bruno.json + .bru tree that
	// Collection, Run and Report read, which is why it is this module's default
	// even though upstream's is the other one.
	formatBru            = "bru"
	formatOpenCollection = "opencollection"

	// defaultCollectionName is the name Generate stamps into the collection
	// when the caller does not choose one.
	defaultCollectionName = "api"
)

// Generate converts an OpenAPI document into a Bruno collection directory
// (`bru import openapi`), so a collection can be produced in CI rather than
// hand-maintained beside the spec it drifts from.
//
// The spec is a *dagger.File rather than a URL string, so a local document and
// a remote one are the same call: dag.HTTP(url) covers the URL case without a
// second parameter — and without needing bru's `--insecure`, since the fetch
// never happens inside the container.
//
// format defaults to "bru" where upstream defaults to "opencollection". Only
// the bru shape carries the bruno.json and .bru requests that Collection, Run
// and Report read, so the default is the one whose output feeds straight back
// into this module. "opencollection" writes an opencollection.yml instead and
// is not runnable here.
//
// Requests are grouped by OpenAPI tag, which is bru's own default, so they
// land one folder deep and need Run's recursive default to be reached.
//
// It is +cache="session" rather than "never": conversion is a pure function of
// the document and touches no live service.
//
// +cache="session"
func (b *Bruno) Generate(
	ctx context.Context,
	// The OpenAPI document to convert. YAML or JSON — bru reads the contents,
	// not the file name.
	spec *dagger.File,
	// Name stamped into the generated collection's bruno.json.
	// +default="api"
	name string,
	// Output shape: "bru", the tree this module can run, or "opencollection".
	// +default="bru"
	format string,
) (*dagger.Directory, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("Generate: collection name is required")
	}
	if err := checkCollectionFormat(format); err != nil {
		return nil, err
	}

	// Expect=ReturnTypeAny keeps a failed conversion on the value path: bru
	// spends exit 1 on every import failure — a missing file, an unparseable
	// document, a spec it cannot convert — so the exit code alone says nothing
	// and the output has to be read back to the caller.
	exec := b.Container().
		WithMountedFile(specPath, spec).
		WithExec([]string{
			"bru", "import", "openapi",
			"--source", specPath,
			"--output", generatedDir,
			"--collection-name", name,
			"--collection-format", format,
		}, dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny})

	// The exit code is read here rather than the directory returned lazily
	// because a failed import writes no output directory: deferring would turn
	// "your spec does not parse" into "that path does not exist", one call
	// later and somewhere else.
	code, err := exec.ExitCode(ctx)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("bru import openapi failed (exit %d):\n%s", code, combinedOutput(ctx, exec))
	}
	return exec.Directory(generatedDir), nil
}

// checkCollectionFormat rejects a shape `bru import` does not write. bru itself
// refuses one too, but by printing its entire help text with the actual
// complaint on the last line; naming the two legal values costs nothing and
// does not run a container to find out.
func checkCollectionFormat(format string) error {
	switch format {
	case formatBru, formatOpenCollection:
		return nil
	default:
		return fmt.Errorf("Generate: invalid format %q: must be one of %s, %s",
			format, formatBru, formatOpenCollection)
	}
}
