package main

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"dagger/tests/internal/dagger"
)

// ------------------------------------------------------------- state modes

// StateAndBackendConfigAreRejected asserts the two state strategies cannot be
// combined, and that the rejection names both of them rather than leaving the
// caller to guess which one silently won.
func (t *Tests) StateAndBackendConfigAreRejected(ctx context.Context) error {
	_, err := opentofu().
		Config(fixture("basic")).
		WithState(emptyState()).
		WithBackendConfig("bucket", "devex-does-not-exist").
		Plan().
		Sync(ctx)
	return expectErrorContains(err, "WithState and WithBackendConfig", "mutually exclusive", "remote backend")
}

// StateAndBackendConfigFileAreRejected asserts the file form of the backend
// settings is rejected alongside WithState too — the check is on the mode,
// not on one particular modifier.
func (t *Tests) StateAndBackendConfigFileAreRejected(ctx context.Context) error {
	_, err := opentofu().
		Config(fixture("basic")).
		WithState(emptyState()).
		WithBackendConfigFile(newFile("backend.hcl", "bucket = \"devex-does-not-exist\"\n")).
		Plan().
		Sync(ctx)
	return expectErrorContains(err, "WithState and WithBackendConfig", "mutually exclusive", "remote backend")
}

// DestroyWithoutStateOrBackendIsRejected asserts a destroy with nothing to
// destroy is an error. tofu itself would report "0 destroyed" and exit 0,
// which reads as a successful teardown while the real infrastructure — whose
// state was never supplied — stays up.
func (t *Tests) DestroyWithoutStateOrBackendIsRejected(ctx context.Context) error {
	_, err := opentofu().
		Config(fixture("basic")).
		Destroy().
		Sync(ctx)
	return expectErrorContains(err, "no state to destroy", "WithState", "remote backend")
}

// WithVarRejectsNameContainingEquals asserts the deferred validation on the
// `name=value` flags fires. `-var a=b=c` would set a different variable than
// the caller asked for.
func (t *Tests) WithVarRejectsNameContainingEquals(ctx context.Context) error {
	_, err := opentofu().
		Config(fixture("basic")).
		WithVar("prefix=oops", "value").
		Plan().
		Sync(ctx)
	return expectErrorContains(err, "WithVar", "must not contain")
}

// ApplyRejectsTargetsWithSavedPlan asserts the two ways of narrowing an apply
// cannot be combined: a saved plan already fixes what it changes, and tofu
// rejects -target alongside one.
func (t *Tests) ApplyRejectsTargetsWithSavedPlan(ctx context.Context) error {
	_, err := opentofu().
		Config(fixture("basic")).
		Apply(dagger.OpentofuConfigApplyOpts{
			Plan:    newFile(planFileName, "not a real plan"),
			Targets: []string{"random_pet.name"},
		}).
		Sync(ctx)
	return expectErrorContains(err, "targets cannot be combined with a saved plan")
}

// ------------------------------------------------------------------ fmt

// FmtAcceptsFormattedConfiguration asserts a canonically formatted root
// module passes the check and reports no diff.
func (t *Tests) FmtAcceptsFormattedConfiguration(ctx context.Context) error {
	out, err := opentofu().Config(fixture("basic")).Fmt(ctx)
	if err != nil {
		return fmt.Errorf("Fmt on a formatted configuration: %w", err)
	}
	if strings.TrimSpace(out) != "" {
		return fmt.Errorf("expected no diff, got:\n%s", out)
	}
	return nil
}

// FmtReportsUnformattedConfiguration asserts drift fails the check and that
// the diff survives into the error — Dagger drops a function's value whenever
// its error is non-nil, so the error text is the only place it can live.
func (t *Tests) FmtReportsUnformattedConfiguration(ctx context.Context) error {
	_, err := opentofu().Config(fixture("unformatted")).Fmt(ctx)
	return expectErrorContains(err, "not formatted", "main.tf", "separator")
}

// ---------------------------------------------------------------- format

