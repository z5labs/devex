package refstore

import (
	"encoding/hex"
	"strings"
	"testing"
)

// TestSelfCheck runs the same cases a consumer's CI runs through the module's
// memo-store-self-test check, so `go test ./...` and the check cannot disagree
// about whether this store works.
func TestSelfCheck(t *testing.T) {
	if err := SelfCheck(); err != nil {
		t.Fatal(err)
	}
}

// TestScopeIsReversible pins the one property the hex encoding is chosen for
// besides safety: an operator looking at a ref in the store can work out which
// branch recorded it.
func TestScopeIsReversible(t *testing.T) {
	for _, ref := range []string{
		"refs/heads/main",
		"refs/pull/12/merge",
		"refs/heads/feature/a b?c*",
	} {
		got := Scope(ref)
		if strings.ContainsAny(got, "/%.") {
			t.Errorf("Scope(%q) = %q, which is not a single safe path component", ref, got)
		}
		back, err := hex.DecodeString(got)
		if err != nil || string(back) != ref {
			t.Errorf("Scope(%q) = %q, which decodes to %q (%v)", ref, got, back, err)
		}
	}
}

// TestScopesDoNotNest is the encoding's whole point: one ref's entries must be
// unreachable from another ref's listing, including when one branch name extends
// another.
func TestScopesDoNotNest(t *testing.T) {
	if a, b := prefix("refs/heads/main"), prefix("refs/heads/main-2"); strings.HasPrefix(b, a) {
		t.Fatalf("%q is a prefix of %q, so listing one scope would read the other", a, b)
	}
	if a, b := prefix("refs/heads/main"), prefix("refs/heads/main/feature"); strings.HasPrefix(b, a) {
		t.Fatalf("%q is a prefix of %q, so listing one scope would read the other", a, b)
	}
}
