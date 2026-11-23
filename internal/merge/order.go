// Package merge orders processed samples deterministically regardless of the
// worker count that produced them. Ordering is by (PTS, mapped track number,
// input sequence), which is a total order: two samples can never tie on every
// key, so concurrency 1 and concurrency 4 yield byte-identical sequences.
package merge

import (
	"sort"

	"mediacore/internal/sample"
)

// Ordered wraps a sample with the stable merge keys.
type Ordered struct {
	Sample *sample.Sample
	// InputSeq is the original ingestion order, used as the final tie-breaker.
	InputSeq int
}

// key comparison: PTS, then mapped track ID, then input sequence.
func less(a, b Ordered) bool {
	if a.Sample.PTS != b.Sample.PTS {
		return a.Sample.PTS < b.Sample.PTS
	}
	if a.Sample.TrackID != b.Sample.TrackID {
		return a.Sample.TrackID < b.Sample.TrackID
	}
	return a.InputSeq < b.InputSeq
}

// Order sorts in place using the total-order key. It is deterministic and does
// not depend on goroutine scheduling because every tie is broken explicitly.
func Order(items []Ordered) []*sample.Sample {
	sort.SliceStable(items, func(i, j int) bool { return less(items[i], items[j]) })
	out := make([]*sample.Sample, len(items))
	for i, it := range items {
		out[i] = it.Sample
	}
	return out
}

// Equal reports whether two ordered sample slices are identical by merge key
// and compressed payload, used to assert concurrency-independent ordering.
func Equal(a, b []*sample.Sample) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].PTS != b[i].PTS || a[i].TrackID != b[i].TrackID ||
			a[i].Seq != b[i].Seq || a[i].TrackIndex != b[i].TrackIndex {
			return false
		}
		if len(a[i].Data) != len(b[i].Data) {
			return false
		}
		for k := range a[i].Data {
			if a[i].Data[k] != b[i].Data[k] {
				return false
			}
		}
	}
	return true
}

// ReorderBuffers re-sequences out-of-order arrivals from a worker pool. It
// accepts results as they arrive and emits samples in the deterministic total
// order once the full batch is closed via Flush.
type ReorderBuffer struct {
	items []Ordered
}

// Add records one processed sample with its ingestion sequence.
func (rb *ReorderBuffer) Add(seq int, s *sample.Sample) {
	rb.items = append(rb.items, Ordered{Sample: s, InputSeq: seq})
}

// Flush returns the deterministically ordered samples and resets the buffer.
func (rb *ReorderBuffer) Flush() []*sample.Sample {
	out := Order(rb.items)
	rb.items = nil
	return out
}

// Len reports buffered item count.
func (rb *ReorderBuffer) Len() int { return len(rb.items) }
