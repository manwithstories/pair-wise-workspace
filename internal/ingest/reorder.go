package ingest

import "container/heap"

// Item is one arrived segment ready for ordering.
type Item struct {
	Seq  int64
	File string
	CRC  uint32
}

// itemHeap is a min-heap of items keyed by media sequence.
type itemHeap []Item

func (h itemHeap) Len() int           { return len(h) }
func (h itemHeap) Less(i, j int) bool { return h[i].Seq < h[j].Seq }
func (h itemHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *itemHeap) Push(x any)        { *h = append(*h, x.(Item)) }
func (h *itemHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}

// Reorderer restores media-sequence order within a retransmission window,
// drops duplicates, flags CRC conflicts, and declares gaps on timeout.
type Reorderer struct {
	Window int

	next  int64
	h     itemHeap
	seen  map[int64]uint32
	inBuf map[int64]bool

	Duplicates int
	Conflicts  []int64
	Expired    int
	Gaps       []int64
}

func NewReorderer(window int) *Reorderer {
	if window < 1 {
		window = 1
	}
	return &Reorderer{
		Window: window,
		seen:   map[int64]uint32{},
		inBuf:  map[int64]bool{},
	}
}

// Push feeds one arrived item and returns any items that became emit-ready
// in sequence order.
func (r *Reorderer) Push(it Item) []Item {
	if crc, ok := r.seen[it.Seq]; ok {
		if crc == it.CRC {
			r.Duplicates++
		} else {
			// Same sequence, different payload: keep the first arrival.
			r.Conflicts = append(r.Conflicts, it.Seq)
		}
		return nil
	}
	if it.Seq < r.next {
		// Never seen and already past the window: too late to place.
		r.Expired++
		return nil
	}
	r.seen[it.Seq] = it.CRC
	r.inBuf[it.Seq] = true
	heap.Push(&r.h, it)
	return r.drain()
}

func (r *Reorderer) drain() []Item {
	var out []Item
	for len(r.h) > 0 {
		top := r.h[0]
		switch {
		case top.Seq == r.next:
			heap.Pop(&r.h)
			delete(r.inBuf, top.Seq)
			out = append(out, top)
			r.next++
		case top.Seq-r.next >= int64(r.Window):
			// Retransmission window elapsed: declare a gap and move on.
			r.Gaps = append(r.Gaps, r.next)
			r.next++
		default:
			return out
		}
	}
	return out
}

// Flush emits everything still buffered at end of stream, declaring gaps
// for any remaining holes.
func (r *Reorderer) Flush() []Item {
	var out []Item
	for len(r.h) > 0 {
		top := heap.Pop(&r.h).(Item)
		delete(r.inBuf, top.Seq)
		for r.next < top.Seq {
			r.Gaps = append(r.Gaps, r.next)
			r.next++
		}
		out = append(out, top)
		r.next = top.Seq + 1
	}
	return out
}
