package transport

import "sort"

// Advance skips the current gap and delivers everything buffered. It returns
// the delivered packets and the number of skipped sequence numbers. It leaves
// the buffer open, so a track can continue after a deadline.
func (r *ReorderBuffer) Advance() ([]Packet, int) {
	out := r.drain()
	skipped := 0
	if len(r.buf) > 0 {
		next := r.lowestBuffered()
		if next > r.expected {
			skipped = int(next - r.expected)
			r.expected = next
		}
		out = append(out, r.drain()...)
	}
	r.haveGap = false
	return out, skipped
}

// Flush delivers every remaining buffered packet in sequence order, skipping
// any final hole, and closes the buffer.
func (r *ReorderBuffer) Flush() ([]Packet, int) {
	out, skipped := r.Advance()
	r.closed = true
	return out, skipped
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// sortedExts is a helper for deterministic diagnostics.
func sortedExts(m map[uint64]Packet) []uint64 {
	out := make([]uint64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
