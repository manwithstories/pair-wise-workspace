package rtp

import (
	"sort"

	"mediacheck/internal/model"
)

// Window is a bounded sequence-number reassembly buffer.
type Window struct {
	capacity int
	base     uint16
	have     map[uint16]Packet
	// totalSent is the expected packet count for percentage accounting.
	totalSent int
}

// NewWindow constructs a reassembly window holding at most capacity seqs.
func NewWindow(capacity, totalSent int) *Window {
	if capacity <= 0 {
		capacity = 64
	}
	return &Window{capacity: capacity, have: map[uint16]Packet{}, totalSent: totalSent}
}

// Stats summarizes one reassembly run.
type Stats struct {
	Sent           int
	Received       int // distinct seqs actually observed (duplicates count once)
	Duplicate      int
	Reordered      int
	Recovered      int // seqs restored from reorder/duplicate handling
	Unrecoverable  int // seqs never observed within their window
	FirstSeq       uint16
	LastSeq        uint16
	UnrecoverableRanges []model.TimeRange
}

// Add inserts one delivered packet into the bounded window. Duplicates are
// rejected at the window; reordering is absorbed because insertion is keyed
// by sequence number rather than arrival order.
func (w *Window) Add(p Packet) (dup bool) {
	if _, ok := w.have[p.Seq]; ok {
		return true
	}
	w.have[p.Seq] = p
	return false
}

// Drain returns packets currently inside the window in sequence order,
// limited to capacity entries starting at the current base.
func (w *Window) Drain() []Packet {
	seqs := make([]int, 0, len(w.have))
	for seq := range w.have {
		r := relSeq(seq, w.base)
		if r >= 0 && r < w.capacity {
			seqs = append(seqs, r)
		}
	}
	sort.Ints(seqs)
	out := make([]Packet, 0, len(seqs))
	for _, r := range seqs {
		out = append(out, w.have[w.base+uint16(r)])
	}
	return out
}

// Reassemble sorts damage-affected deliveries back into sequence order over
// the bounded window, deduplicates, and reports the seqs that cannot be
// recovered. The payload seq echo is independently verified.
func Reassemble(delivered []Packet, totalSent int) ([]Packet, Stats, error) {
	st := Stats{Sent: totalSent, FirstSeq: 0, LastSeq: uint16(totalSent - 1)}
	if totalSent == 0 {
		return nil, st, nil
	}
	win := NewWindow(totalSent, totalSent)
	present := map[uint16]struct{}{}
	for i, p := range delivered {
		if win.Add(p) {
			st.Duplicate++
			continue
		}
		present[p.Seq] = struct{}{}
		if i > 0 {
			// Late arrival relative to the previous delivered packet.
			if relSeq(p.Seq, st.FirstSeq) < relSeq(delivered[i-1].Seq, st.FirstSeq) {
				st.Reordered++
			}
		}
	}
	st.Received = len(present)

	distinct := win.Drain()
	// Every reordered packet that did eventually arrive is recovered.
	st.Recovered = st.Reordered

	for _, p := range distinct {
		if err := verifyPayload(p); err != nil {
			return nil, st, err
		}
	}

	// Missing seqs within the expected run are unrecoverable burst losses.
	var missing []uint16
	for i := 0; i < totalSent; i++ {
		seq := uint16(i)
		if _, ok := present[seq]; !ok {
			missing = append(missing, seq)
		}
	}
	st.Unrecoverable = len(missing)
	st.UnrecoverableRanges = seqRanges(missing)
	return distinct, st, nil
}

// RecoveryPercent returns the share of the expected stream that is fully
// reassembled (recovered reorders included), using integer basis scaled to
// two decimal places only at render time.
func (s Stats) RecoveryPercent() float64 {
	if s.Sent == 0 {
		return 100
	}
	reassembled := s.Sent - s.Unrecoverable
	return float64(reassembled) * 100 / float64(s.Sent)
}

// seqRanges coalesces consecutive missing seqs into half-open index ranges
// expressed on the shared timeline units (here packet-order ticks of 1).
func seqRanges(missing []uint16) []model.TimeRange {
	if len(missing) == 0 {
		return nil
	}
	var out []model.TimeRange
	start := int64(missing[0])
	prev := int64(missing[0])
	for _, m := range missing[1:] {
		cur := int64(m)
		if cur != prev+1 {
			out = append(out, model.TimeRange{Start: start, End: prev + 1})
			start = cur
		}
		prev = cur
	}
	out = append(out, model.TimeRange{Start: start, End: prev + 1})
	return out
}

func verifyPayload(p Packet) error {
	want := syntheticPayload(p.Seq)
	if len(p.Payload) != len(want) {
		return model.NewError(model.CodeCorruptSample, "rtp",
			int64(p.Seq), int64(p.Seq)+1, "payload length mismatch")
	}
	for i := range want {
		if p.Payload[i] != want[i] {
			return model.NewError(model.CodeCorruptSample, "rtp",
				int64(p.Seq), int64(p.Seq)+1, "payload content mismatch")
		}
	}
	return nil
}
