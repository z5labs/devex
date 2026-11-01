package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"dagger/tests/internal/dagger"
)

// boundPrefix is the value the tested-var fixture's test file requires of its
// prefix variable, which has no default.
const boundPrefix = "from-bound-var"

// failingAssertion and unboundPrefix are the markers the failing fixtures put
// in their assertions' error_message, so a test can tell tofu's report of the
// assertion apart from any other failure.
const (
	failingAssertion = "devex-failing-assertion"
	unboundPrefix    = "devex-unbound-prefix"
)

// nonceMessage pulls the UUID the tested-nonce fixture's failing assertion
// reports.
var nonceMessage = regexp.MustCompile(`devex-nonce=([0-9a-f-]{36})`)

// ------------------------------------------------------------------- test

// TestPassesWithMockProvider asserts a module whose test runs against a
// mock_provider passes, and that Test returns tofu's report of it.
func (t *Tests) TestPassesWithMockProvider(ctx context.Context) error {
	out, err := opentofu().Config(fixture("tested")).Test(ctx)
	if err != nil {
		return fmt.Errorf("Test on a module whose mocked test passes: %w", err)
	}
	for _, want := range []string{"names_the_pet", "1 passed, 0 failed"} {
		if !strings.Contains(out, want) {
			return fmt.Errorf("expected the test report to mention %q, got:\n%s", want, out)
		}
	}
	return nil
}

// TestFailsOnFailingAssertion asserts a failing assertion fails Test, and that
// the error carries tofu's own output — the assertion's message included —
// rather than a bare exit code.
func (t *Tests) TestFailsOnFailingAssertion(ctx context.Context) error {
	_, err := opentofu().Config(fixture("tested-failing")).Test(ctx)
	return expectErrorContains(err, "tofu test failed", failingAssertion, "words, not 3")
}

// TestReceivesBoundVariables asserts every way of binding a variable to a
// Config reaches `tofu test`: -var, -var-file, a secret bound as TF_VAR_<name>,
// and a plain environment variable. The fixture's variable has no default, and
// the first case proves an unbound run fails, so a pass below is the bound
// value arriving rather than the assertion being vacuous.
func (t *Tests) TestReceivesBoundVariables(ctx context.Context) error {
	_, err := opentofu().Config(fixture("tested-var")).Test(ctx)
	if err == nil {
		return fmt.Errorf("expected Test to fail with the prefix variable unbound, got a pass")
	}
	if !strings.Contains(err.Error(), "prefix") {
		return fmt.Errorf("expected the unbound run to fail over the prefix variable, got: %v", err)
	}

	id, err := dag.Random().UUIDV4(ctx)
	if err != nil {
		return fmt.Errorf("name a test secret: %w", err)
	}
	secret := dag.SetSecret("opentofu-test-prefix-"+id, boundPrefix)

	base := opentofu().Config(fixture("tested-var"))
	cases := []struct {
		name string
		cfg  *dagger.OpentofuConfig
	}{
		{"WithVar", base.WithVar("prefix", boundPrefix)},
		{"WithVarFile", base.WithVarFile(newFile("test.tfvars", "prefix = \""+boundPrefix+"\"\n"))},
		{"WithSecretVar", base.WithSecretVar("prefix", secret)},
		{"WithEnvVariable", base.WithEnvVariable("TF_VAR_prefix", boundPrefix)},
	}
	for _, c := range cases {
		if _, err := c.cfg.Test(ctx); err != nil {
			return fmt.Errorf("Test with the prefix bound through %s: %w", c.name, err)
		}
	}
	return nil
}

// TestShouldNotBeCached asserts two consecutive runs of Test genuinely re-run
// tofu. The tested-nonce fixture fails on purpose with a fresh UUID in its
// assertion's message, so two failures carrying the same UUID would mean the
// second run was served from a cache.
func (t *Tests) TestShouldNotBeCached(ctx context.Context) error {
	first, err := testNonce(ctx)
	if err != nil {
		return err
	}
	second, err := testNonce(ctx)
	if err != nil {
		return err
	}
	if first == second {
		return fmt.Errorf("expected two test runs to re-execute, both reported %q", first)
	}
	return nil
}

// -------------------------------------------------------------------- ci

// CiCheckWithTestPasses asserts the test stage passes a module whose mocked
// test passes, beside the fmt and validate stages.
func (t *Tests) CiCheckWithTestPasses(ctx context.Context) error {
	err := opentofu().
		Config(fixture("tested")).
		Ci().
		WithFmt().
		WithValidate().
		WithTest().
		Check(ctx)
	if err != nil {
		return fmt.Errorf("Ci.Check with the test stage on a passing module: %w", err)
	}
	return nil
}

// CiCheckWithTestReportsFailingTest asserts the test stage gates the pipeline,
// and that its error joins the other stages' rather than replacing them: the
// tested-failing fixture is unformatted too, so the fmt stage's failure has to
// be reported alongside the failed assertion.
func (t *Tests) CiCheckWithTestReportsFailingTest(ctx context.Context) error {
	err := opentofu().
		Config(fixture("tested-failing")).
		Ci().
		WithFmt().
		WithTest().
		Check(ctx)
	return expectErrorContains(err, "tofu test failed", failingAssertion, "not formatted")
}

// CiCheckRunsTestStageAlone asserts WithTest is enough on its own to make a
// pipeline: a Check with only the test stage enabled runs it rather than
// rejecting the pipeline as empty.
func (t *Tests) CiCheckRunsTestStageAlone(ctx context.Context) error {
	err := opentofu().Config(fixture("tested-failing")).Ci().WithTest().Check(ctx)
	if err != nil && strings.Contains(err.Error(), "no stages enabled") {
		return fmt.Errorf("expected WithTest alone to enable a stage, got: %v", err)
	}
	return expectErrorContains(err, "tofu test failed", failingAssertion)
}

// ------------------------------------------------------------------ helpers

// testNonce runs the tested-nonce fixture's deliberately failing test and
// returns the UUID its assertion reported.
func testNonce(ctx context.Context) (string, error) {
	_, err := opentofu().Config(fixture("tested-nonce")).Test(ctx)
	if err == nil {
		return "", fmt.Errorf("expected the tested-nonce fixture's test to fail, got a pass")
	}
	m := nonceMessage.FindStringSubmatch(err.Error())
	if m == nil {
		return "", fmt.Errorf("expected the failure to report a nonce, got: %v", err)
	}
	return m[1], nil
}
