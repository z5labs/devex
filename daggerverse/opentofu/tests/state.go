package main

// Tests for the state-manipulation surface: the subcommands an operator
// reaches for once state and reality have diverged.
//
// The fixtures stay hermetic, which shapes what these can assert. The random
// provider's resources exist only in state, so "the resource survived being
// dropped from state" has no out-of-band object to check — what is checked
// instead is the state itself and the plan tofu makes from it, which is the
// same evidence an operator acts on.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// -------------------------------------------------------------- state list

// StateListReportsAppliedAddresses asserts StateList names what an Apply put
// under management, one address per line.
func (t *Tests) StateListReportsAppliedAddresses(ctx context.Context) error {
	state, err := applyBasic(ctx)
	if err != nil {
		return err
	}
	out, err := opentofu().Config(fixture("basic")).WithState(state).StateList(ctx)
	if err != nil {
		return fmt.Errorf("StateList: %w", err)
	}
	want := []string{"random_integer.port", "random_pet.name"}
	if got := listedAddresses(out); !slices.Equal(got, want) {
		return fmt.Errorf("expected StateList to report %v, got %v", want, got)
	}
	return nil
}

// StateListOnEmptyStateIsEmpty asserts a configuration with no state yet lists
// nothing rather than failing. tofu refuses a wholly absent state file; the
// module answers the question that was actually asked, which has an empty
// answer.
func (t *Tests) StateListOnEmptyStateIsEmpty(ctx context.Context) error {
	out, err := opentofu().Config(fixture("basic")).StateList(ctx)
	if err != nil {
		return fmt.Errorf("StateList against no state: %w", err)
	}
	if got := listedAddresses(out); len(got) != 0 {
		return fmt.Errorf("expected an empty listing, got %v", got)
	}
	return nil
}

// -------------------------------------------------------------- state show

// StateShowReturnsResourceJson asserts StateShow returns one parseable JSON
// document describing the requested resource — not the JSON *stream* tofu
// writes, whose first line is a UI message about the version it ran.
func (t *Tests) StateShowReturnsResourceJson(ctx context.Context) error {
	state, err := applyBasic(ctx)
	if err != nil {
		return err
	}
	raw, err := opentofu().Config(fixture("basic")).WithState(state).StateShow(ctx, "random_pet.name")
	if err != nil {
		return fmt.Errorf("StateShow: %w", err)
	}

	var doc showDocument
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return fmt.Errorf("parse the shown resource as JSON (%q): %w", raw, err)
	}
	resources := doc.Values.RootModule.Resources
	if len(resources) != 1 {
		return fmt.Errorf("expected exactly one resource in the document, got %d: %s", len(resources), raw)
	}
	if resources[0].Address != "random_pet.name" {
		return fmt.Errorf("expected random_pet.name, got %q", resources[0].Address)
	}
	id, _ := resources[0].Values["id"].(string)
	if !strings.HasPrefix(id, "devex-") {
		return fmt.Errorf("expected the shown resource to carry the applied pet name, got id %q", id)
	}
	return nil
}

// StateShowRejectsUnknownAddress asserts an address absent from state is an
// error naming it, rather than an empty document a caller could mistake for a
// resource with no attributes.
func (t *Tests) StateShowRejectsUnknownAddress(ctx context.Context) error {
	state, err := applyBasic(ctx)
	if err != nil {
		return err
	}
	_, err = opentofu().Config(fixture("basic")).WithState(state).StateShow(ctx, "random_pet.missing")
	return expectErrorContains(err, "tofu state show", "random_pet.missing")
}

// ---------------------------------------------------------------- state rm

