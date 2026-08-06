// Package fanout runs a bounded, fail-fast set of concurrent units of work, and
// partitions a document's pages into the units that run them.
//
// It is a package of its own, importing no dagger, for a reason that is not
// tidiness: it is the only way these properties are covered at all. poppler will
// not produce the failure that exercises them. Measured against poppler 25.12 —
// what the module's pinned Alpine tag ships — a page it cannot draw is a warning
// on stderr and an exit status of 0: a dangling page-tree kid, a kid of the
// wrong type, a loop in the page tree, a null kid, a two-million-point MediaBox
// and a resolution large enough to print `Bogus memory allocation size` all
// render a file and succeed. The only non-zero exits are for a document that
// cannot be opened at all and for a page range outside it, and both are caught
// before any page is rendered. So no fixture makes one page of a render fail,
// and the scheduling has to be tested where it lives instead — here, with
// `go test -race` and no engine, and in CI through SelfCheck.
package fanout

import (
	"context"
	"sync"
)

// Run performs count units of work, at most workers of them at a time, and
// returns the first error any of them reported.
//
// The bound is a slot channel rather than an unbounded fan-out because a
// three-thousand-page document is three thousand containers otherwise, and
// because a machine whose cores are already busy gains nothing from being asked
// for more of them at once.
//
// The first failure cancels the rest: the context handed to run is cancelled as
// soon as any unit fails, and a unit that has not yet taken a slot never starts.
// Work already in flight is not interrupted beyond that cancellation — a render
// that ignores its context runs to completion — so this bounds what is *started*
// rather than what is finished.
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
