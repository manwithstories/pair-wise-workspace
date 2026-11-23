package timeline

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"

	"mediarchive/internal/clock"
	"mediarchive/internal/event"
)

// UnrepairableMark records an event excluded from the canonical timeline
// because it arrived beyond the late window.
type UnrepairableMark struct {
	SeqExt    uint64
	MediaTime clock.Time
	Reason    event.Reason
}

// Timeline accumulates accepted packets and produces a canonical,
// deterministic ordering. Late arrivals inside the window are folded in
// by replay: the canonical order is recomputed from the full arrival log,
// so it always equals what in-order processing would have produced.
type Timeline struct {
	wm           *Watermark
	pkts         []*event.Packet // arrival order
	Replays      int
	TooLate      int
	Unrepairable []UnrepairableMark
}

// New creates a timeline with the given late window.
func New(lateWindow clock.Time) *Timeline {
	return &Timeline{wm: NewWatermark(lateWindow)}
}

// Add classifies p against the watermark and records it. Late packets
// inside the window are accepted and bump the replay counter; packets
// beyond the window are excluded and marked unrepairable.
func (t *Timeline) Add(p *event.Packet) Verdict {
	v := t.wm.Observe(p.MediaTime)
	switch v {
	case Late:
		p.Flags |= event.FlagLate
		t.Replays++
		t.pkts = append(t.pkts, p)
	case OnTime:
		t.pkts = append(t.pkts, p)
	case TooLate:
		t.TooLate++
		t.Unrepairable = append(t.Unrepairable, UnrepairableMark{
			SeqExt: p.SeqExt, MediaTime: p.MediaTime, Reason: event.ReasonTooLate,
		})
	}
	return v
}

// Sorted returns the canonical timeline: the arrival log sorted by
// extended sequence number (the decode order), deduplicated by sequence
// number. Because the order depends only on the accepted set — never on
// arrival order — a replay recompute after a late arrival yields exactly
// what one-pass in-order processing would have produced.
func (t *Timeline) Sorted() []*event.Packet {
	out := make([]*event.Packet, len(t.pkts))
	copy(out, t.pkts)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].SeqExt < out[j].SeqExt
	})
	seen := make(map[uint64]struct{}, len(out))
	n := 0
	for _, p := range out {
		if _, dup := seen[p.SeqExt]; dup {
			continue
		}
		seen[p.SeqExt] = struct{}{}
		out[n] = p
		n++
	}
	return out[:n]
}

// Digest returns the deterministic SHA-256 summary of the canonical
// timeline. Arrival-dependent annotations (late/reordered flags) are
// excluded so that the digest depends only on the recovered media.
func (t *Timeline) Digest() [32]byte {
	h := sha256.New()
	var b [8]byte
	put := func(v uint64) {
		binary.LittleEndian.PutUint64(b[:], v)
		h.Write(b[:])
	}
	for _, p := range t.Sorted() {
		put(p.SeqExt)
		put(p.TSExt)
		put(uint64(p.MediaTime))
		put(uint64(p.Duration))
		put(uint64(len(p.Payload)))
		h.Write(p.Payload)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}
