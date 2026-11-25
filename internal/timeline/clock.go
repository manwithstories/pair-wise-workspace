// Package timeline extends the modulo-33-bit MPEG timestamps (PCR/PTS/DTS)
// into a single monotonic 90 kHz media timeline and reports discontinuities
// that cannot be explained by a legal wrap.
package timeline

import (
	"streamcut/internal/media"
)

const (
	// Mod33 is the MPEG 33-bit timestamp modulus: 2^33 90 kHz ticks,
	// roughly 26.5 hours.
	Mod33 int64 = 1 << 33
	// HalfMod is half of Mod33, the unwrap decision boundary.
	HalfMod int64 = Mod33 / 2
	// MaxForwardJump is the largest forward delta accepted as continuous.
	// Anything larger is treated as an unprovable gap rather than guessed.
	// 10 seconds at 90 kHz; PCR/PTS normally advance per frame (~tens of ms).
	MaxForwardJump int64 = int64(media.TicksPerSecond) * 10
)

// Unwrapper extends one stream of raw 33-bit stamps into monotonic int64
// ticks. A legal wrap (raw near 2^33 followed by raw near 0) produces a small
// positive delta; true backward jumps and implausibly large forward jumps
// are returned as gaps without fabricating a time.
type Unwrapper struct {
	initialized bool
	lastRaw     uint64
	cycles      int64 // number of completed Mod33 wraps
	gaps        int
}

// NewUnwrapper returns an empty unwrapper.
func NewUnwrapper() *Unwrapper { return &Unwrapper{} }

// SeedUnwrapper returns an unwrapper anchored so the first observed raw stamp
// is extended onto the same absolute timeline as a prior run. baseExtended is
// the extended tick at which the resumed stream's first raw stamp should sit.
// A resumed tail uses it instead of restarting its clock at zero, which keeps
// its cut boundaries identical to an uninterrupted run.
func SeedUnwrapper(firstRaw uint64, baseExtended media.Time90) *Unwrapper {
	u := &Unwrapper{initialized: true, lastRaw: firstRaw & uint64(Mod33-1)}
	u.cycles = (int64(baseExtended) - int64(u.lastRaw)) / Mod33
	return u
}

// Gaps returns how many stamps could not be proven continuous.
func (u *Unwrapper) Gaps() int { return u.gaps }

// Last returns the most recent extended timestamp.
func (u *Unwrapper) Last() media.Time90 {
	return media.Time90(u.cycles*Mod33 + int64(u.lastRaw))
}

// Extend maps one raw 33-bit stamp onto the extended timeline. The boolean is
// true when this stamp starts a gap: no timestamp is invented — the stream is
// re-anchored at raw's own value on its current wrap cycle, and monotonicity
// resumes from there.
func (u *Unwrapper) Extend(raw uint64) (media.Time90, bool) {
	raw &= uint64(Mod33 - 1)
	if !u.initialized {
		u.initialized = true
		u.lastRaw = raw
		return u.Last(), false
	}
	delta := int64(raw) - int64(u.lastRaw)
	wrapped := false
	if delta < -HalfMod {
		// Forward wrap: raw went from near Mod33 back to near zero, and the
		// true media time keeps increasing.
		delta += Mod33
		wrapped = true
	} else if delta > HalfMod {
		// Apparent huge forward delta is really a backward jump across zero.
		delta -= Mod33
	}
	switch {
	case delta < 0:
		// A real backward jump (a legal wrap would already be positive).
		u.gaps++
		u.lastRaw = raw
		return u.Last(), true
	case delta > MaxForwardJump:
		// Unprovable: could be a long outage or a broken clock; do not guess.
		u.gaps++
		u.lastRaw = raw
		return u.Last(), true
	default:
		if wrapped {
			u.cycles++
		}
		u.lastRaw = raw
		return u.Last(), false
	}
}

// Clock is the program-level timeline driven by PCR. PCR arrives as a 42-bit
// 27 MHz value (33-bit base << 9 | 9-bit extension); the program clock is
// consumed in 90 kHz ticks, so the 27 MHz extension is divided away.
type Clock struct {
	pcr    Unwrapper
	pcr27  bool // last input carried a 27 MHz extension
	gapNow bool // sticky per-sample flag for the AUs of this PCR epoch
}

// NewClock returns an empty program clock.
func NewClock() *Clock { return &Clock{} }

// ExtendPCRBase extends a PCR expressed directly as a 33-bit 90 kHz base.
func (c *Clock) ExtendPCRBase(base uint64) (media.Time90, bool) {
	t, gap := c.pcr.Extend(base)
	c.gapNow = gap
	return t, gap
}

// ExtendPCR27 extends a full 42-bit 27 MHz PCR, discarding the sub-90kHz
// extension (base*300 + ext)/300 == base; the remainder never exceeds one
// 90 kHz tick.
func (c *Clock) ExtendPCR27(pcr uint64) (media.Time90, bool) {
	return c.ExtendPCRBase(pcr / 300)
}

// Gaps reports the number of PCR discontinuities seen so far.
func (c *Clock) Gaps() int { return c.pcr.gaps }

// EpochGap reports whether the most recent PCR was discontinuous; PES
// timestamps emitted before the next PCR inherit the gap flag.
func (c *Clock) EpochGap() bool { return c.gapNow }

// ClearEpochGap resets the inherited-gap marker once a healthy PCR lands.
func (c *Clock) ClearEpochGap() { c.gapNow = false }
