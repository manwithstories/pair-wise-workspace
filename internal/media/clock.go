package media

import (
	"fmt"
	"math/bits"
	"strconv"
	"strings"
)

// NanosPerSecond is the shared media time base. Every subsystem expresses
// event times as int64 nanoseconds from this base; no float is ever used for
// time accumulation.
const NanosPerSecond int64 = 1_000_000_000

// Rational is an exact, normalized fraction Num/Den. It is the only numeric
// representation used for frame rates, so that 30000/1001 never rounds.
type Rational struct {
	Num int64
	Den int64
}

// NewRational normalizes a fraction. Den must be positive.
func NewRational(num, den int64) (Rational, error) {
	if den <= 0 {
		return Rational{}, fmt.Errorf("rational denominator must be positive: %d", den)
	}
	if num < 0 {
		return Rational{}, fmt.Errorf("rational numerator must be non-negative: %d", num)
	}
	g := gcd(num, den)
	if g == 0 {
		g = 1
	}
	return Rational{Num: num / g, Den: den / g}, nil
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

// String renders the fraction exactly, e.g. "30000/1001" or "25/1".
func (r Rational) String() string {
	return strconv.FormatInt(r.Num, 10) + "/" + strconv.FormatInt(r.Den, 10)
}

// Float is provided ONLY for display/diagnostics. Never use it for time math.
func (r Rational) Float() float64 { return float64(r.Num) / float64(r.Den) }

// Equal compares exact values.
func (r Rational) Equal(o Rational) bool { return r.Num*o.Den == o.Num*r.Den }

// Cmp returns -1, 0 or 1 comparing r to o.
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

// FrameRate is a rational frame rate plus its SMPTE counting mode. DropFrame
// is only meaningful for rates whose seconds are not wall-clock seconds,
// i.e. 30000/1001 and 60000/1001.
type FrameRate struct {
	Rational
	DropFrame bool
}

// The four rates required by the pipeline. 29.97 carries the drop-frame flag;
// 30 is the exact integer rate.
var (
	Rate24   = FrameRate{Rational: Rational{Num: 24, Den: 1}}
	Rate25   = FrameRate{Rational: Rational{Num: 25, Den: 1}}
	Rate30   = FrameRate{Rational: Rational{Num: 30, Den: 1}}
	Rate2997 = FrameRate{Rational: Rational{Num: 30000, Den: 1001}, DropFrame: true}
)

// Label returns a stable human-readable identifier such as "30000/1001df".
func (f FrameRate) Label() string {
	s := f.Rational.String()
	if f.DropFrame {
		s += "df"
	}
	return s
}

// SupportsDropFrame reports whether DF counting is defined for this rate.
func (f FrameRate) SupportsDropFrame() bool {
	return f.Rational.Equal(Rational{Num: 30000, Den: 1001}) ||
		f.Rational.Equal(Rational{Num: 60000, Den: 1001})
}

// dropCountPerMinute is the number of frame numbers skipped per minute in DF
// counting (2 for 29.97, 4 for 59.94).
func (f FrameRate) dropCountPerMinute() int64 {
	if f.Rational.Equal(Rational{Num: 60000, Den: 1001}) {
		return 4
	}
	return 2
}

// NominalFPS is the integer rate used for timecode digit arithmetic
// (30 for 29.97, 24/25/30 otherwise).
func (f FrameRate) NominalFPS() int64 {
	if f.Rational.Equal(Rational{Num: 30000, Den: 1001}) {
		return 30
	}
	if f.Rational.Equal(Rational{Num: 60000, Den: 1001}) {
		return 60
	}
	return f.Num / f.Den
}

// ParseFrameRate accepts "24", "25", "30", "30000/1001", optionally suffixed
// with "df"/"ndf" for 29.97/59.94. Bare "29.97" is rejected as ambiguous;
// callers must state the exact rational.
func ParseFrameRate(s string) (FrameRate, error) {
	orig := s
	df := false
	lower := strings.ToLower(strings.TrimSpace(s))
	switch {
	case strings.HasSuffix(lower, "df") && !strings.HasSuffix(lower, "ndf"):
		df = true
		lower = strings.TrimSuffix(lower, "df")
	case strings.HasSuffix(lower, "ndf"):
		lower = strings.TrimSuffix(lower, "ndf")
	}
	var r Rational
	if strings.Contains(lower, "/") {
		parts := strings.SplitN(lower, "/", 2)
		num, err1 := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		den, err2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err1 != nil || err2 != nil {
			return FrameRate{}, fmt.Errorf("malformed frame rate %q", orig)
		}
		nr, err := NewRational(num, den)
		if err != nil {
			return FrameRate{}, fmt.Errorf("malformed frame rate %q: %w", orig, err)
		}
		r = nr
	} else {
		n, err := strconv.ParseInt(lower, 10, 64)
		if err != nil {
			return FrameRate{}, fmt.Errorf("malformed frame rate %q (use 24, 25, 30 or 30000/1001)", orig)
		}
		nr, err := NewRational(n, 1)
		if err != nil {
			return FrameRate{}, fmt.Errorf("malformed frame rate %q: %w", orig, err)
		}
		r = nr
	}
	// Normalize 30000/1001 with the conventional DF default.
	rate := FrameRate{Rational: r, DropFrame: df}
	if rate.DropFrame && !rate.SupportsDropFrame() {
		return FrameRate{}, fmt.Errorf("frame rate %s does not support drop-frame counting", r)
	}
	if !rate.DropFrame && rate.SupportsDropFrame() {
		// NDF is a valid, explicitly requested mode for 29.97/59.94.
		return rate, nil
	}
	if rate.NominalFPS() <= 0 {
		return FrameRate{}, fmt.Errorf("frame rate %q has no positive nominal rate", orig)
	}
	return rate, nil
}

// ScaleToNanos converts a tick count in an arbitrary timescale to the shared
// nanosecond base using exact 128-bit integer arithmetic:
//
//	nanos = floor(ticks * 1e9 / timescale)
//
// It is used for MP4 media times, RTP clock rates and any other integer
// timebase; no float is involved.
func ScaleToNanos(ticks int64, timescale uint32) (int64, error) {
	if ticks < 0 {
		return 0, fmt.Errorf("negative tick count %d", ticks)
	}
	if timescale == 0 {
		return 0, fmt.Errorf("zero timescale")
	}
	hi, lo := bits.Mul64(uint64(ticks), uint64(NanosPerSecond))
	if hi >= uint64(timescale) {
		return 0, fmt.Errorf("tick count %d overflows nanosecond base at timescale %d", ticks, timescale)
	}
	q, _ := bits.Div64(hi, lo, uint64(timescale))
	if q > uint64(^uint64(0)>>1) {
		return 0, fmt.Errorf("tick count %d exceeds int64 nanoseconds", ticks)
	}
	return int64(q), nil
}

// FrameToNanos maps an absolute frame index to nanoseconds on the shared base
// using exact 128-bit integer arithmetic:
//
//	nanos = floor(frame * 1e9 * Den / Num)
//
// There is no floating-point accumulation; the result for a given frame is
// identical on every run and platform.
func (f FrameRate) FrameToNanos(frame int64) (int64, error) {
	if frame < 0 {
		return 0, fmt.Errorf("negative frame index %d", frame)
	}
	if f.Num <= 0 || f.Den <= 0 {
		return 0, fmt.Errorf("invalid frame rate %s", f.Rational)
	}
	den := uint64(f.Den)
	if hi, _ := bits.Mul64(den, uint64(NanosPerSecond)); hi != 0 {
		return 0, fmt.Errorf("frame rate %s denominator overflows conversion", f.Rational)
	}
	b := den * uint64(NanosPerSecond)
	hi, lo := bits.Mul64(uint64(frame), b)
	if hi >= uint64(f.Num) {
		return 0, fmt.Errorf("frame %d overflows nanosecond base at rate %s", frame, f.Rational)
	}
	q, _ := bits.Div64(hi, lo, uint64(f.Num))
	if q > uint64(^uint64(0)>>1) {
		return 0, fmt.Errorf("frame %d exceeds int64 nanoseconds", frame)
	}
	return int64(q), nil
}

// NanosToFrame returns the frame whose start is nearest to ns, plus the
// remainder in nanoseconds. A non-zero remainder means ns is not on a frame
// boundary ("非整帧时间") and callers should report ClassTimecodeNonIntegralTime.
//
// Nearest rather than floor is deliberate: FrameToNanos floors the ideal
// rational frame start to an integer nanosecond, which can land up to 1ns
// below the ideal boundary; flooring here would misattribute such a value to
// the previous frame, breaking the FrameToNanos/NanosToFrame round trip.
// Ties resolve downward, so the mapping is total and deterministic.
func (f FrameRate) NanosToFrame(ns int64) (frame int64, remainderNs int64, err error) {
	if ns < 0 {
		return 0, 0, fmt.Errorf("negative nanosecond value %d", ns)
	}
	if f.Num <= 0 || f.Den <= 0 {
		return 0, 0, fmt.Errorf("invalid frame rate %s", f.Rational)
	}
	if hi, _ := bits.Mul64(uint64(f.Den), uint64(NanosPerSecond)); hi != 0 {
		return 0, 0, fmt.Errorf("frame rate %s denominator overflows conversion", f.Rational)
	}
	div := uint64(f.Den) * uint64(NanosPerSecond)
	// nearest = floor((2*ns*Num + div) / (2*div))
	hi, lo := bits.Mul64(uint64(ns), uint64(f.Num))
	// hi:lo = 2*(hi:lo) = (2*hi + carry):(2*lo)
	lo2 := lo << 1
	hi2 := hi<<1 | lo>>63
	var carry uint64
	lo2, carry = bits.Add64(lo2, div, 0)
	hi2 += carry
	hi2 += div >> 63 // div < 2^63 for every supported rate, kept for clarity
	if hi2 >= 2*div {
		return 0, 0, fmt.Errorf("nanosecond value %d overflows frame conversion", ns)
	}
	q, _ := bits.Div64(hi2, lo2, 2*div)
	if q > uint64(^uint64(0)>>1) {
		return 0, 0, fmt.Errorf("nanosecond value %d exceeds int64 frames", ns)
	}
	frame = int64(q)
	start, err := f.FrameToNanos(frame)
	if err != nil {
		return 0, 0, err
	}
	if start > ns && frame > 0 {
		// Rounded up past the boundary: step back so the remainder stays in
		// [0, frameDuration) and the caller can treat it as elapsed-in-frame.
		frame--
		if start, err = f.FrameToNanos(frame); err != nil {
			return 0, 0, err
		}
	}
	return frame, ns - start, nil
}
