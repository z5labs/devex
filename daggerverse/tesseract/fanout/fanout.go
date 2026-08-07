// Package fanout runs a bounded, fail-fast set of concurrent units of work, and
// partitions a batch's images into the units that recognise them.
//
// RunSlices is the entry point and does both in one call, taking the bound once.
// Run and Partition are the primitives it is built from and are exported because
// each is meaningful alone — Run for work that is already a flat count of units,
// Partition for a caller that needs the boundaries themselves — but a caller
// passing the same bound to both wants RunSlices.
//
// It is a package of its own, importing no dagger, so the scheduling can be
// checked with `go test -race` and with no engine at all. That matters most for
// the two properties a fixture cannot reach. The bound as a *floor* needs units
// that block until every one of them has arrived, which no recognition does; and
// the exec count staying off the image count is a claim about three thousand
// images, which is not a suite's fixture — the containerd fold ceiling this
// partitioning exists for was hit at around four hundred and forty, so the
// smallest fixture that would demonstrate it is already far past what any check
// should recognise.
//
// This is a copy of daggerverse/pdf/fanout, deliberately and temporarily. Each
// daggerverse module is its own Go module whose Dagger context is its own
// directory, so neither can import the other's packages; devex#321 is the issue
// that lifts one shared fan-out out of both. Keeping the two identical until then
// is what makes that lift a move rather than a merge, so a change here belongs in
// both copies.
package fanout

import (
	"context"
	"sync"
)

// Run performs count units of work, at most workers of them at a time, and
// returns the first error any of them reported.
//
// The bound is a slot channel rather than an unbounded fan-out because a
// three-thousand-image batch is three thousand containers otherwise, and because
// a machine whose cores are already busy gains nothing from being asked for more
// of them at once.
//
// The first failure cancels the rest: the context handed to run is cancelled as
// soon as any unit fails, and a unit that has not yet taken a slot never starts.
// Work already in flight is not interrupted beyond that cancellation — a
// recognition that ignores its context runs to completion — so this bounds what
// is *started* rather than what is finished.
//
// run is called with the index of its unit and may write to distinct elements of
// a caller-owned slice without further synchronisation: no two calls share an
// index, and Run does not return until every call it started has returned.
func Run(ctx context.Context, workers, count int, run func(ctx context.Context, i int) error) error {
	if workers < 1 {
		workers = 1
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	slots := make(chan struct{}, workers)

	for i := range count {
		wg.Go(func() {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				// Cancelled before this unit ever started. The failure that
				// cancelled it is the one worth reporting, and it is already
				// recorded, so this contributes nothing.
				return
			}

			// Taking a slot is not enough to have won the race. A select whose
			// cases are both ready picks between them at random, so a unit
			// offered a free slot — which the failing unit has just released —
			// takes it as readily as it observes the cancellation. Without this
			// second look a failure stops the *queue* but still lets the units
			// behind it start, which is the whole property this exists for.
			if ctx.Err() != nil {
				return
			}

			if err := run(ctx, i); err != nil {
				mu.Lock()
				defer mu.Unlock()
				if first == nil {
					first = err
					cancel()
				}
			}
		})
	}
	wg.Wait()

	return first
}
