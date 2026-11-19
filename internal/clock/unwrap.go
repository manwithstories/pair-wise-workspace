// Package clock unwraps extended RTP timestamps across modulus wraps and
// estimates continuous clock drift without floating point arithmetic.
package clock

import (
	"archivearchivist/internal/model"
)

// Options controls unwrapping.
type Options struct {
	// Modulus is the timestamp modulus, normally 1<<33 for RTP.
	Modulus int64
	// ClockRate is the timestamp clock rate in Hz.
	ClockRate uint32
	// MaxDriftPPM is the review threshold for estimated drift.
	MaxDriftPPM int64
	// WrapGuard is the minimum backwards jump that counts as a wrap. It
	// defaults to Modulus/2 so ordinary reordering is never treated as a wrap.
	WrapGuard int64
}

// Defaults fills unset fields.
func (o Options) Defaults() Options {
	if o.Modulus <= 1 {
		o.Modulus = model.Modulus33
	}
	if o.ClockRate == 0 {
		o.ClockRate = 90000
	}
	if o.MaxDriftPPM <= 0 {
		o.MaxDriftPPM = 80
	}
	if o.WrapGuard <= 0 {
		o.WrapGuard = o.Modulus / 2
	}
	return o
}

// Sample is one raw timestamp read from a packet or sample table entry. Orig
// is int64 because a 33-bit transport clock (MPEG-TS PCR) cannot be stored in
// 32 bits; RTP's 32-bit timestamps are widened without change.
type Sample struct {
	Seq     uint32
	Orig    int64
	Arrival int64 // microseconds; used only when HasArrival
	HasArr  bool
}

// Point is the normalized view of one sample.
type Point struct {
	Seq        uint32 `json:"seq"`
	Orig       int64  `json:"orig_timestamp"`
	Epoch      int64  `json:"wrap_epoch"`
	Norm       int64  `json:"normalized"`
	Wrapped    bool   `json:"wrapped"`
	Regressed  bool   `json:"regressed"`
	ArrivalUS  int64  `json:"arrival_us,omitempty"`
	HasArrival bool   `json:"-"`
	SkewUS     int64  `json:"skew_us,omitempty"`
}

// Result is the outcome of unwrapping one track's timestamps.
type Result struct {
	Points        []Point
	Wraps         int
	Regressions   int
	DriftPPM      model.Rational
	DriftExceeded bool
	MaxSkewUS     int64
	Faults        model.FaultList
}

// Unwrap normalizes samples in presentation order. The wrap detector keeps a
// high-water mark of normalized time: a backward hop greater than the guard
// from that mark advances the epoch, while a forward hop greater than the guard
// is a late packet from the previous epoch and is normalized backwards without
// disturbing the epoch. This keeps one wrap from being counted twice.
func Unwrap(samples []Sample, opts Options) Result {
	opts = opts.Defaults()
	res := Result{DriftPPM: model.NewRat(0, 1)}
	if len(samples) == 0 {
		return res
	}
	res.Points = make([]Point, 0, len(samples))

	// highNorm/highOrig describe the furthest-along sample seen so far.
	highNorm := samples[0].Orig
	highOrig := samples[0].Orig
	prevNorm := highNorm
	// Timestamps slightly behind the previous one are reordering, not a
	// regression worth stopping for. Anything beyond a second of clock ticks
	// indicates a real discontinuity.
	tolerance := int64(opts.ClockRate)
	for i, s := range samples {
		cur := s.Orig
		epochOff := highNorm - highOrig
		wrapped := false
		switch {
		case i > 0 && highOrig-cur > opts.WrapGuard:
			epochOff += opts.Modulus
			wrapped = true
			res.Wraps++
		case i > 0 && cur-highOrig > opts.WrapGuard:
			epochOff -= opts.Modulus
		}
		norm := epochOff + cur
		epoch := epochsOf(norm, cur, opts.Modulus)
		regressed := i > 0 && norm < prevNorm-tolerance
		if regressed {
			res.Regressions++
			res.Faults.Add(model.Fault{
				Code:      model.CodeTimestampRegress,
				Severity:  model.SevWarn,
				PacketSeq: s.Seq,
				TimeOrig:  itoa(cur),
				TimeNorm:  itoa(norm),
				Detail:    "normalized timestamp moved backwards beyond tolerance",
			})
		}
		p := Point{Seq: s.Seq, Orig: cur, Epoch: epoch, Norm: norm, Wrapped: wrapped}
		if s.HasArr {
			p.ArrivalUS = s.Arrival
			p.HasArrival = true
		}
		res.Points = append(res.Points, p)
		if norm > highNorm {
			highNorm = norm
			highOrig = cur
		}
		prevNorm = norm
	}

	res.DriftPPM = estimateDrift(res.Points, opts.ClockRate)
	res.MaxSkewUS = measureSkew(res.Points, opts.ClockRate, res.DriftPPM)
	if abs64(res.DriftPPM.Num) > opts.MaxDriftPPM*res.DriftPPM.Den {
		res.DriftExceeded = true
		res.Faults.Add(model.Fault{
			Code:     model.CodeClockDrift,
			Severity: model.SevWarn,
			Value:    res.DriftPPM.Num / res.DriftPPM.Den,
			Detail:   "estimated clock drift " + res.DriftPPM.String() + " ppm exceeds review threshold",
		})
	}
	if res.Wraps > 0 {
		res.Faults.Add(model.Fault{
			Code:     model.CodeTimestampWrap,
			Severity: model.SevInfo,
			Value:    int64(res.Wraps),
			Detail:   "unwrapped timestamp modulus boundary",
		})
	}
	res.Faults.Sort()
	return res
}

// epochsOf returns the modulus epoch that norm belongs to.
func epochsOf(norm, cur, mod int64) int64 {
	off := norm - cur
	if off >= 0 {
		return off / mod
	}
	return -((-off + mod - 1) / mod)
}
