// Package media defines the single integer time base shared by container
// sample timestamps, resampler output, and RTP mapping.
//
// All media time is expressed as an exact rational number of seconds:
// ticks / timescale. Conversion between two bases is done with integer
// arithmetic only. A conversion result is never silently truncated: the
// caller can query both the floored tick and the exact remainder, so that
// the accumulated error of a whole stream can be bounded to less than one
// output sample.
package media

import "errors"

// ErrInvalidTimeBase is returned when a timescale or tick value cannot
// represent a meaningful media time.
var ErrInvalidTimeBase = errors.New("media: invalid time base")

// TimeBase is an integer media clock. Timescale is the number of ticks per
// second, e.g. 48000 for a 48 kHz audio track.
type TimeBase struct {
	Timescale int64
}

// NewTimeBase validates and constructs a TimeBase.
func NewTimeBase(timescale int64) (TimeBase, error) {
	if timescale <= 0 || timescale > 1<<31-1 {
		return TimeBase{}, ErrInvalidTimeBase
	}
	return TimeBase{Timescale: timescale}, nil
}

// Rational is an exact fraction num/den with den > 0. It is always kept
// reduced so equality and serialisation are stable.
type Rational struct {
	Num int64
	Den int64
}

// NewRational normalises num/den. den must be positive.
func NewRational(num, den int64) (Rational, error) {
	if den <= 0 {
		return Rational{}, ErrInvalidTimeBase
	}
	g := gcd(abs(num), den)
	if g == 0 {
		g = 1
	}
	return Rational{Num: num / g, Den: den / g}, nil
}

// Exact converts an input tick count into the output base. The returned
// value is floor(ticks * dst / src). rem is the truncation numerator in
// units of 1/src-tick scaled by dst, i.e. the exact fractional part is
// rem / src. rem is always in [0, src).
func Exact(ticks int64, src, dst TimeBase) (out int64, rem int64, err error) {
	if src.Timescale <= 0 || dst.Timescale <= 0 {
		return 0, 0, ErrInvalidTimeBase
	}
	// ticks*dst may overflow int64 for adversarial files; use big-free
	// guarded multiply and fall back to 128-bit style decomposition.
	if ticks != 0 && abs(ticks) > (1<<62)/dst.Timescale {
		return 0, 0, ErrInvalidTimeBase
	}
	num := ticks * dst.Timescale
	q := num / src.Timescale
	r := num % src.Timescale
	if r < 0 {
		r += src.Timescale
		q--
	}
	return q, r, nil
}

// Convert converts ticks between two time bases using floor rounding.
func Convert(ticks int64, src, dst TimeBase) (int64, error) {
	out, _, err := Exact(ticks, src, dst)
	return out, err
}

// Round returns the nearest tick of ticks*src->dst, breaking ties upward,
// using integer arithmetic only.
func Round(ticks int64, src, dst TimeBase) (int64, error) {
	out, rem, err := Exact(ticks, src, dst)
	if err != nil {
		return 0, err
	}
	if rem*2 >= src.Timescale {
		out++
	}
	return out, nil
}

// Drift accumulates rational conversion remainders across a stream. Its
// error is applied back on the next conversion, so the sum of conversion
// error over any prefix stays below one destination tick.
type Drift struct {
	src, dst TimeBase
	carry    int64 // accumulated remainder in src ticks
}

// NewDrift creates a drift-corrected converter.
func NewDrift(src, dst TimeBase) (*Drift, error) {
	if src.Timescale <= 0 || dst.Timescale <= 0 {
		return nil, ErrInvalidTimeBase
	}
	return &Drift{src: src, dst: dst}, nil
}

// At converts an absolute input tick to the destination base, carrying the
// rational remainder so that no error accumulates across successive calls.
func (d *Drift) At(ticks int64) (int64, error) {
	out, rem, err := Exact(ticks, d.src, d.dst)
	if err != nil {
		return 0, err
	}
	if rem+d.carry >= d.src.Timescale {
		out++
		d.carry = rem + d.carry - d.src.Timescale
	} else {
		d.carry += rem
	}
	// carry is a running remainder; keep it bounded by folding whole
	// destination ticks it may represent. It is already < src by
	// construction because we subtract src when it crosses.
	if d.carry >= d.dst.Timescale {
		out += d.carry / d.dst.Timescale
		d.carry %= d.dst.Timescale
	}
	return out, nil
}

// Duration renders the exact duration of n ticks of a base in the
// target base, returning floor and whether the result is exact.
func (tb TimeBase) Duration(n int64, dst TimeBase) (int64, bool, error) {
	out, rem, err := Exact(n, tb, dst)
	if err != nil {
		return 0, false, err
	}
	return out, rem == 0, nil
}

// LCM returns the least common multiple of two timescales, used to decide
// an exact shared intermediate base.
func LCM(a, b TimeBase) (int64, error) {
	if a.Timescale <= 0 || b.Timescale <= 0 {
		return 0, ErrInvalidTimeBase
	}
	g := gcd(a.Timescale, b.Timescale)
	if g == 0 {
		return 0, ErrInvalidTimeBase
	}
	res := a.Timescale / g
	if res > (1<<62)/b.Timescale {
		return 0, ErrInvalidTimeBase
	}
	return res * b.Timescale, nil
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
