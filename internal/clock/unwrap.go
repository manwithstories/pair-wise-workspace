package clock

// Unwrap64 extends a wrapping fixed-width counter into a monotonic 64-bit
// value. The window is half the counter modulus: a jump whose signed delta
// fits inside the window is treated as a normal step, anything larger is a
// wrap in the corresponding direction. tfdt uses a 33-bit window; RTP
// sequence numbers use a 15-bit window.
type Unwrap64 struct {
	bits     uint
	mod      uint64
	half     uint64
	last     uint64
	extended uint64
	have     bool
	wraps    int
}

// NewUnwrap64 builds an unwrapper for a counter of bits width (1..63).
func NewUnwrap64(bits uint) *Unwrap64 {
	if bits == 0 || bits > 63 {
		bits = 33
	}
	mod := uint64(1) << bits
	return &Unwrap64{bits: bits, mod: mod, half: mod >> 1}
}

// Extend maps raw onto the unwrapped axis. The first value seeds the axis.
// Returned wraps is the cumulative number of forward wrap events seen so
// far, so callers can distinguish a genuine rollback from a wrap.
func (u *Unwrap64) Extend(raw uint64) (extended uint64, wraps int) {
	raw &= u.mod - 1
	if !u.have {
		u.have = true
		u.last, u.extended = raw, raw
		return raw, 0
	}
	delta := int64(raw - (u.extended & (u.mod - 1)))
	if delta < 0 && uint64(-delta) > u.half {
		// Big negative jump: actually a forward wrap.
		delta += int64(u.mod)
	} else if delta > 0 && uint64(delta) > u.half {
		// Big positive jump: a backwards wrap (counter went around down).
		delta -= int64(u.mod)
	}
	next := uint64(int64(u.extended) + delta)
	switch {
	case delta > 0 && raw < u.last:
		u.wraps++ // stepped forward across the modulus
	case delta < 0 && raw > u.last:
		u.wraps-- // stepped backward across the modulus
	}
	u.last, u.extended = raw, next
	return next, u.wraps
}

// Wraps returns the cumulative forward wrap count.
func (u *Unwrap64) Wraps() int { return u.wraps }

// Reset clears the seeded state.
func (u *Unwrap64) Reset() { *u = *NewUnwrap64(u.bits) }

// Unwrap16 is the 16-bit RTP sequence-number specialization.
type Unwrap16 struct {
	inner *Unwrap64
}

// NewUnwrap16 builds a 16-bit (15-bit half-window) sequence unwrapper.
func NewUnwrap16() *Unwrap16 {
	return &Unwrap16{inner: NewUnwrap64(16)}
}

// Extend maps a raw RTP sequence number to a monotonic 64-bit axis.
func (u *Unwrap16) Extend(seq uint16) uint64 {
	v, _ := u.inner.Extend(uint64(seq))
	return v
}

// Reset clears the seeded state.
func (u *Unwrap16) Reset() { u.inner.Reset() }

// SignedDelta16 returns the signed distance from prev to cur on a 16-bit
// ring, so -1 means "the packet immediately before prev" across a wrap.
func SignedDelta16(prev, cur uint16) int32 {
	d := int32(cur) - int32(prev)
	if d > 32767 {
		d -= 65536
	} else if d < -32768 {
		d += 65536
	}
	return d
}
