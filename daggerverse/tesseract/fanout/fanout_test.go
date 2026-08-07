package fanout

import "testing"

// TestSelfCheck runs the cases the tesseract module exposes as a Dagger check,
// so the same assertions are covered here under `go test -race` — which is where
// the scheduling itself is checked, the race detector being the only thing that
// sees a concurrent write to a shared slot.
func TestSelfCheck(t *testing.T) {
	if err := SelfCheck(); err != nil {
		t.Fatal(err)
	}
}

// TestCases runs each case on its own so a failure names itself, which
// SelfCheck's joined error cannot do as clearly from a CI log.
func TestCases(t *testing.T) {
	for _, c := range selfCheckCases() {
		t.Run(c.name, func(t *testing.T) {
			if err := c.run(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
