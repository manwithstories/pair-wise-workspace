// Package timebase implements clock drift calibration from rational
// anchors and 33-bit PTS unwrapping.
package timebase

import (
	"math/big"

	"archive/internal/media"
)

// MaxDriftPPM bounds the clock correction we are willing to apply.
// Anchors implying a larger drift are rejected as unrecoverable.
const MaxDriftPPM = int64(500)

// Calibrator maps local device-clock nanoseconds onto the reference
// timeline using a rational slope fitted through two anchor endpoints:
//
//	ref(t) = ref0 + (t - local0) * refSpan / localSpan
type Calibrator struct {
	local0, ref0     int64
	refSpan, locSpan int64
}

// Fit derives a Calibrator from at least two anchors. The implied drift
// must not exceed ±MaxDriftPPM, otherwise a calibrate-stage Fault is
// returned (the anchors are deemed unrecoverable).
func Fit(anchors []media.Anchor) (*Calibrator, error) {
	if len(anchors) < 2 {
		return nil, media.Faultf(media.StageCalibrate, -1, 0,
			"need at least 2 anchors, got %d", len(anchors))
	}
	first, last := anchors[0], anchors[len(anchors)-1]
	locSpan := last.Local - first.Local
	refSpan := last.Ref - first.Ref
	if locSpan <= 0 {
		return nil, media.Faultf(media.StageCalibrate, -1, 0,
			"non-positive local anchor span %d", locSpan)
	}
	// ppm = (refSpan - locSpan) * 1e6 / locSpan, computed in big.Int to
	// stay exact for any span.
	diff := big.NewInt(refSpan - locSpan)
	diff.Abs(diff)
	diff.Mul(diff, big.NewInt(1_000_000))
	ppm := new(big.Int).Quo(diff, big.NewInt(locSpan)).Int64()
	if ppm > MaxDriftPPM {
		return nil, media.Faultf(media.StageCalibrate, -1, 0,
			"anchor drift %d ppm exceeds ±%d ppm limit", ppm, MaxDriftPPM)
	}
	return &Calibrator{local0: first.Local, ref0: first.Ref, refSpan: refSpan, locSpan: locSpan}, nil
}

// Identity returns a Calibrator that maps timestamps unchanged.
func Identity() *Calibrator {
	return &Calibrator{refSpan: 1, locSpan: 1}
}

// Apply converts a local timestamp to reference nanoseconds, rounding to
// nearest. The rational slope keeps the residual well under one 48 kHz
// sample period.
func (c *Calibrator) Apply(local int64) int64 {
	d := big.NewInt(local - c.local0)
	d.Mul(d, big.NewInt(c.refSpan))
	den := big.NewInt(c.locSpan)
	// Round to nearest: add/sub half denominator before division.
	half := new(big.Int).Rsh(new(big.Int).Abs(den), 1)
	if d.Sign() >= 0 {
		d.Add(d, half)
	} else {
		d.Sub(d, half)
	}
	d.Quo(d, den)
	return c.ref0 + d.Int64()
}

// DriftPPM reports the fitted drift in parts per million (signed).
func (c *Calibrator) DriftPPM() int64 {
	if c.locSpan == 0 {
		return 0
	}
	d := big.NewInt(c.refSpan - c.locSpan)
	d.Mul(d, big.NewInt(1_000_000))
	den := big.NewInt(c.locSpan)
	half := new(big.Int).Rsh(new(big.Int).Abs(den), 1)
	if d.Sign() >= 0 {
		d.Add(d, half)
	} else {
		d.Sub(d, half)
	}
	return d.Quo(d, den).Int64()
}
