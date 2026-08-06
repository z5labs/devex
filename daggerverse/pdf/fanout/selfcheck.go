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
// that a failure in the middle is the error reported, that it stops the pages
// behind it from starting, and that a document of any length partitions into no
// more slices than the bound allows. See the package comment for why this is a
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
		{"a partition covers every page once, in order", checkPartitionCoversEveryUnit},
		{"a partition is never wider than the bound", checkPartitionIsBoundedByWorkers},
		{"a partition's slices differ in size by at most one", checkPartitionIsBalanced},
		{"a partition holds no empty slice", checkPartitionHasNoEmptySlice},
		{"a partition is the same every time", checkPartitionIsStable},
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

// partitionShapes is the (pages, bound) grid every partition property is
// checked over. It is deliberately lopsided: the document far longer than the
// bound is the case the exec count has to stay off, the document shorter than
// the bound is the ordinary one — most documents are shorter than a machine's
// core count — and the exact multiple is where an off-by-one in the remainder
// hides.
func partitionShapes() [][2]int {
	return [][2]int{
		{1, 1}, {1, 16}, {2, 16}, {12, 4}, {15, 4}, {16, 16}, {17, 16},
		{48, 5}, {129, 32}, {437, 16}, {440, 16}, {3000, 16}, {3000, 1}, {200000, 12},
	}
}

// checkPartitionCoversEveryUnit pins what the assembled directory rests on: the
// slices tile the pages exactly, in order, with nothing repeated and nothing
// dropped. A partition that lost a page would produce a directory short a page,
// which is the silent failure the whole render family is built to refuse.
func checkPartitionCoversEveryUnit() error {
	for _, shape := range partitionShapes() {
		count, workers := shape[0], shape[1]
		next := 0
		for i, s := range Partition(count, workers) {
			if s.Start != next {
				return fmt.Errorf(
					"Partition(%d, %d): slice %d starts at %d, expected %d",
					count, workers, i, s.Start, next)
			}
			next = s.End
		}
		if next != count {
			return fmt.Errorf(
				"Partition(%d, %d): the slices cover %d units, expected %d",
				count, workers, next, count)
		}
	}
	if s := Partition(0, 4); s != nil {
		return fmt.Errorf("Partition(0, 4): expected no slices, got %v", s)
	}
	return nil
}

// checkPartitionIsBoundedByWorkers is the property the fold depth rests on, and
// the one the containerd mount-data ceiling is a consequence of: however long
// the document, a conversion folds no more directories than its bound allows.
func checkPartitionIsBoundedByWorkers() error {
	for _, shape := range partitionShapes() {
		count, workers := shape[0], shape[1]
		if n := len(Partition(count, workers)); n > max(workers, 1) {
			return fmt.Errorf(
				"Partition(%d, %d): expected at most %d slices, got %d",
				count, workers, workers, n)
		}
	}
	// A bound below one is nonsensical and the module rejects it by name; this
	// is the floor under that, and it must not be an unbounded fan-out.
	if n := len(Partition(3000, 0)); n != 1 {
		return fmt.Errorf("Partition(3000, 0): expected 1 slice, got %d", n)
	}
	return nil
}

// checkPartitionIsBalanced asserts no exec is handed materially more of the
// document than another, which is what keeps the conversion's wall clock at the
// slowest slice rather than at the longest one.
func checkPartitionIsBalanced() error {
	for _, shape := range partitionShapes() {
		count, workers := shape[0], shape[1]
		slices := Partition(count, workers)
		smallest, largest := slices[0].Len(), slices[0].Len()
		for _, s := range slices {
			smallest = min(smallest, s.Len())
			largest = max(largest, s.Len())
		}
		if largest-smallest > 1 {
			return fmt.Errorf(
				"Partition(%d, %d): slices run from %d to %d units, expected a spread of at most 1",
				count, workers, smallest, largest)
		}
	}
	return nil
}

// checkPartitionHasNoEmptySlice asserts a document shorter than the bound gets
// one slice per page rather than a tail of empty ones. An empty slice is a
// container created to render nothing, which is exactly the waste this
// partitioning exists to delete.
func checkPartitionHasNoEmptySlice() error {
	for _, shape := range partitionShapes() {
		count, workers := shape[0], shape[1]
		for i, s := range Partition(count, workers) {
			if s.Len() < 1 {
				return fmt.Errorf(
					"Partition(%d, %d): slice %d covers %d units",
					count, workers, i, s.Len())
			}
		}
	}
	return nil
}

// checkPartitionIsStable asserts the same arguments always partition the same
// way. Two conversions of one document under one bound have to produce the same
// execs, or neither ever hits the other's cache entries.
func checkPartitionIsStable() error {
	for _, shape := range partitionShapes() {
		count, workers := shape[0], shape[1]
		first, second := Partition(count, workers), Partition(count, workers)
		if len(first) != len(second) {
			return fmt.Errorf(
				"Partition(%d, %d): repeated calls returned %d and %d slices",
				count, workers, len(first), len(second))
		}
		for i := range first {
			if first[i] != second[i] {
				return fmt.Errorf(
					"Partition(%d, %d): slice %d came back as %v and then %v",
					count, workers, i, first[i], second[i])
			}
		}
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
