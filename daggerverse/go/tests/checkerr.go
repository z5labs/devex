package main

import (
	"context"
	"errors"

	"dagger/tests/internal/dagger"
)

// checkErr runs a dependency's check and returns its failure as an error.
//
// Since Dagger v1.0.0-beta.15 a +check function reaches a consumer as a
// *dagger.Check, a deferred check, rather than as the error it returns, so the
// failure has to be read back off it. The message is the check's own error
// text, so the assertions below still match on it.
func checkErr(ctx context.Context, check *dagger.Check) error {
	failure, err := check.Error(ctx)
	if err != nil {
		return err
	}
	if failure == nil {
		return nil
	}
	msg, err := failure.Message(ctx)
	if err != nil {
		return err
	}
	return errors.New(msg)
}