// StateRmDropsAddressFromState asserts StateRm removes the address from state
// and nothing else: the resulting state omits it, still tracks its neighbour,
// and a plan afterwards proposes creating it again — which is exactly the
// hazard of dropping something whose object is still out there.
func (t *Tests) StateRmDropsAddressFromState(ctx context.Context) error {
	state, err := applyBasic(ctx)
	if err != nil {
		return err
	}
	result, err := pin(ctx, opentofu().
		Config(fixture("basic")).
		WithState(state).
		StateRm([]string{"random_pet.name"}))
	if err != nil {
		return fmt.Errorf("StateRm: %w", err)
	}
	after, err := decodeState(ctx, result.File(stateFileName))
	if err != nil {
		return err
	}
	want := []string{"random_integer.port"}
	if got := after.addresses(); !slices.Equal(got, want) {
		return fmt.Errorf("expected the remaining state to track %v, got %v", want, got)
	}

	plan, err := pin(ctx, opentofu().
		Config(fixture("basic")).
		WithState(result.File(stateFileName)).
		Plan())
	if err != nil {
		return fmt.Errorf("Plan after StateRm: %w", err)
	}
	decoded, err := decodePlan(ctx, plan)
	if err != nil {
		return err
	}
	return expectActions(decoded, map[string][]string{
		"random_pet.name":     {"create"},
		"random_integer.port": {"no-op"},
	})
}

// StateRmRejectsUnknownAddress asserts an address absent from state fails,
// naming it.
func (t *Tests) StateRmRejectsUnknownAddress(ctx context.Context) error {
	state, err := applyBasic(ctx)
	if err != nil {
		return err
	}
	_, err = opentofu().
		Config(fixture("basic")).
		WithState(state).
		StateRm([]string{"random_pet.missing"}).
		Sync(ctx)
	return expectErrorContains(err, "tofu state rm", "random_pet.missing")
}

// ---------------------------------------------------------------- state mv

// StateMvRenamesAddress asserts a resource renamed in state stays the same
// resource: a plan against the configuration that renamed it to match reports
// no changes, where without the move tofu would destroy and recreate.
func (t *Tests) StateMvRenamesAddress(ctx context.Context) error {
	state, err := applyBasic(ctx)
	if err != nil {
		return err
	}
	result, err := pin(ctx, opentofu().
		Config(fixture("basic")).
		WithState(state).
		StateMv("random_pet.name", "random_pet.renamed"))
	if err != nil {
		return fmt.Errorf("StateMv: %w", err)
	}
	moved, err := decodeState(ctx, result.File(stateFileName))
	if err != nil {
		return err
	}
	want := []string{"random_integer.port", "random_pet.renamed"}
	if got := moved.addresses(); !slices.Equal(got, want) {
		return fmt.Errorf("expected the moved state to track %v, got %v", want, got)
	}

	plan, err := pin(ctx, opentofu().
		Config(fixture("renamed")).
		WithState(result.File(stateFileName)).
		Plan())
	if err != nil {
		return fmt.Errorf("Plan against the renamed configuration: %w", err)
	}
	changes, err := plan.File(changesName).Contents(ctx)
	if err != nil {
		return fmt.Errorf("read %s: %w", changesName, err)
	}
	if changes != "none" {
		text, _ := plan.File(planTextName).Contents(ctx)
		return fmt.Errorf("expected changes=%q after the move, got %q:\n%s", "none", changes, text)
	}
	return nil
}

// StateMvRejectsUnknownAddress asserts a source address absent from state
// fails, naming it.
func (t *Tests) StateMvRejectsUnknownAddress(ctx context.Context) error {
	state, err := applyBasic(ctx)
	if err != nil {
		return err
	}
	_, err = opentofu().
		Config(fixture("basic")).
		WithState(state).
		StateMv("random_pet.missing", "random_pet.renamed").
		Sync(ctx)
	return expectErrorContains(err, "tofu state mv", "random_pet.missing")
}

// ------------------------------------------------------------------ import

// ImportBringsResourceUnderManagement asserts an object named by id lands in
// state under the declared address, and that a plan afterwards is empty —
// which it is only if the import recorded the object rather than something
// approximating it.
func (t *Tests) ImportBringsResourceUnderManagement(ctx context.Context) error {
	result, err := pin(ctx, opentofu().
		Config(fixture("importable")).
		Import("random_integer.adopted", importedInteger))
	if err != nil {
		return fmt.Errorf("Import: %w", err)
	}
	imported, err := decodeState(ctx, result.File(stateFileName))
	if err != nil {
		return err
	}
	want := []string{"random_integer.adopted"}
	if got := imported.addresses(); !slices.Equal(got, want) {
		return fmt.Errorf("expected the imported state to track %v, got %v", want, got)
	}

	plan, err := pin(ctx, opentofu().
		Config(fixture("importable")).
		WithState(result.File(stateFileName)).
		Plan())
	if err != nil {
		return fmt.Errorf("Plan after Import: %w", err)
	}
	changes, err := plan.File(changesName).Contents(ctx)
	if err != nil {
		return fmt.Errorf("read %s: %w", changesName, err)
	}
	if changes != "none" {
		text, _ := plan.File(planTextName).Contents(ctx)
		return fmt.Errorf("expected changes=%q after the import, got %q:\n%s", "none", changes, text)
	}
	return nil
}

