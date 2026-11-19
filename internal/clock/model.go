// Package clock provides the shared rational time basis used by every
// stage of the pipeline: subtitle parsing, MP4 indexing, timeline
// composition, validation and frame export.
//
// All media times are represented as exact rationals of seconds. No
// timestamp is accumulated in floating point.
package clock

import (
	"fmt"
	"math/big"
	"strings"
)

// Rational is an exact fraction of seconds. The zero value is 0/1.
type Rational struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

// Zero is the zero instant.
func Zero() Rational { return Rational{Num: 0, Den: 1} }

// NewRational normalises a fraction. A zero or negative denominator is
// rejected by negating both parts; a zero denominator falls back to 1 so
// that malformed input can never trigger a division panic.
func NewRational(num, den int64) Rational {
	if den == 0 {
		den = 1
	}
	if den < 0 {
		num, den = -num, -den
	}
	if num == 0 {
		return Rational{Num: 0, Den: 1}
	}
	g := gcd(abs(num), den)
	return Rational{Num: num / g, Den: den / g}
}

// FromMillis builds a rational second value from integer milliseconds.
func FromMillis(ms int64) Rational { return NewRational(ms, 1000) }

// FromMicros builds a rational second value from integer microseconds.
func FromMicros(us int64) Rational { return NewRational(us, 1_000_000) }

// FromNanos builds a rational second value from integer nanoseconds.
func FromNanos(ns int64) Rational { return NewRational(ns, 1_000_000_000) }

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	return a
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func (r Rational) big() *big.Rat {
	den := r.Den
	if den == 0 {
		den = 1
	}
	return new(big.Rat).SetFrac64(r.Num, den)
}

// fromBig converts an exact rational into the int64 pair used by
// Rational. Values that do not fit are represented with a fixed
// picosecond grid (and a milli-scale fallback) instead of being
// silently truncated to an integer.
func fromBig(x *big.Rat) Rational {
	num := x.Num()
	den := x.Denom()
	if num.IsInt64() && den.IsInt64() {
		return NewRational(num.Int64(), den.Int64())
	}
	if r, ok := scaleRat(x, 1_000_000_000_000); ok {
		return r
	}
	if r, ok := scaleRat(x, 1_000_000_000); ok {
		return r
	}
	return Zero()
}

// scaleRat rounds x*scale to the nearest integer and divides by scale.
func scaleRat(x *big.Rat, scale int64) (Rational, bool) {
	scaled := new(big.Rat).Mul(x, new(big.Rat).SetInt64(scale))
	n := roundRat(scaled)
	if !n.IsInt64() {
		return Zero(), false
	}
	return NewRational(n.Int64(), scale), true
}

// roundRat rounds half away from zero.
func roundRat(x *big.Rat) *big.Int {
	n, d := x.Num(), x.Denom()
	q, r := new(big.Int).QuoRem(n, d, new(big.Int))
	twice := new(big.Int).Lsh(new(big.Int).Abs(r), 1)
	if twice.Cmp(d) >= 0 {
		if n.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	return q
}

// Add returns r + o.
func (r Rational) Add(o Rational) Rational { return fromBig(new(big.Rat).Add(r.big(), o.big())) }

// Sub returns r - o.
func (r Rational) Sub(o Rational) Rational { return fromBig(new(big.Rat).Sub(r.big(), o.big())) }

// Neg returns -r.
func (r Rational) Neg() Rational { return Rational{Num: -r.Num, Den: r.Den} }

// Mul returns r * o.
func (r Rational) Mul(o Rational) Rational { return fromBig(new(big.Rat).Mul(r.big(), o.big())) }

// Div returns r / o. Division by zero yields zero rather than a panic;
// callers validate rates and deltas before dividing.
func (r Rational) Div(o Rational) Rational {
	if o.Num == 0 {
		return Zero()
	}
	return fromBig(new(big.Rat).Quo(r.big(), o.big()))
}

// Cmp compares r and o and returns -1, 0 or +1.
func (r Rational) Cmp(o Rational) int { return r.big().Cmp(o.big()) }

// LT reports r < o.
func (r Rational) LT(o Rational) bool { return r.Cmp(o) < 0 }

// LTE reports r <= o.
func (r Rational) LTE(o Rational) bool { return r.Cmp(o) <= 0 }

// IsZero reports r == 0.
func (r Rational) IsZero() bool { return r.Num == 0 }

// IsPositive reports r > 0.
func (r Rational) IsPositive() bool {
	if r.Den == 0 {
		return false
	}
	return (r.Num > 0 && r.Den > 0) || (r.Num < 0 && r.Den < 0)
}

// Seconds returns the value as a float64 for presentation and for
// locating pixels. It must not be used to accumulate timestamps.
func (r Rational) Seconds() float64 {
	if r.Den == 0 {
		return 0
	}
	return float64(r.Num) / float64(r.Den)
}

// Millis returns the nearest integer millisecond, rounding half away
// from zero.
func (r Rational) Millis() int64 {
	if r.Den == 0 {
		return 0
	}
	n := new(big.Int).Mul(big.NewInt(r.Num), big.NewInt(1000))
	d := big.NewInt(r.Den)
	q, m := new(big.Int).QuoRem(n, d, new(big.Int))
	m.Abs(m)
	m.Lsh(m, 1)
	if m.Cmp(d) >= 0 {
		if r.Num >= 0 {
			q.Add(q, big.NewInt(1))
		} else {
			q.Sub(q, big.NewInt(1))
		}
	}
	if !q.IsInt64() {
		return 0
	}
	return q.Int64()
}

// String renders the exact fraction, e.g. "1001/30000".
func (r Rational) String() string {
	if r.Den == 0 {
		return "0/1"
	}
	return fmt.Sprintf("%d/%d", r.Num, r.Den)
}

// MarshalJSON emits an exact fraction object {"num":..,"den":..}.
func (r Rational) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf(`{"num":%d,"den":%d}`, r.Num, r.Den)), nil
}

