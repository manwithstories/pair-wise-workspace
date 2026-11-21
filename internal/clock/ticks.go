// Package clock defines the unified media timeline and exact rational
// conversions between per-track timescales and the shared tick grid.
//
// All arithmetic is integer based: floating point accumulation across
// thousands of samples is forbidden, because it drifts cut points between
// bitrate representations and produces non-reproducible manifests.
package clock

// TicksPerSecond is the resolution of the shared timeline.
// 90 kHz matches the MPEG-2/STB presentation clock and divides evenly by
// every common frame rate (24/25/30/60 fps) and audio rate used in the
// synthetic fixtures, so most conversions are exact.
const TicksPerSecond int64 = 90000

// Rational expresses a timescale ratio without floating point.
type Rational struct {
	Num int64
	Den int64
}

// Scale converts value measured in src units into dst units using exact
// integer math: result = value * dstUnitsPerSecond / srcUnitsPerSecond,
// rounded to nearest with halves rounded up. Callers must pass strictly
// positive unit rates.
func Scale(value, srcUnitsPerSecond, dstUnitsPerSecond int64) int64 {
	if srcUnitsPerSecond <= 0 || dstUnitsPerSecond <= 0 {
		return 0
	}
	num := value * dstUnitsPerSecond
	den := srcUnitsPerSecond
	q := num / den
	r := num % den
	if r*2 >= den {
		q++
	}
	return q
}

// ToTicks converts an amount expressed in the given timescale into shared
// ticks. Negative media times (edit list offsets) are preserved.
func ToTicks(amount int64, timescale uint32) int64 {
	if timescale == 0 {
		return 0
	}
	if amount >= 0 {
		return Scale(amount, int64(timescale), TicksPerSecond)
	}
	return -Scale(-amount, int64(timescale), TicksPerSecond)
}

// FromTicks converts shared ticks back into the given timescale units.
func FromTicks(ticks int64, timescale uint32) int64 {
	if timescale == 0 {
		return 0
	}
	if ticks >= 0 {
		return Scale(ticks, TicksPerSecond, int64(timescale))
	}
	return -Scale(-ticks, TicksPerSecond, int64(timescale))
}

// TicksToMilliseconds renders a tick span for human/terminal reporting.
func TicksToMilliseconds(ticks int64) int64 {
	return Scale(ticks, TicksPerSecond, 1000)
}

// MillisecondsToTicks is the inverse of TicksToMilliseconds.
func MillisecondsToTicks(ms int64) int64 {
	return Scale(ms, 1000, TicksPerSecond)
}

// DurationTicks converts a length-seconds declaration into shared ticks.
func DurationTicks(seconds int64) int64 {
	return seconds * TicksPerSecond
}

// GCD returns the greatest common divisor (Euclid, sign-normalized).
func GCD(a, b int64) int64 {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// Reduce normalizes a rational to lowest terms. A zero denominator is
// returned unchanged rather than panicking; callers validate rates earlier.
func Reduce(r Rational) Rational {
	if r.Den == 0 {
		return r
	}
	g := GCD(r.Num, r.Den)
	if g == 0 {
		return Rational{Num: 0, Den: r.Den}
	}
	return Rational{Num: r.Num / g, Den: r.Den / g}
}
