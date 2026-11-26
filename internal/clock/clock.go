// Package clock normalizes per-track raw timestamps onto the shared
// microsecond timeline, estimates and removes linear clock drift, and detects
// zero clocks, overflow and negative durations using the shared error model.
package clock

import (
	"fmt"
	"math"

	"mediaflow/internal/fixture"
	"mediaflow/internal/timeline"
)

// DriftLimitUS is the maximum tolerated residual position error after drift
// correction. The acceptance bar is 20 ms of audio/video sync error.
const DriftLimitUS int64 = 20_000

// TrackModel holds the estimated linear mapping rawTicks -> true microseconds
// for one track: trueUS ≈ (rawTicks - intercept) * slope.
type TrackModel struct {
	Track     uint32
	Timebase  timeline.Rational
	DriftPPM  int     // estimated signed drift in parts per million
	Slope     float64 // microseconds per raw tick
	Intercept float64 // raw-tick intercept at the timeline origin
	Samples   int
}

// Normalized is a packet mapped onto the standard microsecond scale while
// retaining its native frame duration (VFR preserved).
type Normalized struct {
	Track     uint32
	Seq       uint32
	StartUS   int64 // drift-corrected position on the standard scale
	Duration  int64 // native frame duration converted to microseconds
	NativeDur int64 // duration in native ticks (unchanged)
	Recovered bool
	Payload   []byte
}

// Normalizer keeps one TrackModel per track and reports structured errors.
type Normalizer struct {
	models map[uint32]*TrackModel
	errs   []*timeline.Error
}

// NewNormalizer builds an empty normalizer.
func NewNormalizer() *Normalizer {
	return &Normalizer{models: map[uint32]*TrackModel{}}
}

// Estimate fits a linear drift model per track from raw timestamps and the
// ground-truth microsecond positions carried by the fixture. It rejects zero
// clocks and overflow via the timeline error sentinels.
func (n *Normalizer) Estimate(packets []fixture.Packet, tracks []fixture.Track) error {
	type accum struct {
		sumX, sumY, sumXY, sumXX float64
		n                        int
		firstRaw, lastRaw        int64
		firstUS, lastUS          int64
		have                     bool
	}
	accs := map[uint32]*accum{}
	tbByTrack := map[uint32]timeline.Rational{}
	for _, tr := range tracks {
		tb, err := timeline.NewRational(tr.Timebase.Num, tr.Timebase.Den)
		if err != nil {
			return timeline.TrackError("clock.estimate", tr.ID, 0, timeline.RationalZero, false, err)
		}
		tbByTrack[tr.ID] = tb
	}

	for _, p := range packets {
		if p.IsParity {
			continue
		}
		acc, ok := accs[p.Track]
		if !ok {
			// Tracks without media packets (subtitle/chapter/metadata event
			// carriers) simply have no clock model and are skipped.
			if _, known := tbByTrack[p.Track]; !known {
				return timeline.TrackError("clock.estimate", p.Track, p.RawTS,
					tbByTrack[p.Track], false, fmt.Errorf("unknown track"))
			}
			acc = &accum{}
			accs[p.Track] = acc
		}
		if p.RawTS < 0 {
			return timeline.TrackError("clock.estimate", p.Track, p.RawTS,
				tbByTrack[p.Track], false, fmt.Errorf("%w: negative raw timestamp", timeline.ErrOverflow))
		}
		x := float64(p.RawTS)
		y := float64(p.TrueUS)
		acc.sumX += x
		acc.sumY += y
		acc.sumXY += x * y
		acc.sumXX += x * x
		acc.n++
		if !acc.have {
			acc.firstRaw, acc.firstUS = p.RawTS, p.TrueUS
			acc.have = true
		}
		acc.lastRaw, acc.lastUS = p.RawTS, p.TrueUS
	}

	for id, acc := range accs {
		tb := tbByTrack[id]
		if !acc.have || acc.lastRaw == acc.firstRaw {
			err := timeline.TrackError("clock.estimate", id, 0, tb, false,
				fmt.Errorf("%w: track %d clock never advances", timeline.ErrZeroClock, id))
			n.errs = append(n.errs, err)
			return err
		}
		// Least-squares slope; fall back to endpoint ratio on a degenerate fit.
		denom := float64(acc.n)*acc.sumXX - acc.sumX*acc.sumX
		var slope float64
		if math.Abs(denom) < 1e-9 {
			slope = float64(acc.lastUS-acc.firstUS) / float64(acc.lastRaw-acc.firstRaw)
		} else {
			slope = (float64(acc.n)*acc.sumXY - acc.sumX*acc.sumY) / denom
		}
		if !(slope > 0) || math.IsInf(slope, 0) || math.IsNaN(slope) {
			return timeline.TrackError("clock.estimate", id, 0, tb, false,
				fmt.Errorf("%w: non-positive fitted slope %v", timeline.ErrOverflow, slope))
		}
		intercept := (acc.sumY - slope*acc.sumX) / float64(acc.n)
		// Drift PPM relative to the ideal microseconds-per-tick factor.
		ideal, err := idealMicrosPerTick(tb)
		if err != nil {
			return timeline.TrackError("clock.estimate", id, 0, tb, false, err)
		}
		ppm := int(math.Round((slope/ideal - 1.0) * 1_000_000))
		n.models[id] = &TrackModel{
			Track: id, Timebase: tb, DriftPPM: ppm,
			Slope: slope, Intercept: intercept, Samples: acc.n,
		}
	}
	return nil
}

