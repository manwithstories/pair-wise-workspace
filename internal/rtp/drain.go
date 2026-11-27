package rtp

import (
	"sort"
	"time"
)

// Advance moves the release clock forward without adding a packet.
func (r *Reorderer) Advance(now time.Duration) []Packet {
	if now > r.now {
		r.now = now
	}
	return r.drain(false)
}

// Flush releases everything left in the window in sequence order, declaring
// the gaps in between. The caller decides whether trailing loss matters.
func (r *Reorderer) Flush() ([]Packet, []Gap) {
	before := len(r.Declared)
	out := r.drain(true)
	return out, append([]Gap(nil), r.Declared[before:]...)
}

// Pending reports how many packets are still buffered.
func (r *Reorderer) Pending() int { return len(r.buf) }

// SortedSeqs is a helper for tests and diagnostics.
func (r *Reorderer) SortedSeqs() []uint16 {
	keys := make([]int64, 0, len(r.buf))
	for k := range r.buf {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]uint16, len(keys))
	for i, k := range keys {
		out[i] = uint16(k & 0xffff)
	}
	return out
}
