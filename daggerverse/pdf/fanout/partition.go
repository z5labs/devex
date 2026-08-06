package fanout

// Slice is a contiguous run of units, half-open: the units at indices Start
// through End-1.
//
// It is contiguous rather than strided — unit i to worker i%n would balance
// just as well — because the units are a document's pages in page order, and a
// slice of consecutive pages is the one shape whose exec both reads and writes
// a run a human can name.
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
// This is the whole of what keeps a conversion's exec count off the page count.
// A directory produced by an exec is *one* snapshot however many files that
// exec wrote, so the overlayfs chain a fold builds is as deep as the number of
// execs folded and no deeper — and containerd refuses to mount a chain whose
// compacted lowerdir list exceeds one page of joined mount data, which a
// three-thousand-page document reached at around 440 folds. Handing each exec a
// slice rather than a page makes the ceiling a property of the machine's core
// count, which does not grow with the document.
//
// Fewer units than workers gives one unit per slice rather than empty slices at
// the end: an exec with no pages to render is a container created to do nothing.
// Zero units gives no slices at all, which is a conversion with nothing to do —
// the render family refuses that by name long before it reaches here.
//
// The bigger slices come first, which is arbitrary but fixed: what matters is
// that one (count, workers) always partitions the same way, so two conversions
// of the same document under the same bound produce the same execs and hit the
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
