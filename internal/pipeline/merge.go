package pipeline

import (
	"container/heap"
	"context"
	"fmt"

	"avlab/internal/media"
)

// resultHeap orders Results by (epoch, segment, seq).
type resultHeap []Result

func (h resultHeap) Len() int { return len(h) }
func (h resultHeap) Less(i, j int) bool {
	if h[i].Epoch != h[j].Epoch {
		return h[i].Epoch < h[j].Epoch
	}
	if h[i].Segment != h[j].Segment {
		return h[i].Segment < h[j].Segment
	}
	return seqLess(h[i].Seq, h[j].Seq)
}
func (h resultHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *resultHeap) Push(x any)   { *h = append(*h, x.(Result)) }
func (h *resultHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}

// seqLess implements RFC 3550 sequence comparison.
func seqLess(a, b uint16) bool { return int16(a-b) < 0 }

// Merger collects completions arriving in arbitrary order and emits them in
// strict timeline order. Stale results from older epochs are discarded so a
// discontinuity cannot be polluted by late packets.
type Merger struct {
	h      resultHeap
	seen   map[string]bool
	stats  MergeStats
}

// MergeStats records acceptance and rejection counts.
type MergeStats struct {
	Accepted   uint64
	Duplicates uint64
	StaleEpoch uint64
	Failed     uint64
}

// NewMerger creates an empty merger.
func NewMerger() *Merger {
	return &Merger{seen: map[string]bool{}}
}

func keyOf(r Result) string {
	return fmt.Sprintf("%d/%d/%d", r.Epoch, r.Segment, r.Seq)
}

// Add accepts one completion. Duplicates and stale-epoch results are counted
// and dropped instead of entering the ordered heap.
func (m *Merger) Add(r Result) error {
	if r.Err != nil {
		if media.IsCode(r.Err, media.CodeCanceled) {
			m.stats.StaleEpoch++
			return r.Err
		}
		m.stats.Failed++
		return r.Err
	}
	k := keyOf(r)
	if m.seen[k] {
		m.stats.Duplicates++
		return media.NewFault(media.CodePacketDuplicate, true,
			"merger already emitted this key").WithSeq(r.Seq, r.Epoch)
	}
	m.seen[k] = true
	heap.Push(&m.h, r)
	m.stats.Accepted++
	return nil
}

// DrainOrdered returns every currently buffered result in timeline order.
func (m *Merger) DrainOrdered() []Result {
	out := make([]Result, 0, m.h.Len())
	for m.h.Len() > 0 {
		out = append(out, heap.Pop(&m.h).(Result))
	}
	return out
}

// Collect reads the pool result channel until it closes (or ctx is canceled),
// then returns every accepted result in timeline order.
func (m *Merger) Collect(ctx context.Context, ch <-chan Result) ([]Result, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, media.WrapFault(media.CodeCanceled, true,
				"merger stopped by cancellation", ctx.Err())
		case r, ok := <-ch:
			if !ok {
				return m.DrainOrdered(), nil
			}
			_ = m.Add(r) // accounting happens even for rejected results
		}
	}
}

// Stats returns the merge counters.
func (m *Merger) Stats() MergeStats { return m.stats }
