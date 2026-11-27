// Package media provides the shared rational time base, 33-bit PTS
// unwrapping and the timeline model used by every subsystem.
package media

import (
	"sort"
)

// Wrap33 is the modulus of the 33-bit MPEG timestamp clock.
const Wrap33 int64 = 1 << 33

// HalfWrap33 is half of the 33-bit clock range. A delta beyond it is
// considered a wrap and a delta exactly on it is ambiguous.
const HalfWrap33 int64 = Wrap33 / 2

// Rational is an exact non-negative or signed time value in seconds.
// Den is always positive and the fraction is kept reduced.
type Rational struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

// NewRational reduces num/den. A zero denominator is coerced to one so
// that values decoded from untrusted JSON can never panic the process.
func NewRational(num, den int64) Rational {
	if den == 0 {
		den = 1
	}
	if den < 0 {
		num, den = -num, -den
	}
	g := gcd(abs(num), den)
	if g > 1 {
		num /= g
		den /= g
	}
	return Rational{Num: num, Den: den}
}

// Zero is the origin of a timeline.
var Zero = Rational{Num: 0, Den: 1}

// Ticks converts a tick count on the given clock scale into a Rational.
func Ticks(ticks, scale int64) Rational {
	return NewRational(ticks, scale)
}

// Add returns r+o.
func (r Rational) Add(o Rational) Rational {
	return NewRational(r.Num*o.Den+o.Num*r.Den, r.Den*o.Den)
}

// Sub returns r-o.
func (r Rational) Sub(o Rational) Rational {
	return NewRational(r.Num*o.Den-o.Num*r.Den, r.Den*o.Den)
}

// Neg returns -r.
func (r Rational) Neg() Rational { return NewRational(-r.Num, r.Den) }

// Cmp returns -1, 0 or 1.
func (r Rational) Cmp(o Rational) int {
	l := r.Num * o.Den
	rr := o.Num * r.Den
	switch {
	case l < rr:
		return -1
	case l > rr:
		return 1
	default:
		return 0
	}
}

// Abs returns |r|.
func (r Rational) Abs() Rational {
	if r.Num < 0 {
		return r.Neg()
	}
	return r
}

// Seconds returns the value as a float64 number of seconds.
func (r Rational) Seconds() float64 { return float64(r.Num) / float64(r.Den) }

// Millis returns the value as a float64 number of milliseconds.
func (r Rational) Millis() float64 { return r.Seconds() * 1000 }

// MillisRounded returns the nearest whole millisecond. Ties go away from zero.
func (r Rational) MillisRounded() int64 {
	return roundDiv(r.Num*1000, r.Den)
}

// Tick returns the value rounded to the nearest tick on scale.
func (r Rational) Tick(scale int64) int64 {
	return roundDiv(r.Num*scale, r.Den)
}

// String renders the reduced fraction.
func (r Rational) String() string {
	if r.Den == 1 {
		return itoa(r.Num)
	}
	return itoa(r.Num) + "/" + itoa(r.Den)
}

// Unwrap33 maps a raw 33-bit timestamp onto the continuous timeline that
// contains prev. It returns ok=false when the jump is exactly half the
// clock range, because that case cannot be resolved uniquely.
func Unwrap33(prev, raw int64) (int64, bool) {
	raw = mod(raw, Wrap33)
	base := prev - mod(prev, Wrap33)
	best := base + raw
	bestDist := abs64(best - prev)
	for _, cand := range []int64{base - Wrap33 + raw, base + Wrap33 + raw} {
		if d := abs64(cand - prev); d < bestDist {
			best, bestDist = cand, d
		}
	}
	if bestDist >= HalfWrap33 {
		return 0, false
	}
	return best, true
}

// MedianOffset returns the median of audio[i]-video[i] for all paired
// samples. It returns false when no pair is available.
func MedianOffset(audio, video []Rational) (Rational, bool) {
	if len(audio) != len(video) || len(audio) == 0 {
		return Zero, false
	}
	diffs := make([]Rational, 0, len(audio))
	for i := range audio {
		diffs = append(diffs, audio[i].Sub(video[i]))
	}
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].Cmp(diffs[j]) < 0 })
	return diffs[len(diffs)/2], true
}

// MaxAbsDiff returns the largest |a-b| among paired values.
func MaxAbsDiff(a, b []Rational) Rational {
	out := Zero
	for i := 0; i < len(a) && i < len(b); i++ {
		if d := a[i].Sub(b[i]).Abs(); d.Cmp(out) > 0 {
			out = d
		}
	}
	return out
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

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func abs64(n int64) int64 { return abs(n) }

func mod(v, m int64) int64 {
	r := v % m
	if r < 0 {
		r += m
	}
	return r
}

func roundDiv(n, d int64) int64 {
	if d < 0 {
		n, d = -n, -d
	}
	if n >= 0 {
		return (n + d/2) / d
	}
	return -((-n + d/2) / d)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [24]byte
	i := len(buf)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