// Base maps integer ticks to rational seconds. It is the single time
// basis shared by RTP (90000 Hz), MP4 (per-track timescale) and
// subtitle (1000 Hz) inputs.
type Base struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

// NewBase defines one tick as Num/Den seconds.
func NewBase(num, den int64) Base {
	if num == 0 {
		num = 1
	}
	if den == 0 {
		den = 1
	}
	r := NewRational(num, den)
	return Base{Num: r.Num, Den: r.Den}
}

// Time converts ticks on this base to seconds.
func (b Base) Time(ticks int64) Rational {
	return NewRational(ticks*b.Num, b.Den)
}

// Ticks converts seconds to ticks on this base, truncating toward zero.
func (b Base) Ticks(r Rational) int64 {
	if r.Den == 0 || b.Num == 0 {
		return 0
	}
	n := new(big.Int).Mul(big.NewInt(r.Num), big.NewInt(b.Den))
	d := new(big.Int).Mul(big.NewInt(r.Den), big.NewInt(b.Num))
	n.Quo(n, d)
	if !n.IsInt64() {
		return 0
	}
	return n.Int64()
}

// String renders the base as "num/den".
func (b Base) String() string { return fmt.Sprintf("%d/%d", b.Num, b.Den) }

// SourceError is the error structure shared by all modules. Every
// record carries the raw byte offset (or -1) of the input that caused
// it, and keeps the source value so a reviewer can audit what was not
// silently repaired.
type SourceError struct {
	Code    string `json:"code"`
	Source  string `json:"source"`
	Offset  int64  `json:"offset"`
	Message string `json:"message"`
	Raw     string `json:"raw,omitempty"`
	Err     error  `json:"-"`
}

// NewSourceError creates an offset-carrying error. Pass -1 when the
// error is not tied to a byte position.
func NewSourceError(source, code string, offset int64, message string) *SourceError {
	return &SourceError{Code: code, Source: source, Offset: offset, Message: message}
}

// Errorf is a formatted constructor for SourceError.
func Errorf(source, code string, offset int64, format string, args ...interface{}) *SourceError {
	return NewSourceError(source, code, offset, fmt.Sprintf(format, args...))
}

