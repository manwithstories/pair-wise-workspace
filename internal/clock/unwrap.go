// Package clock converts source capture clocks into the unified nanosecond
// time base: 32-bit RTP-style timestamp unwrapping and anchor-based clock
// drift estimation.
package clock

const wrapMod32 = 1 << 32

// halfRange32 is the jump threshold: a delta whose unsigned magnitude is at
// least half the 32-bit range is treated as a wraparound.
const halfRange32 = wrapMod32 / 2

// Unwrapper expands a stream of 32-bit timestamps into monotonically tracked
// 64-bit values. It tolerates reordering: a packet arriving slightly late is
// unwrapped against the same high bits as the value it follows in arrival
// order, while a jump of half the range or more counts as one wrap.
type Unwrapper struct {
	last      uint64
	haveValue bool
}

// Next feeds one raw 32-bit timestamp and returns its expanded 64-bit form.
// A jump of at least half the range is treated as a wrap (negative delta);
// smaller out-of-order steps retain their current high bits.
func (u *Unwrapper) Next(raw uint32) uint64 {
	if !u.haveValue {
		u.haveValue = true
		u.last = uint64(raw)
		return u.last
	}
	delta := (uint64(raw) - u.last) % wrapMod32
	if delta >= halfRange32 {
		delta -= wrapMod32
	}
	u.last += delta
	return u.last
}

// Unwrap expands a whole 32-bit series. It is a convenience wrapper around
// Unwrapper for callers that already hold the full arrival-ordered series.
func Unwrap(raw []uint32) []uint64 {
	out := make([]uint64, len(raw))
	var u Unwrapper
	for i, v := range raw {
		out[i] = u.Next(v)
	}
	return out
}

// SeqUnwrapper expands 16-bit RTP sequence numbers using the same half-range
// rule, which is enough to order reordered packets across 16-bit wraps.
type SeqUnwrapper struct {
	last      uint64
	haveValue bool
}

// Next feeds one raw 16-bit sequence number and returns its expanded form.
// The forward-wrapping rule is strict: only a jump that covers at least half
// the 16-bit range increments the high bits, so small out-of-order steps keep
// their wrap while genuine 0xffff->0 wraps advance correctly.
func (s *SeqUnwrapper) Next(raw uint16) uint64 {
	if !s.haveValue {
		s.haveValue = true
		s.last = uint64(raw)
		return s.last
	}
	mod := uint64(1 << 16)
	delta := (uint64(raw) - s.last) % mod
	if delta >= mod/2 {
		delta -= mod
	}
	s.last += delta
	return s.last
}
