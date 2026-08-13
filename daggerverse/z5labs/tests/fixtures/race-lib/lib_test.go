package racelib

import "testing"

// TestAddConcurrently writes to one variable from two goroutines with no
// synchronization between the writes. The channel close orders the
// goroutine's write before the receive in the test, but nothing orders it
// against the test's own write, so the two are concurrent whatever the
// scheduler does — the race is structural rather than a timing window, and
// the detector reports it on every run.
//
// Without -race this passes: the sum is never asserted, only that
// something accumulated.
func TestAddConcurrently(t *testing.T) {
	total := 0
	done := make(chan struct{})

	go func() {
		total = Add(total, 1)
		close(done)
	}()

	total = Add(total, 1)
	<-done

	if total == 0 {
		t.Fatalf("expected a non-zero total, got %d", total)
	}
}
