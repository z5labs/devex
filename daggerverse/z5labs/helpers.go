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
func writeWorkdirFile(name string, content []byte) (*dagger.File, error) {
	sum := sha256.Sum256(content)
	dir := "out-" + hex.EncodeToString(sum[:])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, "."+name+"-*")
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
func sharedCheck(ctx context.Context, source *dagger.Directory, lintOverride *dagger.File) error {
	cfg, err := resolvedLintConfig(ctx, lintOverride)
	if err != nil {
		return err
	}
	return dag.Go().
		Ci(source).
		WithFmt().
		WithVet().
		WithLint(dagger.GoCiWithLintOpts{Config: cfg}).
		WithTest(dagger.GoCiWithTestOpts{Race: true}).
		Check(ctx)
}

// parseModuleDirective scans go.mod for the top-level `module <path>`
// directive and returns the path. Returns "" if absent.
func parseModuleDirective(content string) string {
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "module" {
			return fields[1]
		}
	}
	return ""
}

// basenameAfterSlash returns everything after the final "/" in s.
func basenameAfterSlash(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}
