package clock

import (
	"time"
)

const (
	// TicksPerSecond is the MPEG system clock rate.
	TicksPerSecond int64 = 27_000_000
	// TicksPerMilli is the number of 27 MHz ticks in one millisecond.
	TicksPerMilli int64 = 27_000
	// PTSHz is the MPEG timestamp clock rate.
	PTSHz int64 = 90_000
	// Wrap is the 33-bit modulus shared by PCR and PTS/DTS.
	Wrap int64 = 1 << 33
	// SyncTolerance is the maximum accepted audio/video presentation skew.
	SyncTolerance = 40 * time.Millisecond
	// SyncToleranceTicks is SyncTolerance expressed in 27 MHz ticks.
	SyncToleranceTicks int64 = 40 * TicksPerMilli
)

// Stamp is a signed, unwrapped 27 MHz tick count.
type Stamp int64

// Seconds returns the stamp as seconds.
func (s Stamp) Seconds() float64 { return float64(s) / float64(TicksPerSecond) }

// Millis returns the stamp as milliseconds.
func (s Stamp) Millis() float64 { return float64(s) / float64(TicksPerMilli) }

// Unwrapper turns a sequence of 33-bit modulo timestamps into a monotonic
// signed tick count. It tolerates a small backwards step (field reordering)
// and interprets anything larger than half the modulus as a wrap.
type Unwrapper struct {
	init bool
	last int64
}

// Unwrap extends v relative to the previous value.
func (u *Unwrapper) Unwrap(v uint64) int64 {
	x := int64(v % uint64(Wrap))
	if !u.init {
		u.init = true
		u.last = x
		return x
	}
	rem := u.last % Wrap
	delta := x - rem
	if delta < -(Wrap / 2) {
		delta += Wrap
	} else if delta >= Wrap/2 {
		delta -= Wrap
	}
	u.last += delta
	return u.last
}

// Reset clears the unwrap history.
func (u *Unwrapper) Reset() { *u = Unwrapper{} }

// Last returns the most recently unwrapped value (zero before first use).
func (u *Unwrapper) Last() int64 { return u.last }

// Timeline owns the independent PCR, PTS and DTS unwrap state for one
// elementary stream or program. PTS/DTS are converted to 27 MHz ticks.
type Timeline struct {
	pcr  Unwrapper
	pts  Unwrapper
	dts  Unwrapper
	seen bool
	last int64
}

// PCR extends a 33-bit PCR base read from a TS adaptation field, adds the
// 9-bit 27 MHz extension and checks forward progress.
func (t *Timeline) PCR(base uint64, ext uint16) (int64, error) {
	v := t.pcr.Unwrap(base)*300 + int64(ext)
	if t.seen && v < t.last {
		return v, FatalError(CodePCRNonMonotonic, "clock.PCR", "PCR moved backwards by %d ticks", t.last-v)
	}
	t.seen = true
	t.last = v
	return v, nil
}

// PCRValue extends a PCR without applying the monotonic check. It is used
// while resynchronising after an unrecoverable gap.
func (t *Timeline) PCRValue(base uint64, ext uint16) int64 {
	v := t.pcr.Unwrap(base)*300 + int64(ext)
	t.seen = true
	t.last = v
	return v
}

// PTS extends a 33-bit PTS and returns 27 MHz ticks.
func (t *Timeline) PTS(v uint64) int64 { return t.pts.Unwrap(v) * 300 }

// DTS extends a 33-bit DTS and returns 27 MHz ticks.
func (t *Timeline) DTS(v uint64) int64 { return t.dts.Unwrap(v) * 300 }

// Reset clears all unwrap state.
func (t *Timeline) Reset() { *t = Timeline{} }

// LastPCR returns the most recent unwrapped PCR, or 0 before the first one.
func (t *Timeline) LastPCR() int64 { return t.last }

// SyncDelta returns the absolute difference between two 27 MHz stamps.
func SyncDelta(a, b int64) time.Duration {
	d := a - b
	if d < 0 {
		d = -d
	}
	return time.Duration(d/27) * time.Microsecond
}

// CheckSync fails when audio and video presentation stamps differ by more
// than the 40 ms tolerance. drift is always reported.
func CheckSync(video, audio int64) (time.Duration, error) {
	drift := SyncDelta(video, audio)
	if drift > SyncTolerance {
		return drift, FatalError(CodeSyncDrift, "clock.CheckSync", "audio/video drift %s exceeds %s", drift, SyncTolerance)
	}
	return drift, nil
}

// Forward checks that next does not precede prev.
func Forward(prev, next int64, what string) error {
	if next < prev {
		return FatalError(CodeTimestampRegression, "clock.Forward", "%s regressed by %d ticks", what, prev-next)
	}
	return nil
}
