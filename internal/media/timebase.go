package media

import (
	"fmt"
	"strconv"
)

// Rat is an exact rational number. All media timestamps in this project are
// accumulated with Rat values; floating point is never used for time math.
type Rat struct {
	Num int64
	Den int64
}

// NewRat returns a normalized rational. The denominator must be non-zero.
func NewRat(num, den int64) Rat {
	if den == 0 {
		panic("media: rational denominator is zero")
	}
	if den < 0 {
		num, den = -num, -den
	}
	g := gcd(absInt64(num), den)
	if g > 1 {
		num /= g
		den /= g
	}
	if num == 0 {
		den = 1
	}
	return Rat{Num: num, Den: den}
}

// RatFromInt returns n/1.
func RatFromInt(n int64) Rat { return Rat{Num: n, Den: 1} }

// RatFromTicks converts a tick count at the given timescale into seconds.
func RatFromTicks(ticks int64, timescale uint32) Rat {
	if timescale == 0 {
		panic("media: zero timescale")
	}
	return NewRat(ticks, int64(timescale))
}

// RatToTicks converts a rational number of seconds into ticks at timescale,
// rounding half away from zero.
func RatToTicks(r Rat, timescale uint32) int64 {
	if timescale == 0 {
		panic("media: zero timescale")
	}
	return divRound(r.Num*int64(timescale), r.Den)
}

func (r Rat) Add(o Rat) Rat {
	return NewRat(r.Num*o.Den+o.Num*r.Den, r.Den*o.Den)
}

func (r Rat) Sub(o Rat) Rat {
	return NewRat(r.Num*o.Den-o.Num*r.Den, r.Den*o.Den)
}

func (r Rat) Mul(o Rat) Rat {
	return NewRat(r.Num*o.Num, r.Den*o.Den)
}

// Cmp reports -1, 0 or 1 as r is less than, equal to, or greater than o.
func (r Rat) Cmp(o Rat) int {
	a := r.Num * o.Den
	b := o.Num * r.Den
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// Sign reports -1, 0 or 1 according to the sign of r.
func (r Rat) Sign() int {
	switch {
	case r.Num < 0:
		return -1
	case r.Num > 0:
		return 1
	default:
		return 0
	}
}

// IsZero reports whether r is exactly zero.
func (r Rat) IsZero() bool { return r.Num == 0 }

// Float64 is provided for display and reporting only. It must not be used to
// accumulate timestamps.
func (r Rat) Float64() float64 {
	return float64(r.Num) / float64(r.Den)
}

func (r Rat) String() string {
	if r.Den == 1 {
		return strconv.FormatInt(r.Num, 10)
	}
	return strconv.FormatInt(r.Num, 10) + "/" + strconv.FormatInt(r.Den, 10)
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func divRound(n, d int64) int64 {
	if d < 0 {
		n, d = -n, -d
	}
	q, rem := n/d, n%d
	if rem == 0 {
		return q
	}
	twice := absInt64(rem) * 2
	if twice >= d {
		if n < 0 {
			return q - 1
		}
		return q + 1
	}
	return q
}

// Unwrapper expands a wrapping unsigned timestamp (typically 32 or 33 bits)
// into a monotonically increasing tick sequence for one track.
type Unwrapper struct {
	modulus   uint64
	half      int64
	lastRaw   uint64
	epoch     int64
	unwrapped int64
	started   bool
}

// NewUnwrapper creates an unwrapper for timestamps of the given bit width.
func NewUnwrapper(bits uint) *Unwrapper {
	if bits == 0 || bits > 63 {
		panic(fmt.Sprintf("media: unsupported timestamp width %d", bits))
	}
	modulus := uint64(1) << bits
	return &Unwrapper{modulus: modulus, half: int64(modulus >> 1)}
}

// Unwrap expands raw using per-track continuity. inputOffset is the byte
// offset of the timestamp in the input; it is reported verbatim when the
// transition is ambiguous.
func (u *Unwrapper) Unwrap(raw uint64, inputOffset uint64) (int64, error) {
	raw %= u.modulus
	if !u.started {
		u.started = true
		u.lastRaw = raw
		u.unwrapped = int64(raw)
		return u.unwrapped, nil
	}

	delta := int64(raw) - int64(u.lastRaw)
	if delta == u.half || delta == -u.half {
		return 0, NewError(ClassTimestampAmbiguous,
			"timestamp transition is exactly half the wrap range").
			WithOffset(int64(inputOffset)).
			WithTolerance(0)
	}
	switch {
	case delta < -u.half:
		u.epoch++
	case delta > u.half:
		u.epoch--
	}
	u.unwrapped = u.epoch*int64(u.modulus) + int64(raw)
	u.lastRaw = raw
	return u.unwrapped, nil
}

// Last returns the most recently expanded value.
func (u *Unwrapper) Last() int64 { return u.unwrapped }

// WrapCount returns the number of forward wraps observed.
func (u *Unwrapper) WrapCount() int64 { return u.epoch }
