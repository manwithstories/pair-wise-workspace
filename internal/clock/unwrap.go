// Package clock reconstructs a monotonic decode timeline from raw fragment
// timestamps. It handles 32-bit wraparound, variable sample durations (VFR),
// and a linear drift fit against the external reference clock.
package clock

import (
	"errors"
	"fmt"

	"archive/internal/media"
)

// Window is the tick modulus of 32-bit tfdt fields.
const Window = int64(1) << 32

// ErrRegression marks a genuinely backward decode time (not a wrap).
var ErrRegression = errors.New("tfdt regression")

// Unwrap maps a raw 32-bit decode time onto the unwrapped value closest to
// prev, adding or subtracting whole windows. On the first call it returns raw.
//
// Because the counter is only 32 bits, a forward wrap and a large backward
// step are locally indistinguishable; Unwrap resolves them as "closest to
// prev", and the caller's monotonicity check rejects true regressions.
func Unwrap(raw, prev int64, havePrev bool) (int64, error) {
	if raw < 0 || raw >= Window {
		return 0, fmt.Errorf("%w: raw dts %d outside 32-bit window", media.ErrUnrecoverable, raw)
	}
	if !havePrev {
		return raw, nil
	}
	// Round (prev-raw)/Window to the nearest integer to pick the window.
	k := (prev - raw + Window/2) / Window
	// Guard the subtract-then-divide rounding for exact half-way cases.
	for raw+k*Window-prev > Window/2 {
		k--
	}
	for prev-(raw+k*Window) > Window/2 {
		k++
	}
	cand := raw + k*Window
	if cand < 0 {
		return 0, fmt.Errorf("%w: unwrapped dts negative", media.ErrUnrecoverable)
	}
	return cand, nil
}

// Wraps is how many 32-bit window crossings occurred between prev and the
// unwrapped next value.
func Wraps(next, prev int64) int64 {
	return (next - prev) / Window
}

// TrackCursor walks a track's samples, producing a strictly increasing
// unwrapped DTS sequence and drift-corrected shared-clock microseconds.
type TrackCursor struct {
	Timescale media.TimeScale

	prev     int64
	havePrev bool
	lastStep int64

	// PrevDTS is the last accepted unwrapped decode time.
	PrevDTS int64
	// WrapCount counts window crossings.
	WrapCount int64
	// RegressionCount counts rejected backward steps.
	RegressionCount int
	// AnchorMicros is the shared-clock origin for this track.
	AnchorMicros int64
	// Corrector holds the drift fit once installed.
	Corrector Corrector
	// Drift accumulates points for the fit.
	Drift DriftEstimator
}

// NewTrackCursor anchors a cursor at anchorMicros.
func NewTrackCursor(ts media.TimeScale, anchorMicros int64) *TrackCursor {
	return &TrackCursor{Timescale: ts, AnchorMicros: anchorMicros}
}

// StepUnwrapped unwraps raw against the cursor state and validates monotonicity.
// It does not mutate the cursor; call Accept to commit.
func (c *TrackCursor) StepUnwrapped(raw int64) (int64, error) {
	next, err := Unwrap(raw, c.prev, c.havePrev)
	if err != nil {
		return 0, err
	}
	if c.havePrev {
		if next < c.prev {
			return 0, fmt.Errorf("%w: %d -> %d", ErrRegression, c.prev, next)
		}
		step := next - c.prev
		if c.lastStep > 0 && step > 8*c.lastStep && step > 1<<20 {
			return 0, fmt.Errorf("%w: decode jump of %d ticks after stride %d", media.ErrUnrecoverable, step, c.lastStep)
		}
	}
	return next, nil
}

// Accept commits an unwrapped value returned by StepUnwrapped.
func (c *TrackCursor) Accept(next int64) {
	if c.havePrev {
		c.WrapCount += Wraps(next, c.prev)
		if d := next - c.prev; d > 0 {
			c.lastStep = d
		}
	}
	c.prev = next
	c.havePrev = true
	c.PrevDTS = next
}

// Micros converts an accepted unwrapped DTS to drift-corrected microseconds.
func (c *TrackCursor) Micros(dts int64) int64 {
	return c.Corrector.Micros(dts)
}

// DriftEstimator least-squares fits observed track time against reference time.
type DriftEstimator struct {
	refX []float64
	obsY []float64
}

// Add records one (reference, observed) microsecond pair.
func (d *DriftEstimator) Add(refMicros, obsMicros int64) {
	d.refX = append(d.refX, float64(refMicros))
	d.obsY = append(d.obsY, float64(obsMicros))
}

// Samples returns the accumulated point count.
func (d *DriftEstimator) Samples() int { return len(d.refX) }

// Fit returns the slope in ppm and the largest post-fit residual. Fewer than
// two points or a degenerate span yields a flat estimate.
func (d *DriftEstimator) Fit() media.DriftEstimate {
	est := media.DriftEstimate{Samples: len(d.refX)}
	if len(d.refX) == 0 {
		return est
	}
	est.OriginMicros = int64(d.refX[0])
	if len(d.refX) < 2 {
		return est
	}
	n := float64(len(d.refX))
	var sumX, sumY, sumXY, sumXX float64
	for i := range d.refX {
		x, y := d.refX[i], d.obsY[i]
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}
	den := n*sumXX - sumX*sumX
	if den == 0 {
		return est
	}
	slope := (n*sumXY - sumX*sumY) / den
	// slope is d(observed)/d(reference); convert a rate offset to ppm.
	est.SlopePPM = (slope - 1) * 1e6
	var maxRes float64
	intercept := (sumY - slope*sumX) / n
	for i := range d.refX {
		if r := abs(d.obsY[i] - (slope*d.refX[i] + intercept)); r > maxRes {
			maxRes = r
		}
	}
	est.ResidualMicros = int64(maxRes + 0.5)
	return est
}

// Anchor converts the estimate's origin to the track's tick space.
func (d *DriftEstimator) Anchor(ts media.TimeScale) int64 {
	if len(d.obsY) == 0 {
		return 0
	}
	return int64(d.obsY[0])
}

// Corrector applies a drift estimate while preserving the anchor.
type Corrector struct {
	TS       media.TimeScale
	Estimate media.DriftEstimate
}

// Micros maps an unwrapped tick value to shared-clock microseconds.
func (c Corrector) Micros(ticks int64) int64 {
	raw := c.TS.ToMicros(ticks)
	est := c.Estimate
	if est.SlopePPM == 0 {
		return raw
	}
	span := raw - est.OriginMicros
	return est.OriginMicros + span - int64(float64(span)*est.SlopePPM/1e6)
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
