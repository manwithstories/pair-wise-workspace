// Package clock defines the unified 90 kHz media timebase and the exact
// rational arithmetic used to normalize timestamps from any source
// timescale without accumulating rounding drift.
package clock

// MediaTimebase is the shared timeline resolution in ticks per second.
const MediaTimebase = 90000

// Rational is a reduced fraction num/den with den > 0. All timeline math
// stays in Rational until a single final rounding step.
type Rational struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

// NewRational returns num/den normalized: sign on the numerator, reduced.
func NewRational(num, den int64) Rational {
	if den < 0 {
		num, den = -num, -den
	}
	if num == 0 {
		return Rational{0, 1}
	}
	g := gcd(abs64(num), den)
	return Rational{num / g, den / g}
}

// Add returns r + o exactly.
func (r Rational) Add(o Rational) Rational {
	return NewRational(r.Num*o.Den+o.Num*r.Den, r.Den*o.Den)
}

// Sub returns r - o exactly.
func (r Rational) Sub(o Rational) Rational {
	return NewRational(r.Num*o.Den-o.Num*r.Den, r.Den*o.Den)
}

// Abs returns |r|.
func (r Rational) Abs() Rational {
	if r.Num < 0 {
		return Rational{-r.Num, r.Den}
	}
	return r
}

// ToTimebase converts ts ticks of the given source timescale into 90 kHz
// ticks, kept as an exact rational.
func ToTimebase(ts int64, timescale int64) Rational {
	return NewRational(ts*MediaTimebase, timescale)
}

// Ticks rounds r (in 90 kHz ticks) to the nearest whole tick, halves away
// from zero. This is the only place rounding is allowed to happen.
func (r Rational) Ticks() int64 {
	q := r.Num / r.Den
	rem := r.Num % r.Den
	if abs64(rem)*2 >= r.Den {
		if r.Num >= 0 {
			q++
		} else {
			q--
		}
	}
	return q
}

// Millis returns r (in 90 kHz ticks) as milliseconds for reporting only.
func (r Rational) Millis() float64 {
	return float64(r.Num) / float64(r.Den) / (MediaTimebase / 1000)
}

// TicksToMillis converts whole 90 kHz ticks to milliseconds for reporting.
func TicksToMillis(ticks int64) float64 {
	return float64(ticks) / (MediaTimebase / 1000)
}

// Unwrap extends a wrapping counter of the given bit width (e.g. 33-bit
// MPEG-TS PCR, 32-bit RTP timestamps) into a monotonic int64 by choosing
// the continuation nearest to prev.
func Unwrap(prev int64, raw uint64, bits uint) int64 {
	mod := int64(1) << bits
	half := mod >> 1
	cand := (prev &^ (mod - 1)) | int64(raw&(uint64(mod)-1))
	switch {
	case cand-prev > half:
		cand -= mod
	case prev-cand > half:
		cand += mod
	}
	return cand
}

// Unwrap33 extends a 33-bit wrapping timestamp into a monotonic int64.
func Unwrap33(prev int64, raw uint64) int64 { return Unwrap(prev, raw, 33) }

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