// ImportRejectsAddressAbsentFromConfiguration asserts import writes state and
// not HCL: an address the configuration never declares fails, naming it.
func (t *Tests) ImportRejectsAddressAbsentFromConfiguration(ctx context.Context) error {
	_, err := opentofu().
		Config(fixture("importable")).
		Import("random_integer.missing", importedInteger).
		Sync(ctx)
	return expectErrorContains(err, "tofu import", "random_integer.missing")
}

// ----------------------------------------------------------------- refresh

// RefreshUpdatesStateWithoutChanges asserts a refresh-only apply hands back
// state that still tracks everything it was given and still matches the
// configuration. With hermetic fixtures there is no reality to drift, so what
// is proved is that the refresh round-trips state rather than rewriting it.
func (t *Tests) RefreshUpdatesStateWithoutChanges(ctx context.Context) error {
	state, err := applyBasic(ctx)
	if err != nil {
		return err
	}
	result, err := pin(ctx, opentofu().Config(fixture("basic")).WithState(state).Refresh())
	if err != nil {
		return fmt.Errorf("Refresh: %w", err)
	}
	refreshed, err := decodeState(ctx, result.File(stateFileName))
	if err != nil {
		return err
	}
	want := []string{"random_integer.port", "random_pet.name"}
	if got := refreshed.addresses(); !slices.Equal(got, want) {
		return fmt.Errorf("expected the refreshed state to track %v, got %v", want, got)
	}
	log, err := result.File("refresh.log").Contents(ctx)
	if err != nil {
		return fmt.Errorf("read refresh.log: %w", err)
	}
	if !strings.Contains(log, "Apply complete!") {
		return fmt.Errorf("expected the refresh log to record completion, got:\n%s", log)
	}

	plan, err := pin(ctx, opentofu().
		Config(fixture("basic")).
		WithState(result.File(stateFileName)).
		Plan())
	if err != nil {
		return fmt.Errorf("Plan after Refresh: %w", err)
	}
	changes, err := plan.File(changesName).Contents(ctx)
	if err != nil {
		return fmt.Errorf("read %s: %w", changesName, err)
	}
	if changes != "none" {
		text, _ := plan.File(planTextName).Contents(ctx)
		return fmt.Errorf("expected changes=%q after a refresh, got %q:\n%s", "none", changes, text)
	}
	return nil
}

// ------------------------------------------------------------ taint/untaint

// TaintProposesReplacement asserts a tainted resource is planned for
// replacement — destroyed and created again — while its neighbour is left
// alone.
func (t *Tests) TaintProposesReplacement(ctx context.Context) error {
	state, err := applyBasic(ctx)
	if err != nil {
		return err
	}
	tainted, err := pin(ctx, opentofu().
		Config(fixture("basic")).
		WithState(state).
		Taint("random_pet.name"))
	if err != nil {
		return fmt.Errorf("Taint: %w", err)
	}
	plan, err := pin(ctx, opentofu().
		Config(fixture("basic")).
		WithState(tainted.File(stateFileName)).
		Plan())
	if err != nil {
		return fmt.Errorf("Plan after Taint: %w", err)
	}
	decoded, err := decodePlan(ctx, plan)
	if err != nil {
		return err
	}
	return expectActions(decoded, map[string][]string{
		"random_pet.name":     {"delete", "create"},
		"random_integer.port": {"no-op"},
	})
}

