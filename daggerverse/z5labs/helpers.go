package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"dagger/z-5-labs/internal/dagger"
)

// resolvedLintConfig returns override when non-nil; otherwise materializes
// the bundled defaultLintConfig as a *dagger.File via the module workdir.
func resolvedLintConfig(_ context.Context, override *dagger.File) (*dagger.File, error) {
	if override != nil {
		return override, nil
	}
	return writeWorkdirFile("golangci.yml", defaultLintConfig)
}

// writeWorkdirFile writes content to a content-addressed subdir of the
// module's scratch workdir and returns it as a *dagger.File.
//
// name may carry path separators. The provenance envelope is named after
// the binary, and a binary name is "<owner>/<repo>" for any registry
// without single-segment repositories — GHCR, which is the shape this
// module's own guidance leads to. So the directory created is the
// parent of the final path rather than the content-addressed dir alone,
// and the temp pattern is name's last element: os.CreateTemp rejects any
// pattern containing a separator, by documented design.
func writeWorkdirFile(name string, content []byte) (*dagger.File, error) {
	sum := sha256.Sum256(content)
	dir := "out-" + hex.EncodeToString(sum[:])
	path := filepath.Join(dir, name)
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, err
	}
	// The temp file is created in the same directory it is renamed into,
	// so the rename stays within one filesystem and lands atomically.
	tmp, err := os.CreateTemp(parent, "."+filepath.Base(path)+"-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return nil, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return nil, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return nil, err
	}
	return dag.CurrentModule().WorkdirFile(path), nil
}

// sharedCheck runs the standard z5labs check stages (fmt, vet, lint with
// the resolved config, test -race) against source via the go module dep.
//
// lintVersion is passed straight through: empty means the `go` module's
// pinned default, which is what keeps the pin a single edit in one place
// rather than a number restated here as well.
func sharedCheck(ctx context.Context, source *dagger.Directory, lintOverride *dagger.File, lintVersion string) error {
	cfg, err := resolvedLintConfig(ctx, lintOverride)
	if err != nil {
		return err
	}
	return dag.Go().
		Ci(source).
		WithFmt().
		WithVet().
		WithLint(dagger.GoCiWithLintOpts{Config: cfg, Version: lintVersion}).
		WithTest(dagger.GoCiWithTestOpts{Race: true}).
		Check(ctx)
}

// parseModuleDirective scans go.mod for the top-level `module <path>`
// directive and returns the path. Returns "" with a nil error if the
// directive is absent; surfaces scanner.Err() so I/O / long-line
// failures don't masquerade as a missing directive.
func parseModuleDirective(content string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "module" {
			return fields[1], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", nil
}

// basenameAfterSlash returns everything after the final "/" in s.
func basenameAfterSlash(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}