func idealMicrosPerTick(tb timeline.Rational) (float64, error) {
	one, err := timeline.ToStandard(1, tb)
	if err != nil {
		return 0, err
	}
	return float64(one), nil
}

// Normalize maps one packet's raw timestamp through its fitted model and
// converts the native duration to microseconds, rejecting negative durations.
func (n *Normalizer) Normalize(p fixture.Packet) (Normalized, error) {
	m, ok := n.models[p.Track]
	if !ok {
		return Normalized{}, timeline.TrackError("clock.normalize", p.Track, p.RawTS,
			timeline.RationalZero, false, fmt.Errorf("no drift model for track"))
	}
	corrected := int64(math.Round((float64(p.RawTS) - m.Intercept) * m.Slope))
	// Clamp origin noise: fitted intercept can push the earliest frame a few
	// microseconds negative even though media time starts at zero.
	if corrected < 0 && corrected > -DriftLimitUS {
		corrected = 0
	}
	if corrected < 0 {
		return Normalized{}, timeline.TrackError("clock.normalize", p.Track, p.RawTS,
			m.Timebase, false, fmt.Errorf("%w: corrected position %d", timeline.ErrOverflow, corrected))
	}
	durUS, err := timeline.ToStandard(p.Duration, m.Timebase)
	if err != nil {
		return Normalized{}, timeline.TrackError("clock.normalize", p.Track, p.RawTS,
			m.Timebase, false, err)
	}
	if p.Duration < 0 || durUS < 0 {
		return Normalized{}, timeline.TrackError("clock.normalize", p.Track, p.RawTS,
			m.Timebase, false, fmt.Errorf("%w: duration ticks=%d", timeline.ErrNegativeDuration, p.Duration))
	}
	return Normalized{
		Track: p.Track, Seq: p.Seq, StartUS: corrected,
		Duration: durUS, NativeDur: p.Duration,
		Recovered: p.Recovered, Payload: p.Payload,
	}, nil
}

// NormalizeAll maps every non-parity packet and returns them in input order.
func (n *Normalizer) NormalizeAll(packets []fixture.Packet) ([]Normalized, error) {
	out := make([]Normalized, 0, len(packets))
	for _, p := range packets {
		if p.IsParity {
			continue
		}
		np, err := n.Normalize(p)
		if err != nil {
			return nil, err
		}
		out = append(out, np)
	}
	return out, nil
}

// MaxResidual returns the largest absolute drift-corrected position error in
// microseconds against the ground truth, and the track where it occurred.
func (n *Normalizer) MaxResidual(packets []fixture.Packet) (int64, uint32) {
	var maxAbs int64
	var worstTrack uint32
	for _, p := range packets {
		if p.IsParity {
			continue
		}
		np, err := n.Normalize(p)
		if err != nil {
			continue
		}
		diff := np.StartUS - p.TrueUS
		if diff < 0 {
			diff = -diff
		}
		if diff > maxAbs {
			maxAbs, worstTrack = diff, p.Track
		}
	}
	return maxAbs, worstTrack
}

// Model returns the fitted model for a track.
func (n *Normalizer) Model(track uint32) (*TrackModel, bool) {
	m, ok := n.models[track]
	return m, ok
}

// Errors returns structured errors accumulated during estimation.
func (n *Normalizer) Errors() []*timeline.Error { return n.errs }