func (e *SourceError) Error() string {
	var b strings.Builder
	b.WriteString(e.Code)
	if e.Source != "" {
		b.WriteString(" [")
		b.WriteString(e.Source)
		b.WriteString("]")
	}
	if e.Offset >= 0 {
		fmt.Fprintf(&b, " offset=%d", e.Offset)
	} else {
		b.WriteString(" offset=unknown")
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap exposes the wrapped cause for errors.Is/As.
func (e *SourceError) Unwrap() error { return e.Err }

// AsSourceError finds the first SourceError in an error chain.
func AsSourceError(err error) (*SourceError, bool) {
	for err != nil {
		if se, ok := err.(*SourceError); ok {
			return se, true
		}
		switch x := err.(type) {
		case interface{ Unwrap() []error }:
			for _, e := range x.Unwrap() {
				if se, ok := AsSourceError(e); ok {
					return se, true
				}
			}
			return nil, false
		case interface{ Unwrap() error }:
			err = x.Unwrap()
		default:
			return nil, false
		}
	}
	return nil, false
}

// Pair links a true media tick (from MP4 decode time) to the node's
// observed tick (unwrapped RTP timestamp).
type Pair struct {
	SourceTicks int64
	NodeTicks   int64
}

// Correction describes the fitted linear relation
//
//	nodeSeconds = Offset + Slope*sourceSeconds
//
// and provides the inverse used to normalise node timestamps.
type Correction struct {
	Offset   Rational `json:"offset_seconds"`
	Slope    Rational `json:"slope"`
	DriftPPM Rational `json:"drift_ppm"`
	Samples  int      `json:"samples"`
}

// Correct converts a node tick value to corrected source seconds.
func (c Correction) Correct(nodeTicks int64, base Base) Rational {
	nodeSec := base.Time(nodeTicks)
	if c.Slope.IsZero() {
		return nodeSec.Sub(c.Offset)
	}
	return nodeSec.Sub(c.Offset).Div(c.Slope)
}

// Predict returns the predicted node seconds for a source tick value.
func (c Correction) Predict(sourceTicks int64, base Base) Rational {
	return c.Offset.Add(c.Slope.Mul(base.Time(sourceTicks)))
}

// Anomaly is a sample whose residual against the fitted line exceeds
// the configured threshold. The raw node timestamp is preserved.
type Anomaly struct {
	Index       int      `json:"index"`
	SourceTicks int64    `json:"source_ticks"`
	NodeTicks   int64    `json:"node_ticks"`
	ResidualMS  Rational `json:"residual_ms"`
}

// Estimate fits offset and linear drift from source/node pairs using
// exact big.Rat least squares. threshold is the maximum absolute
// residual, in seconds, considered healthy. When fewer than two pairs
// are available a zero-drift correction anchored on the first pair is
// returned.
func Estimate(pairs []Pair, base Base, threshold Rational) (Correction, []Anomaly, bool) {
	if len(pairs) == 0 {
		return Correction{Offset: Zero(), Slope: NewRational(1, 1)}, nil, false
	}
	if len(pairs) == 1 {
		off := base.Time(pairs[0].NodeTicks).Sub(base.Time(pairs[0].SourceTicks))
		c := Correction{Offset: off, Slope: NewRational(1, 1), DriftPPM: Zero(), Samples: 1}
		return c, nil, true
	}

	n := int64(len(pairs))
	sx, sy := new(big.Int), new(big.Int)
	sxy, sxx := new(big.Int), new(big.Int)
	for _, p := range pairs {
		x, y := big.NewInt(p.SourceTicks), big.NewInt(p.NodeTicks)
		sx.Add(sx, x)
		sy.Add(sy, y)
		sxy.Add(sxy, new(big.Int).Mul(x, y))
		sxx.Add(sxx, new(big.Int).Mul(x, x))
	}
	nn := big.NewInt(n)
	den := new(big.Int).Sub(new(big.Int).Mul(nn, sxx), new(big.Int).Mul(sx, sx))
	if den.Sign() == 0 {
		// All source ticks identical: no slope can be estimated.
		off := base.Time(pairs[0].NodeTicks).Sub(base.Time(pairs[0].SourceTicks))
		return Correction{Offset: off, Slope: NewRational(1, 1), Samples: len(pairs)}, nil, true
	}
	num := new(big.Int).Sub(new(big.Int).Mul(nn, sxy), new(big.Int).Mul(sx, sy))
	slope := new(big.Rat).SetFrac(num, den)
	intercept := new(big.Rat).Sub(new(big.Rat).SetFrac(sy, nn),
		new(big.Rat).Mul(slope, new(big.Rat).SetFrac(sx, nn)))

	slopeRat := fromBig(slope)
	// intercept is in node ticks; rotate it to seconds with the base
	// scale factor Num/Den.
	offsetBig := new(big.Rat).SetFrac(
		new(big.Int).Mul(big.NewInt(base.Num), intercept.Num()),
		new(big.Int).Mul(big.NewInt(base.Den), intercept.Denom()))
	offsetSec := fromBig(offsetBig)
	driftPPM := fromBig(new(big.Rat).Mul(
		new(big.Rat).Sub(slope, big.NewRat(1, 1)), new(big.Rat).SetInt64(1_000_000)))

	c := Correction{Offset: offsetSec, Slope: slopeRat, DriftPPM: driftPPM, Samples: len(pairs)}
	var anomalies []Anomaly
	for i, p := range pairs {
		predicted := new(big.Rat).Add(intercept, new(big.Rat).Mul(slope, new(big.Rat).SetInt64(p.SourceTicks)))
		observed := new(big.Rat).SetInt64(p.NodeTicks)
		residualTicks := new(big.Rat).Sub(observed, predicted)
		residualSec := new(big.Rat).Quo(residualTicks, new(big.Rat).SetFrac64(base.Den, base.Num))
		res := fromBig(residualSec)
		if res.Cmp(threshold) > 0 || res.Neg().Cmp(threshold) > 0 {
			anomalies = append(anomalies, Anomaly{
				Index:       i,
				SourceTicks: p.SourceTicks,
				NodeTicks:   p.NodeTicks,
				ResidualMS:  res.Mul(NewRational(1000, 1)),
			})
		}
	}
	return c, anomalies, true
}
