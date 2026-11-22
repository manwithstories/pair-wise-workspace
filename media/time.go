// Package media defines the shared 90 kHz media time base, 32-bit RTP
// timestamp unwrapping and progressive drift correction used by every
// pipeline stage (scan, align and stream).
package media

import "math"

// ClockRate is the unified media clock: 90 kHz, the RTP video clock.
const ClockRate uint32 = 90000

// Ticks is a signed 64-bit media time expressed in 90 kHz ticks. A single
// tick is therefore 1/90000 s (~11.1 us).
type Ticks int64

// WrapMod is the modulus of the 32-bit RTP timestamp space.
const WrapMod uint64 = 1 << 32

// HalfWrap is the decision threshold: a forward jump smaller than this is a
// normal step, a backward jump or a jump beyond half the space is only
// accepted as a wrap when the sequence number proves it.
const HalfWrap uint32 = 1 << 31

// FromSeconds converts a duration in seconds to ticks.
func FromSeconds(seconds float64) Ticks {
	return Ticks(math.Round(seconds * float64(ClockRate)))
}

// Seconds converts ticks to fractional seconds.
func (t Ticks) Seconds() float64 {
	return float64(t) / float64(ClockRate)
}

// Milliseconds converts ticks to whole milliseconds, rounded.
func (t Ticks) Milliseconds() int64 {
	return int64(math.Round(float64(t) * 1000 / float64(ClockRate)))
}

// WrapEvent records one accepted 32-bit wrap.
type WrapEvent struct {
	// Sequence is the RTP sequence number at which the wrap was observed.
	Sequence uint16
	// RawTimestamp is the wrapped 32-bit timestamp observed.
	RawTimestamp uint32
	// Unwrapped is the resulting 64-bit timestamp.
	Unwrapped Ticks
}

// UnwrapResult is the outcome of feeding one RTP packet to the unwrapper.
type UnwrapResult struct {
	Unwrapped Ticks
	Wrapped   bool
}

// WrapUnwrapper expands 32-bit RTP timestamps into a monotonic 64-bit tick
// line. A wrap is accepted only when either the raw jump crosses the half
// space boundary or the RTP sequence number advanced while the raw value
// went backwards; otherwise the track is stopped with a diagnosable error.
type WrapUnwrapper struct {
	initialized bool
	lastSeq     uint16
	lastRaw     uint32
	lastUnwrap  Ticks
	base        Ticks
	events      []WrapEvent
}

// NewWrapUnwrapper creates an unwrapper whose first timestamp is mapped to
// base (typically the non-zero edit-list start time).
func NewWrapUnwrapper(base Ticks) *WrapUnwrapper {
	return &WrapUnwrapper{base: base}
}

// Events returns all accepted wrap events.
func (u *WrapUnwrapper) Events() []WrapEvent { return u.events }

// Last returns the most recent unwrapped timestamp.
func (u *WrapUnwrapper) Last() Ticks { return u.lastUnwrap }

// Push feeds one (sequence number, raw timestamp) pair. ok is false when the
// movement cannot be explained as a wrap: the caller must stop the track.
func (u *WrapUnwrapper) Push(seq uint16, raw uint32) (UnwrapResult, bool) {
	if !u.initialized {
		u.initialized = true
		u.lastSeq = seq
		u.lastRaw = raw
		u.lastUnwrap = u.base + Ticks(raw)
		return UnwrapResult{Unwrapped: u.lastUnwrap}, true
	}

	seqDelta := int32(seq - u.lastSeq) // wraps naturally modulo 65536
	rawDelta := int64(raw) - int64(u.lastRaw)
	isWrap := false

	switch {
	case rawDelta > int64(HalfWrap):
		// Forward jump beyond half the space: a backward raw step plus one
		// full modulus.
		rawDelta -= int64(WrapMod)
		isWrap = true
	case rawDelta < -int64(HalfWrap):
		// Raw value jumped far upward: the canonical wrap case.
		rawDelta += int64(WrapMod)
		isWrap = true
	case rawDelta <= 0:
		// Small backward/zero step. Acceptable only if the sequence number
		// proves one full timestamp rotation happened.
		if seqDelta > 0 && int64(seqDelta)*int64(HalfWrap) >= int64(WrapMod) {
			rawDelta += int64(WrapMod)
			isWrap = true
		} else {
			return UnwrapResult{}, false
		}
	}

	u.lastUnwrap += Ticks(rawDelta)
	if isWrap {
		u.events = append(u.events, WrapEvent{
			Sequence:     seq,
			RawTimestamp: raw,
			Unwrapped:    u.lastUnwrap,
		})
	}
	u.lastSeq = seq
	u.lastRaw = raw
	return UnwrapResult{Unwrapped: u.lastUnwrap, Wrapped: isWrap}, true
}

// DriftSample is one observation fed to the drift corrector: the RTP-derived
// unwrapped timestamp and the reference tick taken from the MP4 sample
// table at the same sample.
type DriftSample struct {
	Observed  Ticks
	Reference Ticks
}

// Correction records a single drift adjustment.
type Correction struct {
	// Sequence is the RTP sequence number where the correction was applied.
	Sequence uint16
	// Amount is the signed correction added to the observed timeline.
	Amount Ticks
	// Residual is the remaining observed-minus-reference error afterwards.
	Residual Ticks
}

// DriftCorrector tracks the slow, monotonic drift between the RTP clock and
// the container reference clock using an exponentially weighted estimator,
// applying corrections only in bounded steps so a single bad sample cannot
// jump the timeline.
type DriftCorrector struct {
	gain         float64
	maxStep      Ticks
	initialized  bool
	estimate     float64
	corrections  []Correction
	totalApplied Ticks
}

// NewDriftCorrector builds a corrector. gain is the EWMA gain in (0,1] and
// maxStep bounds the absolute correction applied per sample.
func NewDriftCorrector(gain float64, maxStep Ticks) *DriftCorrector {
	if gain <= 0 || gain > 1 {
		gain = 0.05
	}
	if maxStep <= 0 {
		maxStep = 1
	}
	return &DriftCorrector{gain: gain, maxStep: maxStep}
}

// Update consumes one paired observation and returns the corrected
// timestamp and the correction applied. The estimator tracks the residual
// drift left after the corrections already applied, so it converges to zero
// once the accumulated correction matches the real clock offset.
func (d *DriftCorrector) Update(seq uint16, s DriftSample) (Ticks, Correction) {
	residual0 := s.Observed - s.Reference - d.totalApplied
	err0 := float64(residual0)
	if !d.initialized {
		d.initialized = true
		d.estimate = err0
	} else {
		d.estimate = d.estimate*(1-d.gain) + err0*d.gain
	}

	amount := Ticks(math.Round(d.estimate))
	if amount > d.maxStep {
		amount = d.maxStep
	} else if amount < -d.maxStep {
		amount = -d.maxStep
	}
	d.totalApplied += amount
	c := Correction{
		Sequence: seq,
		Amount:   amount,
		Residual: s.Observed - d.totalApplied - s.Reference,
	}
	d.corrections = append(d.corrections, c)
	return s.Observed - d.totalApplied, c
}

// TotalCorrection is the cumulative signed correction applied so far.
func (d *DriftCorrector) TotalCorrection() Ticks { return d.totalApplied }

// Corrections returns every individual correction record.
func (d *DriftCorrector) Corrections() []Correction { return d.corrections }
