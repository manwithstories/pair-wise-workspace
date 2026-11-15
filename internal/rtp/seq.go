// Package rtp implements bounded RTP reordering and H.264 depacketization.
package rtp

import (
	"sort"

	pionrtp "github.com/pion/rtp"
)

// Gap is a contiguous range of missing 16-bit RTP sequence numbers.
type Gap struct {
	Start   uint16 `json:"start"`
	End     uint16 `json:"end"`
	Missing int    `json:"missing"`
}

// OrderedPacket is emitted after reordering. GapBefore is the number of
// missing packets immediately preceding it.
type OrderedPacket struct {
	Packet    *pionrtp.Packet
	GapBefore int
}

// SeqTracker unwraps 16-bit sequence numbers and buffers a fixed-size window.
type SeqTracker struct {
	window      uint64
	initialized bool
	next        uint64
	highest     uint64
	pending     map[uint64]*pionrtp.Packet
	recent      map[uint64]struct{}
	gaps        []Gap
	duplicates  uint64
	lateDropped uint64
	gapBefore   int
}

// NewSeqTracker starts on the first packet seen.
func NewSeqTracker(window int) *SeqTracker {
	return newTracker(window, 0, false)
}

// NewSeqTrackerAt starts at a known sequence number, which preserves leading gaps.
func NewSeqTrackerAt(initial uint16, window int) *SeqTracker {
	return newTracker(window, uint64(initial), true)
}

func newTracker(window int, initial uint64, initialized bool) *SeqTracker {
	if window <= 0 {
		window = 32
	}
	return &SeqTracker{
		window:      uint64(window),
		initialized: initialized,
		next:        initial,
		highest:     initial,
		pending:     make(map[uint64]*pionrtp.Packet),
		recent:      make(map[uint64]struct{}),
	}
}

// Push accepts one packet in arrival order. Packets are returned only when they
// can be emitted in sequence order without exceeding the reorder window.
func (t *SeqTracker) Push(packet *pionrtp.Packet) ([]OrderedPacket, bool, bool) {
	if packet == nil {
		return nil, false, true
	}
	ext := t.extend(packet.SequenceNumber)
	if !t.initialized {
		t.initialized = true
		t.next, t.highest = ext, ext
	}

	if ext < t.next {
		if _, ok := t.recent[ext]; ok {
			t.duplicates++
			return nil, true, false
		}
		t.lateDropped++
		return nil, false, true
	}
	if _, ok := t.pending[ext]; ok {
		t.duplicates++
		return nil, true, false
	}
	if ext > t.highest {
		t.highest = ext
	}

	if len(t.pending) == 0 && ext >= t.next+t.window {
		t.recordGap(t.next, ext)
		t.next = ext
	}
	t.pending[ext] = packet
	t.trimWindow()
	return t.drain(), false, false
}

func (t *SeqTracker) extend(seq uint16) uint64 {
	if !t.initialized {
		return uint64(seq)
	}
	base := int64(t.highest &^ 0xffff)
	candidate := base + int64(seq)
	delta := candidate - int64(t.highest)
	if delta < -32768 {
		candidate += 65536
	} else if delta > 32767 {
		candidate -= 65536
	}
	if candidate < 0 {
		candidate += 65536
	}
	return uint64(candidate)
}

func (t *SeqTracker) trimWindow() {
	for len(t.pending) >= int(t.window) {
		if _, ok := t.pending[t.next]; ok {
			return
		}
		first, ok := t.smallestPending()
		if !ok {
			return
		}
		t.recordGap(t.next, first)
		t.next = first
	}
}

func (t *SeqTracker) drain() []OrderedPacket {
	var out []OrderedPacket
	for {
		packet, ok := t.pending[t.next]
		if !ok {
			return out
		}
		out = append(out, OrderedPacket{Packet: packet, GapBefore: t.gapBefore})
		t.gapBefore = 0
		delete(t.pending, t.next)
		t.recent[t.next] = struct{}{}
		t.next++
		t.pruneRecent()
	}
}

func (t *SeqTracker) pruneRecent() {
	cutoff := uint64(0)
	if t.next > t.window {
		cutoff = t.next - t.window
	}
	for seq := range t.recent {
		if seq < cutoff {
			delete(t.recent, seq)
		}
	}
}

func (t *SeqTracker) smallestPending() (uint64, bool) {
	var first uint64
	found := false
	for seq := range t.pending {
		if !found || seq < first {
			first, found = seq, true
		}
	}
	return first, found
}

func (t *SeqTracker) recordGap(from, to uint64) {
	if from >= to {
		return
	}
	missing := int(to - from)
	t.gapBefore += missing
	start, end := uint16(from), uint16(to-1)
	if len(t.gaps) > 0 {
		last := &t.gaps[len(t.gaps)-1]
		if uint16(last.End+1) == start {
			last.End = end
			last.Missing += missing
			return
		}
	}
	t.gaps = append(t.gaps, Gap{Start: start, End: end, Missing: missing})
}

// Flush emits all buffered packets and records gaps inside their range.
func (t *SeqTracker) Flush() []OrderedPacket {
	var out []OrderedPacket
	keys := make([]uint64, 0, len(t.pending))
	for seq := range t.pending {
		keys = append(keys, seq)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, seq := range keys {
		if seq < t.next {
			continue
		}
		if seq > t.next {
			t.recordGap(t.next, seq)
			t.next = seq
		}
		out = append(out, t.drain()...)
	}
	return out
}

// Gaps returns a copy of the sequence gaps declared so far.
func (t *SeqTracker) Gaps() []Gap {
	out := make([]Gap, len(t.gaps))
	copy(out, t.gaps)
	return out
}

// Duplicates returns the number of duplicate packets discarded.
func (t *SeqTracker) Duplicates() uint64 { return t.duplicates }

// LateDropped returns packets outside the reorder window.
func (t *SeqTracker) LateDropped() uint64 { return t.lateDropped }
