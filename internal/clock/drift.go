package clock

import (
	"math"

	"pairflow/internal/model"
)

// MaxBoundPPM is the accepted bounded oscillator correction range.
const MaxBoundPPM = 5_000.0

// Sample is one correspondence between an extended RTP tick and reference ns.
type Sample struct {
	ExtendedTS  uint64
	ReferenceNS int64
}

// DriftModel maps extended media ticks to the shared nanosecond time base.
type DriftModel struct {
	ClockRateHz uint32
	OriginTick  uint64
	OriginNS    int64
	// NSPerTick is nanoseconds per extended tick, after bounded correction.
	NSPerTick     float64
	DriftPPM      float64
	ResidualMaxNS int64
	Bounded       bool
}

// Map converts an extended timestamp to nanoseconds.
func (m DriftModel) Map(ext uint64) int64 {
	delta := float64(int64(ext - m.OriginTick))
	return m.OriginNS + SafeInt64NS(delta*m.NSPerTick)
}

// EstimateDrift fits a linear tick-to-reference mapping. The slope is clamped
// to MaxBoundPPM around the nominal clock rate so corrupted metadata cannot
// produce an unbounded time base.
func EstimateDrift(rateHz uint32, samples []Sample) (DriftModel, *model.MediaError) {
	modelOut := DriftModel{ClockRateHz: rateHz, NSPerTick: 1e9 / float64(rateHz)}
	if rateHz == 0 {
		return modelOut, &model.MediaError{Code: model.CodeMetadataInvalid, Message: "zero clock rate"}
	}
	if len(samples) < 2 {
		if len(samples) == 1 {
			modelOut.OriginTick = samples[0].ExtendedTS
			modelOut.OriginNS = samples[0].ReferenceNS
		}
		return modelOut, &model.MediaError{Code: model.CodeClockUncertain, Message: "drift estimate needs at least two samples", Recoverable: true}
	}

	x0 := float64(samples[0].ExtendedTS)
	y0 := float64(samples[0].ReferenceNS)
	var sx, sy, sxx, sxy float64
	for _, s := range samples {
		x := float64(s.ExtendedTS) - x0
		y := float64(s.ReferenceNS) - y0
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	n := float64(len(samples))
	den := sxx - sx*sx/n
	if math.Abs(den) < 1e-9 {
		return modelOut, &model.MediaError{Code: model.CodeClockUncertain, Message: "constant timestamps cannot estimate drift", Recoverable: true}
	}
	slope := (sxy - sx*sy/n) / den
	intercept := sy/n - slope*sx/n

	nominal := 1e9 / float64(rateHz)
	// ppm is expressed as oscillator rate error: a fast clock has smaller
	// observed nanoseconds per tick than nominal.
	ppm := (nominal/slope - 1) * 1e6
	bounded := false
	if math.Abs(ppm) > MaxBoundPPM || math.IsNaN(ppm) || math.IsInf(ppm, 0) {
		bounded = true
		if ppm > MaxBoundPPM {
			ppm = MaxBoundPPM
		} else if ppm < -MaxBoundPPM || math.IsNaN(ppm) {
			ppm = -MaxBoundPPM
		}
		slope = nominal / (1 + ppm/1e6)
	}

	modelOut.NSPerTick = slope
	modelOut.DriftPPM = ppm
	modelOut.Bounded = bounded
	// intercept is in relative coordinates; preserve it by choosing origin at
	// the first sample plus intercept.
	modelOut.OriginTick = samples[0].ExtendedTS
	modelOut.OriginNS = samples[0].ReferenceNS + SafeInt64NS(intercept)

	var maxRes float64
	for _, s := range samples {
		pred := modelOut.Map(s.ExtendedTS)
		res := math.Abs(float64(pred - s.ReferenceNS))
		if res > maxRes {
			maxRes = res
		}
	}
	modelOut.ResidualMaxNS = SafeInt64NS(maxRes)
	return modelOut, nil
}