// UntaintClearsReplacement asserts Untaint reverses Taint: the same state,
// taken through both, plans no changes at all.
func (t *Tests) UntaintClearsReplacement(ctx context.Context) error {
	state, err := applyBasic(ctx)
	if err != nil {
		return err
	}
	tainted, err := pin(ctx, opentofu().
		Config(fixture("basic")).
		WithState(state).
		Taint("random_pet.name"))
	if err != nil {
		return fmt.Errorf("Taint: %w", err)
	}
	cleared, err := pin(ctx, opentofu().
		Config(fixture("basic")).
		WithState(tainted.File(stateFileName)).
		Untaint("random_pet.name"))
	if err != nil {
		return fmt.Errorf("Untaint: %w", err)
	}
	plan, err := pin(ctx, opentofu().
		Config(fixture("basic")).
		WithState(cleared.File(stateFileName)).
		Plan())
	if err != nil {
		return fmt.Errorf("Plan after Untaint: %w", err)
	}
	changes, err := plan.File(changesName).Contents(ctx)
	if err != nil {
		return fmt.Errorf("read %s: %w", changesName, err)
	}
	if changes != "none" {
		text, _ := plan.File(planTextName).Contents(ctx)
		return fmt.Errorf("expected changes=%q after Untaint, got %q:\n%s", "none", changes, text)
	}
	return nil
}

// ------------------------------------------------------------------- graph

// GraphRendersDependencyGraph asserts Graph returns DOT a reader could parse —
// one balanced `digraph` — naming both resources the configuration declares
// and the variable one of them depends on.
func (t *Tests) GraphRendersDependencyGraph(ctx context.Context) error {
	dot, err := opentofu().Config(fixture("basic")).Graph(ctx)
	if err != nil {
		return fmt.Errorf("Graph: %w", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(dot), "digraph") {
		return fmt.Errorf("expected a digraph, got:\n%s", dot)
	}
	if err := checkBalancedBraces(dot); err != nil {
		return fmt.Errorf("%w:\n%s", err, dot)
	}
	for _, want := range []string{"random_pet.name", "random_integer.port", "var.prefix"} {
		if !strings.Contains(dot, want) {
			return fmt.Errorf("expected the graph to name %s, got:\n%s", want, dot)
		}
	}
	return nil
}

// ------------------------------------------------------------------ helpers

// importedInteger is the id ImportBringsResourceUnderManagement adopts:
// random_integer's id is `result,min,max`, and these bounds are the ones the
// importable fixture declares. A mismatch would show up as a plan proposing to
// replace what was just imported.
const importedInteger = "42,1,100"

// expectActions asserts a plan proposes exactly the given action per address —
// `no-op` for the resources it leaves alone included, since those appear in
// the plan too.
func expectActions(plan planDocument, want map[string][]string) error {
	got := plan.actions()
	if len(got) != len(want) {
		return fmt.Errorf("expected a plan covering %v, got %v", want, got)
	}
	for address, actions := range want {
		if !slices.Equal(got[address], actions) {
			return fmt.Errorf("expected %s to be %v, got %v", address, actions, got[address])
		}
	}
	return nil
}

// listedAddresses splits `tofu state list` output into addresses, sorted so
// assertions do not depend on tofu's ordering.
func listedAddresses(out string) []string {
	var addresses []string
	for line := range strings.SplitSeq(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			addresses = append(addresses, line)
		}
	}
	slices.Sort(addresses)
	return addresses
}

// showDocument is the slice of `tofu state show -json` the assertions read.
type showDocument struct {
	FormatVersion string `json:"format_version"`
	Values        struct {
		RootModule struct {
			Resources []struct {
				Address string         `json:"address"`
				Values  map[string]any `json:"values"`
			} `json:"resources"`
		} `json:"root_module"`
	} `json:"values"`
}

// checkBalancedBraces is the parseability check GraphRendersDependencyGraph
// applies to the DOT: every block opened is closed, and none is closed that
// was never opened. Braces inside quoted labels are skipped — DOT escapes a
// quote with a backslash, which the graph's own provider labels use.
func checkBalancedBraces(dot string) error {
	depth := 0
	quoted := false
	escaped := false
	for _, r := range dot {
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
		case r == '"':
			quoted = !quoted
		case quoted:
		case r == '{':
			depth++
		case r == '}':
			depth--
			if depth < 0 {
				return fmt.Errorf("expected balanced DOT, found a stray %q", "}")
			}
		}
	}
	if quoted {
		return fmt.Errorf("expected balanced DOT, found an unterminated quoted string")
	}
	if depth != 0 {
		return fmt.Errorf("expected balanced DOT, found %d unclosed %q", depth, "{")
	}
	return nil
}
