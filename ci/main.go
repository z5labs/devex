// Ci re-exposes each daggerverse module's tests.All() check on this root
// module. Discovery via `dagger check -l` only surfaces +check functions
// defined directly on the module being loaded; it does not transitively
// enumerate +check on dependencies. So each per-module suite gets a thin
// wrapper here, annotated +check, which the CI matrix can fan out one
// engine per row.
package main

import "context"

type Ci struct{}

// +check
func (c *Ci) CertificateManagementTestsAll(ctx context.Context) error {
	return dag.CertificateManagementTests().All(ctx)
}

// +check
func (c *Ci) CryptoTestsAll(ctx context.Context) error {
	return dag.CryptoTests().All(ctx)
}

// +check
func (c *Ci) DgraphTestsAll(ctx context.Context) error {
	return dag.DgraphTests().All(ctx)
}

// +check
func (c *Ci) EnvoyTestsAll(ctx context.Context) error {
	return dag.EnvoyTests().All(ctx)
}

// +check
func (c *Ci) GoTestsAll(ctx context.Context) error {
	return dag.GoTests().All(ctx)
}

// +check
func (c *Ci) GrafanaStackTestsAll(ctx context.Context) error {
	return dag.GrafanaStackTests().All(ctx)
}

// +check
func (c *Ci) KafkaTestsAll(ctx context.Context) error {
	return dag.KafkaTests().All(ctx)
}

// +check
func (c *Ci) OtelTestsAll(ctx context.Context) error {
	return dag.OtelTests().All(ctx)
}

// +check
func (c *Ci) RandomTestsAll(ctx context.Context) error {
	return dag.RandomTests().All(ctx)
}

// +check
func (c *Ci) Z5LabsTestsAll(ctx context.Context) error {
	return dag.Z5LabsTests().All(ctx)
}
