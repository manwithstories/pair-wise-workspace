package transport

import "mediamix/internal/core"

// Extender maps 16-bit wire sequence numbers onto a monotonic 64-bit space,
// tolerating wraparound and reordering. Each track is tracked independently.
//
// The mapping only needs to be unambiguous for jumps smaller than half the
// sequence space, which matches RTP's own reorder window semantics.
type Extender struct {
	state map[string]*extState
}

type extState struct {
	init bool
	max  uint64 // highest extended sequence observed for the track
}

// NewExtender creates a wraparound tracker.
func NewExtender() *Extender { return &Extender{state: make(map[string]*extState)} }

// Extend returns the extended sequence number for a wire sequence. Because the
// reference is the highest sequence seen, a late packet from the previous
// 16-bit epoch still maps behind the reference instead of jumping forward.
func (e *Extender) Extend(track string, seq uint16) (uint64, error) {
	st, ok := e.state[track]
	if !ok {
		st = &extState{}
		e.state[track] = st
	}
	if !st.init {
		st.init = true
		st.max = uint64(seq)
		return st.max, nil
	}
	delta := int64(int16(seq - uint16(st.max&0xffff)))
	ext := int64(st.max) + delta
	if ext < 0 {
		// A packet more than half the sequence space before the origin is not
		// attributable to this stream.
		return 0, core.SeqError(core.CodeBadPacket, track, uint32(seq),
			"sequence precedes stream origin")
	}
	if uint64(ext) > st.max {
		st.max = uint64(ext)
	}
	return uint64(ext), nil
}

// Snapshot returns the highest extended sequence seen for a track.
func (e *Extender) Snapshot(track string) (uint64, bool) {
	st, ok := e.state[track]
	if !ok || !st.init {
		return 0, false
	}
	return st.max, true
}
