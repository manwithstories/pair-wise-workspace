// Package clock provides the unified timebase for the pipeline: exact
// rational time arithmetic (no floating point accumulation), extension
// of wrapping timestamps to a monotonic 64-bit domain, and per-track
// clock drift fitting against anchor pairs.
package clock

import (
	"math/big"
)

// Rational is an exact rational number with a positive denominator.
type Rational struct {
	Num int64
	Den int64
}

// NewRational returns num/den normalised so the denominator is
// positive. It panics on a zero denominator.
func NewRational(num, den int64) Rational {
	if den == 0 {
		panic("clock: zero denominator")
	}
	if den < 0 {
		num, den = -num, -den
	}
	if g := gcd(abs64(num), den); g > 1 {
		num, den = num/g, den/g
	}
	return Rational{Num: num, Den: den}
}

// Rat converts r to a *big.Rat.
func (r Rational) Rat() *big.Rat { return big.NewRat(r.Num, r.Den) }

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	return a
}

// Unwrapper extends a wrapping unsigned counter (e.g. a 32-bit RTP
// timestamp or a 16-bit RTP sequence number) into a monotonic int64
// sequence. Forward wraparound is unwrapped; backward jumps smaller
// than half the range are treated as reordering and extend backwards.
type Unwrapper struct {
	bits    uint
	mod     uint64
	mask    uint64
	inited  bool
	lastRaw uint64
	lastExt int64
}

// NewUnwrapper creates an Unwrapper for a counter of the given bit
// width (2..63).
func NewUnwrapper(bits uint) *Unwrapper {
	if bits < 2 || bits > 63 {
		panic("clock: unwrap bits out of range")
	}
	mod := uint64(1) << bits
	return &Unwrapper{bits: bits, mod: mod, mask: mod - 1}
}

// Extend feeds one raw counter value and returns its extended value.
func (u *Unwrapper) Extend(v uint64) int64 {
	v &= u.mask
	if !u.inited {
		u.inited = true
		u.lastRaw = v
		u.lastExt = int64(v)
		return u.lastExt
	}
	d := (v - u.lastRaw) & u.mask
	if d >= u.mod/2 {
		u.lastExt -= int64(u.mod - d)
	} else {
		u.lastExt += int64(d)
	}
	u.lastRaw = v
	return u.lastExt
}

// Anchor pairs an unwrapped local timestamp with the reference
// timeline position (nanoseconds) observed for it.
type Anchor struct {
	Local int64 `json:"local"`
	Ref   int64 `json:"ref"`
}

// Fit is the result of a drift fit. Slope and Intercept map local
// ticks to reference nanoseconds: ref = local*Slope + Intercept.
// Aligned is false when the track could not be aligned (too few
// anchors or drift above the configured threshold); Reason says why.
type Fit struct {
	Slope     *big.Rat
	Intercept *big.Rat
	Anchors   int
	DriftPPM  *big.Rat
	Aligned   bool
	Reason    string
}

// Fitter configures drift fitting. Nominal is the track's nominal
// timebase in seconds per tick.
type Fitter struct {
	Nominal     Rational
	MinAnchors  int
	MaxDriftPPM int64
}

// Fit performs a least-squares fit of ref = slope*local + intercept
// over the anchors using exact rational arithmetic, then decides
// whether the track is alignable.
func (f Fitter) Fit(anchors []Anchor) Fit {
	nominalNs := new(big.Rat).Mul(f.Nominal.Rat(), big.NewRat(1_000_000_000, 1))
	if len(anchors) < f.MinAnchors {
		return Fit{
			Slope:     nominalNs,
			Intercept: new(big.Rat),
			Anchors:   len(anchors),
			DriftPPM:  new(big.Rat),
			Aligned:   false,
			Reason:    "insufficient anchors",
		}
	}
	n := big.NewRat(int64(len(anchors)), 1)
	meanL, meanR := new(big.Rat), new(big.Rat)
	for _, a := range anchors {
		meanL.Add(meanL, new(big.Rat).SetInt64(a.Local))
		meanR.Add(meanR, new(big.Rat).SetInt64(a.Ref))
	}
	meanL.Quo(meanL, n)
	meanR.Quo(meanR, n)
	sxy, sxx := new(big.Rat), new(big.Rat)
	tmp := new(big.Rat)
	for _, a := range anchors {
		dl := tmp.Sub(new(big.Rat).SetInt64(a.Local), meanL)
		dr := new(big.Rat).Sub(new(big.Rat).SetInt64(a.Ref), meanR)
		sxy.Add(sxy, new(big.Rat).Mul(dl, dr))
		sxx.Add(sxx, new(big.Rat).Mul(dl, dl))
	}
	if sxx.Sign() == 0 {
		return Fit{
			Slope: nominalNs, Intercept: new(big.Rat),
			Anchors: len(anchors), DriftPPM: new(big.Rat),
			Aligned: false, Reason: "degenerate anchors",
		}
	}
	slope := new(big.Rat).Quo(sxy, sxx)
	intercept := new(big.Rat).Sub(meanR, new(big.Rat).Mul(slope, meanL))
	// drift = |slope/nominal - 1| * 1e6, exact rational comparison.
	drift := new(big.Rat).Quo(new(big.Rat).Set(slope), nominalNs)
	drift.Sub(drift, big.NewRat(1, 1))
	drift.Abs(drift)
	drift.Mul(drift, big.NewRat(1_000_000, 1))
	fit := Fit{
		Slope: slope, Intercept: intercept,
		Anchors: len(anchors), DriftPPM: drift,
		Aligned: true,
	}
	if drift.Cmp(big.NewRat(f.MaxDriftPPM, 1)) > 0 {
		fit.Aligned = false
		fit.Reason = "drift exceeds threshold"
	}
	return fit
}

// MapTime converts an unwrapped local timestamp to reference
// nanoseconds using the fit, rounding half away from zero. All
// arithmetic is exact; no floating point is involved.
func MapTime(fit Fit, local int64) int64 {
	r := new(big.Rat).Mul(new(big.Rat).SetInt64(local), fit.Slope)
	r.Add(r, fit.Intercept)
	return RoundRat(r)
}

// RoundRat rounds a rational to the nearest integer, halves away
// from zero.
func RoundRat(r *big.Rat) int64 {
	num, den := r.Num(), r.Denom()
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	twice := new(big.Int).Lsh(new(big.Int).Abs(rem), 1)
	if twice.Cmp(den) >= 0 {
		if r.Sign() >= 0 {
			q.Add(q, big.NewInt(1))
		} else {
			q.Sub(q, big.NewInt(1))
		}
	}
	return q.Int64()
}
