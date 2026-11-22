package rtp

// Release is the output of one jitter-buffer drain: in-order packets plus
// the extended sequence ranges declared lost under the window policy.
type Release struct {
	Packets []*Packet
	Gaps    [][2]int64 // declared-lost extended seq ranges, inclusive
}

// Buffer is a bounded reorder window over one RTP stream. Packets that
// arrive after their sequence position has already been released are
// rejected as late and can never rewrite released (committed) regions.
type Buffer struct {
	window   int
	started  bool
	maxExt   int64 // highest extended seq seen
	released int64 // highest extended seq released
	pending  map[int64]*Packet
	late     int
	dups     int
}

// NewBuffer creates a jitter buffer holding up to window packets.
func NewBuffer(window int) *Buffer {
	if window < 1 {
		window = 1
	}
	return &Buffer{window: window, released: -1, pending: make(map[int64]*Packet)}
}

// Late returns the count of packets rejected for arriving behind the
// released edge.
func (b *Buffer) Late() int { return b.late }

// Duplicates returns the count of rejected duplicate packets.
func (b *Buffer) Duplicates() int { return b.dups }

func (b *Buffer) extend(seq uint16) int64 {
	if !b.started {
		b.started = true
		b.maxExt = int64(seq)
		return b.maxExt
	}
	ext := b.maxExt + int64(int16(seq-uint16(b.maxExt)))
	if ext > b.maxExt {
		b.maxExt = ext
	}
	return ext
}

// Push inserts one packet. It reports false for duplicates and for late
// packets that fall at or behind the released edge.
func (b *Buffer) Push(p *Packet) bool {
	ext := b.extend(p.Seq)
	if ext <= b.released {
		b.late++
		return false
	}
	if _, ok := b.pending[ext]; ok {
		b.dups++
		return false
	}
	p.ExtSeq = ext
	b.pending[ext] = p
	return true
}

// Drain releases contiguous packets from the released edge. When the
// pending span exceeds the window, the oldest missing positions are
// declared lost so the stream keeps moving.
func (b *Buffer) Drain() *Release {
	rel := &Release{}
	for {
		if p, ok := b.pending[b.released+1]; ok {
			rel.Packets = append(rel.Packets, p)
			delete(b.pending, b.released+1)
			b.released++
			continue
		}
		if len(b.pending) == 0 || b.maxExt-b.released <= int64(b.window) {
			break
		}
		// Declare the oldest gap lost, up to the lowest pending seq.
		low := b.maxExt
		for ext := range b.pending {
			if ext < low {
				low = ext
			}
		}
		rel.Gaps = append(rel.Gaps, [2]int64{b.released + 1, low - 1})
		b.released = low - 1
	}
	return rel
}

// Flush releases everything still pending at end of stream, declaring
// any remaining interior gaps lost.
func (b *Buffer) Flush() *Release {
	rel := &Release{}
	for len(b.pending) > 0 {
		if p, ok := b.pending[b.released+1]; ok {
			rel.Packets = append(rel.Packets, p)
			delete(b.pending, b.released+1)
			b.released++
			continue
		}
		low := b.maxExt
		for ext := range b.pending {
			if ext < low {
				low = ext
			}
		}
		rel.Gaps = append(rel.Gaps, [2]int64{b.released + 1, low - 1})
		b.released = low - 1
	}
	return rel
}
