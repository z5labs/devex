package fanout

import "context"

// Slice is a contiguous run of units, half-open: the units at indices Start
// through End-1.
//
// It is contiguous rather than strided — unit i to worker i%n would balance
// just as well — because the units are a batch's images in sorted path order,
// and a slice of consecutive paths is the one shape whose exec both reads and
// writes a run a human can name.
type Slice struct {
	// Start is the index of the slice's first unit.
	Start int
	// End is one past the index of its last.
	End int
}

// Len is how many units the slice covers.
func (s Slice) Len() int {
	return s.End - s.Start
}

// Partition splits count units into at most workers contiguous slices, each as
// close to the same size as the division allows.
//
// This is the whole of what keeps a batch's exec count off the image count. A
// directory produced by an exec is *one* snapshot however many files that exec
// wrote, so the overlayfs chain a fold builds is as deep as the number of execs
// folded and no deeper — and containerd refuses to mount a chain whose compacted
// lowerdir list exceeds one page of joined mount data, which a three-thousand
// image batch reached at around 440 folds. Handing each exec a slice rather than
// an image makes the ceiling a property of the machine's core count, which does
// not grow with the batch.
//
// Fewer units than workers gives one unit per slice rather than empty slices at
// the end: an exec with no images to recognise is a container created to do
// nothing. Zero units gives no slices at all, which is a batch with nothing to
// do — Export refuses that by name long before it reaches here.
//
// The bigger slices come first, which is arbitrary but fixed: what matters is
// that one (count, workers) always partitions the same way, so two exports of
// the same directory under the same bound produce the same execs and hit the
// same cache entries.
func Partition(count, workers int) []Slice {
	if count <= 0 {
		return nil
	}
	if workers < 1 {
		workers = 1
	}
	if workers > count {
		workers = count
	}

	size, extra := count/workers, count%workers
	slices := make([]Slice, 0, workers)
	start := 0
	for i := range workers {
		n := size
		if i < extra {
			n++
		}
		slices = append(slices, Slice{Start: start, End: start + n})
		start += n
	}
	return slices
}

// RunSlices splits items into at most workers contiguous slices, runs one unit
// of work per slice — at most workers of them at a time — and returns each
// unit's result positionally, in slice order.
//
// It is the whole of the fan-out in one call, and that is deliberate: the bound
// is one number that means one thing, and a caller cannot partition by one
// figure and schedule by another. Pairing Partition with Run by hand is what
// this replaces; the two are still exported because each is meaningful alone —
// Run for work that is already a flat count of units, Partition for a caller
// that needs the boundaries themselves — but nothing should be passing the same
// bound to both.
//
// run receives its slice of items and nothing else. It needs no index: the
// results come back in the order the slices were cut, so position in the
// returned slice is position in items, and whatever identifies the work is in
// the items themselves.
//
// Everything Run promises holds here. The bound is a ceiling on units in flight,
// the first failure is the error returned and cancels the rest, and no result is
// returned at all when one unit fails — a partial fan-out is not a partial
// answer, it is an answer with a hole in it.
//
// Empty items yields no units and no results, which is a fan-out with nothing to
// do rather than an error; Export refuses that case by name long before it
// reaches here.
func RunSlices[In, Out any](
	ctx context.Context,
	workers int,
	items []In,
	run func(ctx context.Context, items []In) (Out, error),
) ([]Out, error) {
	slices := Partition(len(items), workers)
	out := make([]Out, len(slices))

	// Each unit writes only its own element, and Run does not return until every
	// unit it started has, so the slice needs no lock of its own.
	err := Run(ctx, workers, len(slices), func(ctx context.Context, i int) error {
		result, err := run(ctx, items[slices[i].Start:slices[i].End])
		if err != nil {
			return err
		}
		out[i] = result
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
