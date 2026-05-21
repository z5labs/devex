package main

import (
	"context"

	"dagger/z-5-labs/internal/dagger"
)

// GoLib is the library archetype. Construct via Z5labs.GoLib.
type GoLib struct {
	// +private
	Source *dagger.Directory
	// +private
	LintConfig *dagger.File
}

// Ci runs the standardized check stages (fmt, vet, lint, test -race)
// against the supplied library source.
//
// +check
// +cache="session"
func (l *GoLib) Ci(ctx context.Context) error {
	return sharedCheck(ctx, l.Source, l.LintConfig)
}
