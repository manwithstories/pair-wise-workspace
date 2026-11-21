// Package clock unwraps 33-bit MPEG timestamps (PCR base and PTS, both in
// 90 kHz units) onto a single monotonic 64-bit media timeline. All
// subsystems share this unwrapper so that faults, keyframe indexes and
// segment boundaries refer to the same time base.
package clock

// Modulus is the 33-bit wrap period of PCR/PTS values.
const Modulus uint64 = 1 << 33

// halfModulus is the maximum forward step we accept as normal progress;
// anything larger is treated as a wrap (forward) or a jump (backward).
const halfModulus uint64 = Modulus / 2

// Timeline unwraps one stream of 33-bit timestamps.
type Timeline struct {
	have    bool
	last    int64 // last unwrapped value
	lastRaw uint64
	wraps   int
}

// Event describes what Unwrap observed for one raw timestamp.
type Event struct {
	Value    int64 // unwrapped 90 kHz media time
	Wrapped  bool  // raw counter rolled over forward past 2^33
	Backward bool  // unwrapped value moved backwards (non-monotonic jump)
}

// Unwrap extends raw (a 33-bit value) to the monotonic timeline.
//
// The candidate closest to the previous unwrapped value is chosen, which
// resolves forward wraps. A candidate below the previous value is reported
// as a backward jump but is still returned: callers decide whether the
// stream can recover.
func (t *Timeline) Unwrap(raw uint64) Event {
	raw &= Modulus - 1
	if !t.have {
		t.have = true
		t.last = int64(raw)
		t.lastRaw = raw
		return Event{Value: t.last}
	}
	base := t.last &^ int64(Modulus-1)
	cand := base + int64(raw)
	switch {
	case cand-t.last > int64(halfModulus):
		cand -= int64(Modulus)
	case t.last-cand >= int64(halfModulus):
		cand += int64(Modulus)
	}
	wrapped := raw < t.lastRaw && cand > t.last
	if wrapped {
		t.wraps++
	}
	backward := cand < t.last
	ev := Event{Value: cand, Wrapped: wrapped, Backward: backward}
	// Forward progress (including wraps) advances the reference point.
	// Backward jumps do not, so a stream that recovers keeps its base.
	if !backward {
		t.last = cand
		t.lastRaw = raw
	}
	return ev
}

// Wraps returns how many forward 33-bit rollovers were observed.
func (t *Timeline) Wraps() int { return t.wraps }

// Last returns the most recent unwrapped value and whether one exists.
func (t *Timeline) Last() (int64, bool) { return t.last, t.have }
