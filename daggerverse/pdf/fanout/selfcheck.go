package fanout

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// selfCheckTimeout bounds the cases that wait for concurrency to happen, so a
// bound that is too narrow fails as a timeout rather than hanging CI.
const selfCheckTimeout = 30 * time.Second

// SelfCheck verifies the properties a render depends on and no fixture can
// exercise: that every page runs, that the bound is honoured in both directions,
// that a failure in the middle is the error reported, and that it stops the
// pages behind it from starting. See the package comment for why this is a
// self-check rather than a test against a document.
//
// It is exposed as a Dagger check so a regression fails CI. The same cases run
// under `go test -race`, which is where the concurrency itself is checked.
func SelfCheck() error {
	var errs []error
	for _, c := range selfCheckCases() {
		if err := c.run(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.name, err))
		}
	}
	return errors.Join(errs...)
}

type selfCheckCase struct {
	name string
	run  func() error
}

func selfCheckCases() []selfCheckCase {
	return []selfCheckCase{
		{"every unit runs exactly once, positionally", checkEveryUnitRuns},
		{"no more than the bound run at once", checkBoundIsACeiling},
		{"the bound is reached", checkBoundIsReached},
		{"a failure in the middle is the error reported", checkMiddleFailureIsReported},
		{"the first failure stops the rest from starting", checkFailureStopsTheRest},
		{"a bound below one still renders", checkBoundBelowOne},
	}
}

// checkEveryUnitRuns pins the property the assembled directory rests on: each
// unit runs once, and writes to its own index.
func checkEveryUnitRuns() error {
	const count = 64

	got := make([]int, count)
	var runs atomic.Int64
	err := Run(context.Background(), 8, count, func(_ context.Context, i int) error {
		runs.Add(1)
		got[i] = i + 1
		return nil
	})
	if err != nil {
		return err
	}
	if n := runs.Load(); n != count {
		return fmt.Errorf("expected %d units to run, got %d", count, n)
	}
	for i, v := range got {
		if v != i+1 {
			return fmt.Errorf("unit %d wrote %d to its slot, expected %d", i, v, i+1)
		}
	}
	return nil
}

// checkBoundIsACeiling asserts the slot channel is a ceiling: never more than
// workers units in flight at once.
func checkBoundIsACeiling() error {
	const (
		workers = 3
		count   = 40
	)

	var live, peak atomic.Int64
	err := Run(context.Background(), workers, count, func(_ context.Context, _ int) error {
		n := live.Add(1)
		for {
			max := peak.Load()
			if n <= max || peak.CompareAndSwap(max, n) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		live.Add(-1)
		return nil
	})
	if err != nil {
		return err
	}
	if p := peak.Load(); p > workers {
		return fmt.Errorf("expected at most %d units in flight, saw %d", workers, p)
	}
	return nil
}

// checkBoundIsReached asserts the bound is also a floor — that the units really
// do run at the same time rather than one after another, which a ceiling alone
// would not distinguish.
//
// Every unit waits until all of them have arrived, so a fan-out narrower than
// the bound cannot finish: it fails as a timeout rather than as a wrong answer.
func checkBoundIsReached() error {
	const workers = 4

	var (
		mu      sync.Mutex
		arrived int
		all     = make(chan struct{})
	)
	ctx, cancel := context.WithTimeout(context.Background(), selfCheckTimeout)
	defer cancel()

	err := Run(ctx, workers, workers, func(ctx context.Context, _ int) error {
		mu.Lock()
		arrived++
		if arrived == workers {
			close(all)
		}
		mu.Unlock()

		select {
		case <-all:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("only %d of %d units ran at once", arrived, workers)
		}
	})
	if err != nil {
		return err
	}
	return nil
}

// checkMiddleFailureIsReported is the case the page name rests on: the error the
// caller sees is the failing unit's own, naming the page that failed rather than
// summarising that something did.
//
// The unit that fails is the fourth to *run* rather than the fourth by index,
// because which index reaches the pool first is the Go scheduler's business and
// not this package's — goroutines contend for a slot, they do not queue in
// order. With a bound of one that makes the case exactly deterministic: four
// units run, the fourth fails, and none of the remaining six starts.
func checkMiddleFailureIsReported() error {
	const (
		count  = 10
		breaks = 4
	)

	var (
		mu      sync.Mutex
		ran     []int
		failure error
	)
	err := Run(context.Background(), 1, count, func(_ context.Context, i int) error {
		mu.Lock()
		defer mu.Unlock()
		ran = append(ran, i)
		if len(ran) == breaks {
			failure = fmt.Errorf("page %d could not be rendered", i+1)
			return failure
		}
		return nil
	})

	mu.Lock()
	defer mu.Unlock()
	if failure == nil {
		return fmt.Errorf("expected %d units to run, got %d", breaks, len(ran))
	}
	if !errors.Is(err, failure) {
		return fmt.Errorf("expected the failing unit's own error %q, got: %v", failure, err)
	}
	if len(ran) != breaks {
		return fmt.Errorf(
			"expected the failure to stop the fan-out after %d units, got %d", breaks, len(ran))
	}
	return nil
}

// checkFailureStopsTheRest asserts a conversion that is going to fail does not
// render the rest of the document first.
//
// Every unit fails, so which one lands first does not matter — what is asserted
// is how many got to start at all. Nothing beyond a full pool can: a failing
// unit records its error and cancels while it still holds its slot, so the unit
// that inherits that slot finds the fan-out already cancelled.
func checkFailureStopsTheRest() error {
	const (
		workers = 4
		count   = 200
	)

	failure := errors.New("this page could not be rendered")
	var entered atomic.Int64
	err := Run(context.Background(), workers, count, func(_ context.Context, _ int) error {
		entered.Add(1)
		return failure
	})
	if !errors.Is(err, failure) {
		return fmt.Errorf("expected a failing unit's error, got: %v", err)
	}
	if n := entered.Load(); n > workers {
		return fmt.Errorf(
			"expected at most the %d units already in flight to run, got %d of %d",
			workers, n, count)
	}
	return nil
}

// checkBoundBelowOne asserts a nonsensical bound renders the document rather
// than deadlocking on a channel of no capacity. The module rejects such a bound
// long before this, by name; this is the floor under that.
func checkBoundBelowOne() error {
	const count = 5

	var runs atomic.Int64
	err := Run(context.Background(), 0, count, func(_ context.Context, _ int) error {
		runs.Add(1)
		return nil
	})
	if err != nil {
		return err
	}
	if n := runs.Load(); n != count {
		return fmt.Errorf("expected %d units to run, got %d", count, n)
	}
	return nil
}