// FormatRewritesUnformattedConfiguration asserts Format returns the corrected
// tree: the rewritten file differs from the input, and the result passes the
// check-only Fmt that the input fails.
func (t *Tests) FormatRewritesUnformattedConfiguration(ctx context.Context) error {
	src := fixture("unformatted")
	before, err := src.File("main.tf").Contents(ctx)
	if err != nil {
		return fmt.Errorf("read the unformatted fixture: %w", err)
	}

	formatted := opentofu().Config(src).Format()
	after, err := formatted.File("main.tf").Contents(ctx)
	if err != nil {
		return fmt.Errorf("Format: %w", err)
	}
	if after == before {
		return fmt.Errorf("expected Format to rewrite the configuration, got it back unchanged:\n%s", after)
	}
	if _, err := opentofu().Config(formatted).Fmt(ctx); err != nil {
		return fmt.Errorf("expected the formatted tree to pass the fmt check: %w", err)
	}
	return nil
}

// FormatLeavesFormattedConfigurationUnchanged asserts Format is a no-op on a
// canonically formatted root module — byte-identical in, byte-identical out.
func (t *Tests) FormatLeavesFormattedConfigurationUnchanged(ctx context.Context) error {
	src := fixture("basic")
	before, err := src.File("main.tf").Contents(ctx)
	if err != nil {
		return fmt.Errorf("read the basic fixture: %w", err)
	}
	after, err := opentofu().Config(src).Format().File("main.tf").Contents(ctx)
	if err != nil {
		return fmt.Errorf("Format: %w", err)
	}
	if after != before {
		return fmt.Errorf("expected a formatted configuration to come back unchanged, got:\n%s", after)
	}
	return nil
}

// FormatLeavesInputDirectoryUntouched asserts the rewrite lands in the
// returned copy and nowhere else: the directory handed to Config still reads
// as it did before, so a caller decides for itself whether to export over its
// working copy.
func (t *Tests) FormatLeavesInputDirectoryUntouched(ctx context.Context) error {
	src := fixture("unformatted")
	before, err := src.File("main.tf").Contents(ctx)
	if err != nil {
		return fmt.Errorf("read the unformatted fixture: %w", err)
	}
	if _, err := opentofu().Config(src).Format().Sync(ctx); err != nil {
		return fmt.Errorf("Format: %w", err)
	}
	after, err := src.File("main.tf").Contents(ctx)
	if err != nil {
		return fmt.Errorf("re-read the unformatted fixture: %w", err)
	}
	if after != before {
		return fmt.Errorf("expected the input directory to survive Format untouched, got:\n%s", after)
	}
	return nil
}

// FormatDropsCarriedState asserts the returned tree is the configuration and
// nothing else: file-carried state is written into the container's copy of the
// root module, and formatting is no reason to hand it back for the caller to
// export over their working copy.
func (t *Tests) FormatDropsCarriedState(ctx context.Context) error {
	entries, err := opentofu().
		Config(fixture("basic")).
		WithState(emptyState()).
		Format().
		Entries(ctx)
	if err != nil {
		return fmt.Errorf("Format with file-carried state: %w", err)
	}
	if slices.Contains(entries, stateFileName) {
		return fmt.Errorf("expected %s to stay out of the formatted tree, got %v", stateFileName, entries)
	}
	return nil
}

// ------------------------------------------------------------- validate

// ValidateAcceptsValidConfiguration asserts a well-formed root module
// validates.
func (t *Tests) ValidateAcceptsValidConfiguration(ctx context.Context) error {
	if err := opentofu().Config(fixture("basic")).Validate(ctx); err != nil {
		return fmt.Errorf("Validate on a valid configuration: %w", err)
	}
	return nil
}

// ValidateRejectsInvalidConfiguration asserts a broken reference fails, and
// that tofu's own diagnostic reaches the caller rather than a bare exit code.
func (t *Tests) ValidateRejectsInvalidConfiguration(ctx context.Context) error {
	err := opentofu().Config(fixture("invalid")).Validate(ctx)
	return expectErrorContains(err, "tofu validate", "random_pet", "missing")
}

// ValidateWorksWithoutBackendCredentials asserts a configuration declaring a
// remote backend validates with no credentials and no reachable bucket:
// Validate initialises with -backend=false, so the backend is never contacted.
func (t *Tests) ValidateWorksWithoutBackendCredentials(ctx context.Context) error {
	if err := opentofu().Config(fixture("remote-backend")).Validate(ctx); err != nil {
		return fmt.Errorf("Validate on a remote-backend configuration: %w", err)
	}
	return nil
}
