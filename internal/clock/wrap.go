package clock

import (
	"fmt"
	"time"
)

// RTPMod is the 32-bit RTP timestamp modulus (RFC 3550 §5.1).
const RTPMod int64 = 1 << 32

// RawPoint is one observed RTP timestamp paired with its position on the
// reference (program) timeline, built from capture order rather than content.
type RawPoint struct {
	Raw32   uint32 // timestamp as carried in the RTP header
	RefTime Time   // reference timeline position of this observation
	Offset  int64  // byte offset inside the stream, for error location
}

// Unwrapped is the result of expanding one 32-bit timestamp onto the
// monotonic 64-bit timeline.
type Unwrapped struct {
	Linear int64 // full 64-bit tick count, never decreasing for accepted input
	Time   Time  // position on the unified timeline
	Wrapped bool  // true when this observation crossed a modulo boundary
	Late    bool  // true when the packet arrived after the committed frontier
}

// Unwrapper32 expands 32-bit RTP timestamps into a monotonic 64-bit series.
// Once a timestamp has been committed, late/out-of-order packets are flagged
// but never move the committed frontier, so the submitted timeline is stable.
type Unwrapper32 struct {
	rate    Rate
	mod     int64
	lastRaw int64 // last accepted raw value, expressed in 64-bit ticks
	front   int64 // highest committed linear tick
	started bool
}

// NewUnwrapper32 builds an unwrapper for the given (valid) clock rate.
func NewUnwrapper32(rate Rate) (*Unwrapper32, error) {
	if !rate.Valid() {
		return nil, ErrZeroClock
	}
	return &Unwrapper32{rate: rate, mod: RTPMod, lastRaw: 0, front: -1}, nil
}

// Push consumes one observation. Late packets (raw ticks below the committed
// frontier) are returned with Late=true and do not mutate internal state.
func (u *Unwrapper32) Push(p RawPoint) (Unwrapped, *Error) {
	raw := int64(p.Raw32)
	if !u.started {
		u.started = true
		u.lastRaw = raw
		u.front = raw
		t, err := u.rate.ToTimeLocated(raw, 0, p.Offset)
		if err != nil {
			return Unwrapped{}, err
		}
		return Unwrapped{Linear: raw, Time: t}, nil
	}

	delta := raw - (u.lastRaw % u.mod)
	wrapped := false
	if delta < -u.mod/2 {
		// Forward wrap: raw32 crossed zero on its way up.
		delta += u.mod
		wrapped = true
	} else if delta > u.mod/2 {
		// Backward wrap indicates a reorder, not a real clock movement.
		delta -= u.mod
	}
	candidate := u.lastRaw + delta

	if candidate < u.front {
		// Late/duplicate packet: report it but keep the committed timeline.
		t, err := u.rate.ToTimeLocated(u.front, 0, p.Offset)
		if err != nil {
			return Unwrapped{}, err
		}
		return Unwrapped{Linear: u.front, Time: t, Wrapped: wrapped, Late: true}, nil
	}

	u.lastRaw = candidate
	u.front = candidate
	t, err := u.rate.ToTimeLocated(candidate, 0, p.Offset)
	if err != nil {
		return Unwrapped{}, err
	}
	return Unwrapped{Linear: candidate, Time: t, Wrapped: wrapped}, nil
}

// Frontier returns the highest committed linear tick.
func (u *Unwrapper32) Frontier() int64 { return u.front }

// DriftPoint pairs reference time with the sender's own linear clock reading.
type DriftPoint struct {
	Linear  int64 // unwrapped sender tick
	RefTime Time  // reference (wall/program) position
}

// DriftEstimator fits sender clock vs. reference time and reports sustained
// drift in parts-per-million. Sums are kept in float64 so a long stream can
// neither overflow int64 nor suffer integer quantization of sub-tick slopes.
type DriftEstimator struct {
	rate Rate
	n    float64
	sx   float64 // sum of reference nanoseconds
	sy   float64 // sum of linear ticks
	sxx  float64
	sxy  float64
}

// NewDriftEstimator requires the sender's nominal clock rate.
func NewDriftEstimator(rate Rate) (*DriftEstimator, error) {
	if !rate.Valid() {
		return nil, ErrZeroClock
	}
	return &DriftEstimator{rate: rate}, nil
}

// Add records one paired observation, rejecting negative reference time.
func (d *DriftEstimator) Add(p DriftPoint) error {
	x := float64(p.RefTime.Nanoseconds())
	if x < 0 {
		return ErrNegative
	}
	d.n++
	d.sx += x
	d.sy += float64(p.Linear)
	d.sxx += x * x
	d.sxy += x * float64(p.Linear)
	return nil
}

// SlopePPM returns the fitted slope expressed as ppm deviation from the
// nominal rate. Positive values mean the sender clock runs fast.
func (d *DriftEstimator) SlopePPM() (float64, error) {
	if d.n < 2 {
		return 0, fmt.Errorf("clock: need >=2 points for drift, have %d", int64(d.n))
	}
	denom := d.n*d.sxx - d.sx*d.sx
	if denom == 0 {
		return 0, fmt.Errorf("clock: degenerate drift fit (no time span)")
	}
	// slope ticks per reference nanosecond
	m := (d.n*d.sxy - d.sx*d.sy) / denom
	nominal := float64(d.rate.Num) / float64(d.rate.Den) / 1e9
	return (m/nominal - 1) * 1e6, nil
}

// ExceedsThreshold reports whether |ppm| is over the configured limit.
func ExceedsThreshold(ppm, thresholdPPM float64) bool {
	if thresholdPPM < 0 {
		return false
	}
	if ppm < 0 {
		ppm = -ppm
	}
	return ppm > thresholdPPM
}

// DriftWindow marks the program-time ranges affected by an over-threshold
// drift reading, so the split stage can flag the owning segments.
type DriftWindow struct {
	Range
	PPM float64
}

// WindowFromPoint builds the affected window around a single over-threshold
// reading using a fixed look-back/look-ahead radius.
func WindowFromPoint(t Time, radius time.Duration, ppm float64) (DriftWindow, error) {
	start := t - Time(radius)
	if start < 0 {
		start = 0
	}
	end, err := t.Add(radius)
	if err != nil {
		return DriftWindow{}, err
	}
	return DriftWindow{Range: Range{Start: start, End: end}, PPM: ppm}, nil
}
