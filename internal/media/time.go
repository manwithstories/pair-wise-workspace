// Package media defines the rational time model shared by parsers, analyzers,
// pipelines, and reports.
package media

import (
	"math/big"
)

// UnifiedTimescale is the lossless-reporting time base: 1 tick is 1 ns.
const UnifiedTimescale uint32 = 1_000_000_000

// Rational is an exact rational number. Den == 0 is reported as invalid rather
// than panicking.
type Rational struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

// Valid reports whether the denominator is non-zero.
func (r Rational) Valid() bool { return r.Den != 0 }

// Float64 is intended only for human output. Canonical reports use Num/Den.
func (r Rational) Float64() float64 {
	if r.Den == 0 {
		return 0
	}
	return float64(r.Num) / float64(r.Den)
}

// Time is a tick count in an explicit MP4/MOV timescale. A zero timescale is
// invalid in a well-formed mdhd but is retained so callers can report it.
type Time struct {
	Ticks     int64  `json:"ticks"`
	Timescale uint32 `json:"timescale"`
}

// NewTime constructs a media time.
func NewTime(ticks int64, timescale uint32) Time {
	return Time{Ticks: ticks, Timescale: timescale}
}

// ValidTimescale reports whether scaling is possible.
func (t Time) ValidTimescale() bool { return t.Timescale != 0 }

// Rational returns ticks/timescale in seconds.
func (t Time) Rational() Rational {
	return Rational{Num: t.Ticks, Den: int64(t.Timescale)}
}

// ScaleTicks converts ticks between timescales using round-to-nearest and exact
// big integer arithmetic. The second result is false for a zero source or
// destination timescale.
func ScaleTicks(ticks int64, from, to uint32) (int64, bool) {
	if from == 0 || to == 0 {
		return 0, false
	}
	if from == to || ticks == 0 {
		return ticks, true
	}

	n := new(big.Int).Mul(big.NewInt(ticks), big.NewInt(int64(to)))
	d := big.NewInt(int64(from))
	q, rem := new(big.Int).QuoRem(n, d, new(big.Int))

	// Round the magnitude to nearest; ties round away from zero.
	twiceRem := new(big.Int).Abs(rem)
	twiceRem.Mul(twiceRem, big.NewInt(2))
	den := new(big.Int).Abs(d)
	if twiceRem.Cmp(den) >= 0 {
		if q.Sign() >= 0 {
			q.Add(q, big.NewInt(1))
		} else {
			q.Sub(q, big.NewInt(1))
		}
	}
	if !q.IsInt64() {
		return 0, false
	}
	return q.Int64(), true
}

// Unified converts to the common nanosecond time base.
func (t Time) Unified() (int64, bool) {
	return ScaleTicks(t.Ticks, t.Timescale, UnifiedTimescale)
}

// Interval is a half-open [Start, End) duration in one timescale.
type Interval struct {
	Start     Time   `json:"start"`
	End       Time   `json:"end"`
	Ticks     int64  `json:"ticks"`
	Timescale uint32 `json:"timescale"`
}

// NewInterval constructs an interval and derives its positive tick duration.
func NewInterval(start, end Time) Interval {
	delta := end.Ticks - start.Ticks
	if end.Ticks < start.Ticks {
		delta = 0
	}
	return Interval{Start: start, End: end, Ticks: delta, Timescale: start.Timescale}
}
