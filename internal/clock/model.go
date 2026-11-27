// Package clock provides a rational time base shared by the RTP window,
// recovery budget and the audio/video aligner. All modules reason in integer
// ticks so that repeated runs produce bit-identical results.
package clock

import "pairwise/ingestcheck/internal/clock/rat"

// MaxTicksPerSecond bounds the rational time base so tick counts stay inside
// the int64 range for the durations used by the pipeline.
const MaxTicksPerSecond int64 = 1 << 32

// Rate is a rational time base expressed as Ticks per Second; both values are
// integral so repeated runs cannot drift.
type Rate struct {
	Ticks  int64
	Second int64
}

// NewRate validates and normalises a rational rate.
func NewRate(ticks, second int64) (Rate, error) {
	if ticks <= 0 || second <= 0 {
		return Rate{}, ErrInvalidRate
	}
	g := rat.GCD(ticks, second)
	ticks /= g
	second /= g
	if ticks > MaxTicksPerSecond || second > MaxTicksPerSecond {
		return Rate{}, ErrRateOverflow
	}
	return Rate{Ticks: ticks, Second: second}, nil
}

// CDN returns the 90 kHz clock used by most CMAF video tracks.
func CDN() Rate { return Rate{Ticks: 90000, Second: 1} }

// Audio returns the 48 kHz time base used by the demo audio tracks.
func Audio() Rate { return Rate{Ticks: 48000, Second: 1} }

// TicksFor converts microseconds to ticks, rounding down.
func (r Rate) TicksFor(micros int64) (int64, error) {
	if micros < 0 {
		return 0, ErrNegativeDuration
	}
	return r.scale(uint64(micros), uint64(r.Ticks), 1000000*uint64(r.Second), "clock.ticks_for")
}

// Micros converts ticks to microseconds, rounding down.
func (r Rate) Micros(ticks int64) (int64, error) {
	if ticks < 0 {
		return 0, ErrNegativeDuration
	}
	return r.scale(uint64(ticks), 1000000*uint64(r.Second), uint64(r.Ticks), "clock.micros")
}

// scale evaluates in * num / den with a 128-bit intermediate and range checks.
func (r Rate) scale(in, num, den uint64, op string) (int64, error) {
	_ = r
	v, err := rat.MulDiv(in, num, den)
	if err != nil {
		return 0, Wrap(CodeRateOverflow, op, -1, ErrRateOverflow)
	}
	if v > 1<<62 {
		return 0, ErrRateOverflow
	}
	return int64(v), nil
}

// Convert re-expresses ticks from one rate to another.
func Convert(ticks int64, from, to Rate) (int64, error) {
	if ticks < 0 {
		return 0, ErrNegativeDuration
	}
	us, err := from.Micros(ticks)
	if err != nil {
		return 0, err
	}
	return to.TicksFor(us)
}

// ScaledEqual reports whether two integer tick values represent the same
// instant under their rates, within toleranceMicros.
func ScaledEqual(a int64, ra Rate, b int64, rb Rate, toleranceMicros int64) (bool, error) {
	ma, err := ra.Micros(a)
	if err != nil {
		return false, err
	}
	mb, err := rb.Micros(b)
	if err != nil {
		return false, err
	}
	return absDiff(ma, mb) <= toleranceMicros, nil
}

// Duration is the half-open tick span [Start, Start+Len).
type Duration struct {
	Start int64
	Len   int64
}

// End returns the exclusive end tick.
func (d Duration) End() int64 { return d.Start + d.Len }

// Overlaps reports whether two durations share any tick.
func (d Duration) Overlaps(o Duration) bool { return d.Start < o.End() && o.Start < d.End() }

// Timeline is an explicit, append-only mapping from media ticks to wall-clock
// ticks. A tfdt regression opens a new epoch rather than rewriting history.
type Timeline struct {
	Rate   Rate
	Epoch  int
	Start  int64 // wall-clock tick where this epoch begins
	Media  int64 // media tick where this epoch begins
	Span   int64 // wall-clock length of the epoch so far
	Pinned bool  // set once a regression forced a new epoch
}

// NewTimeline creates an epoch-zero timeline anchored at both origins.
func NewTimeline(rate Rate) Timeline { return Timeline{Rate: rate} }

// Wall maps a media tick to wall ticks within the current timeline. A value
// predating the anchor is extrapolated backwards without mutating anything.
func (t Timeline) Wall(media int64) int64 { return t.Start + (media - t.Media) }

// Regress opens a new epoch at wall tick w while keeping media timestamps
// untouched, so a tfdt regression is recorded rather than rewritten.
func (t *Timeline) Regress(wall, media int64) error {
	if wall < t.Start {
		return ErrMonotonicWall
	}
	t.Epoch++
	t.Pinned = true
	t.Start = wall
	t.Media = media
	t.Span = 0
	return nil
}

// Advance records wall time consumed since the last call.
func (t *Timeline) Advance(delta int64) {
	if delta > 0 {
		t.Span += delta
	}
}

// SkewMicros compares a media tick against a wall tick under the rate.
func (t Timeline) SkewMicros(media, wall int64) (int64, error) {
	return t.Rate.Micros(media - t.Wall(wall))
}

func absDiff(a, b int64) int64 {
	if a > b {
		return a - b
	}
	return b - a
}
