// Package clock defines the unified rational time base shared by the RTP
// reorderer, access-unit parser, timeline, silence detector and fingerprint
// modules. All timestamps are integer ticks in a rational time base so that
// mixed Opus/AAC variable-duration packets align exactly, with no float drift.
package clock

import "fmt"

// Rat is a reduced rational number Num/Den (Den > 0).
type Rat struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

func gcd(a, b int64) int64 {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return 1
	}
	return a
}

// NewRat returns num/den reduced to lowest terms.
func NewRat(num, den int64) Rat {
	if den == 0 {
		panic("clock: zero denominator")
	}
	if den < 0 {
		num, den = -num, -den
	}
	g := gcd(num, den)
	return Rat{Num: num / g, Den: den / g}
}

// TimeBase is the duration of one tick in seconds, e.g. NewRat(1, 48000).
type TimeBase = Rat

// Common time bases used across the pipeline.
var (
	Base48k = NewRat(1, 48000) // Opus reference base
	Base44k = NewRat(1, 44100) // AAC reference base
	Base90k = NewRat(1, 90000) // RTP transport base
)

// Time is an instant expressed as integer Ticks in Base.
type Time struct {
	Ticks int64 `json:"ticks"`
	Base  Rat   `json:"base"`
}

// At builds a Time of n ticks in base b.
func At(ticks int64, b TimeBase) Time { return Time{Ticks: ticks, Base: b} }

// lcm of two positive denominators.
func lcm(a, b int64) int64 { return a / gcd(a, b) * b }

// Convert re-expresses t in base b. The result is exact because bases are
// rational: ticks are scaled by (t.Base.Num*b.Den)/(t.Base.Den*b.Num) and the
// division must come out even, otherwise the caller asked for a sub-tick
// precision the target base cannot represent and we truncate toward zero.
func (t Time) Convert(b TimeBase) Time {
	num := t.Ticks * t.Base.Num * b.Den
	den := t.Base.Den * b.Num
	return Time{Ticks: num / den, Base: b}
}

// Add returns t+d, converting d into t's base.
func (t Time) Add(d Time) Time {
	d = d.Convert(t.Base)
	return Time{Ticks: t.Ticks + d.Ticks, Base: t.Base}
}

// Sub returns t-o in t's base.
func (t Time) Sub(o Time) Time {
	o = o.Convert(t.Base)
	return Time{Ticks: t.Ticks - o.Ticks, Base: t.Base}
}

// Cmp compares two instants across bases: -1, 0 or +1.
func (t Time) Cmp(o Time) int {
	b := NewRat(1, lcm(t.Base.Den, o.Base.Den))
	a, c := t.Convert(b), o.Convert(b)
	switch {
	case a.Ticks < c.Ticks:
		return -1
	case a.Ticks > c.Ticks:
		return 1
	}
	return 0
}

// Seconds returns the instant as a float64 (display only).
func (t Time) Seconds() float64 {
	return float64(t.Ticks) * float64(t.Base.Num) / float64(t.Base.Den)
}

// IsZero reports whether t is the zero instant.
func (t Time) IsZero() bool { return t.Ticks == 0 }

func (t Time) String() string {
	return fmt.Sprintf("%d@%d/%d", t.Ticks, t.Base.Num, t.Base.Den)
}
