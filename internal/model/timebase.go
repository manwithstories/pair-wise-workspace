// Package model holds the public rational timebase shared by every stage of
// the relay pipeline. No stage may compare raw source timestamps directly;
// timestamps are converted into this timebase first.
package model

import (
	"errors"
	"fmt"
	"math/bits"
	"strconv"
	"strings"
)

// ErrZeroDenominator is returned when a rational timebase has denominator 0.
var ErrZeroDenominator = errors.New("timebase: zero denominator")

// ErrWrapExceeded is returned when a 32-bit wrap expansion would overflow the
// supported 64-bit tick range.
var ErrWrapExceeded = errors.New("timebase: 32-bit wrap expansion exceeded 64-bit range")

// Timebase is a positive rational number of ticks per second.
//
// Every rate in media (90 kHz RTP, 1000/1 ms, 30000/1001 fps, ...) can be
// expressed exactly as a Timebase, so timing math never falls back to float.
type Timebase struct {
	Num int64 // ticks per second numerator
	Den int64 // ticks per second denominator
}

// NewTimebase returns a normalized Timebase, rejecting a zero denominator.
func NewTimebase(num, den int64) (Timebase, error) {
	if den == 0 {
		return Timebase{}, ErrZeroDenominator
	}
	if den < 0 {
		num, den = -num, -den
	}
	if num <= 0 {
		return Timebase{}, fmt.Errorf("timebase: non-positive numerator %d/%d", num, den)
	}
	g := gcd(num, den)
	return Timebase{Num: num / g, Den: den / g}, nil
}

// MustTimebase is NewTimebase for compile-time constant rates.
func MustTimebase(num, den int64) Timebase {
	tb, err := NewTimebase(num, den)
	if err != nil {
		panic(err)
	}
	return tb
}

// RTP90k is the RTP timestamp clock used by MJPEG video (90 kHz).
var RTP90k = MustTimebase(90000, 1)

// Millis is the MXF/container edit-unit clock (1000 ticks per second).
var Millis = MustTimebase(1000, 1)

// NTSC30 is the 29.97 fps rate expressed as an exact rational.
var NTSC30 = MustTimebase(30000, 1001)

// Equal reports whether two timebases represent the same rate.
func (tb Timebase) Equal(other Timebase) bool {
	return tb.Num == other.Num && tb.Den == other.Den
}

// String renders the timebase as "num/den".
func (tb Timebase) String() string {
	return strconv.FormatInt(tb.Num, 10) + "/" + strconv.FormatInt(tb.Den, 10)
}

// Rate returns ticks per second as an exact TX{Num,Den} rational.
func (tb Timebase) Rate() (num, den int64) {
	return tb.Num, tb.Den
}

// Ratio returns the exact conversion ratio from tb to dst: the number that
// multiplies a tick count in tb to produce a tick count in dst.
func (tb Timebase) Ratio(dst Timebase) (num, den int64) {
	return dst.Num * tb.Den, tb.Num * dst.Den
}

// ConvertTo converts ticks from tb into dst, rounding half to even so results
// are symmetric and reproducible. The multiplication is checked for overflow.
func (tb Timebase) ConvertTo(ticks int64, dst Timebase) (int64, error) {
	if tb.Equal(dst) {
		return ticks, nil
	}
	rnum, rden := tb.Ratio(dst)
	hi, lo := bits.Mul64(uint64(abs64(ticks)), uint64(abs64(rnum)))
	if hi != 0 {
		return 0, fmt.Errorf("timebase: convert %d %s->%s overflows 64 bits", ticks, tb, dst)
	}
	prod := int64(lo)
	if (ticks < 0) != (rnum < 0) {
		prod = -prod
	}
	return roundDiv(prod, rden), nil
}

// MustConvertTo is ConvertTo that panics on overflow; use only for values
// already bounded by the parser limits.
func (tb Timebase) MustConvertTo(ticks int64, dst Timebase) int64 {
	out, err := tb.ConvertTo(ticks, dst)
	if err != nil {
		panic(err)
	}
	return out
}

// SecondsString renders ticks in tb as an exact decimal string with up to six
// fractional digits, used only for terminal reporting.
func (tb Timebase) SecondsString(ticks int64) string {
	if tb.Num == 0 {
		return "0.000000"
	}
	tf := tb.MustConvertTo(ticks, MustTimebase(1000000, 1))
	sign := ""
	if tf < 0 {
		sign, tf = "-", -tf
	}
	frac := tf % 1000000
	return fmt.Sprintf("%s%d.%06d", sign, tf/1000000, frac)
}

// ParseTimebase parses "num/den" or a bare integer rate.
func ParseTimebase(s string) (Timebase, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Timebase{}, errors.New("timebase: empty")
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		num, err := strconv.ParseInt(strings.TrimSpace(s[:i]), 10, 64)
		if err != nil {
			return Timebase{}, fmt.Errorf("timebase: bad numerator %q: %w", s[:i], err)
		}
		den, err := strconv.ParseInt(strings.TrimSpace(s[i+1:]), 10, 64)
		if err != nil {
			return Timebase{}, fmt.Errorf("timebase: bad denominator %q: %w", s[i+1:], err)
		}
		return NewTimebase(num, den)
	}
	num, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return Timebase{}, fmt.Errorf("timebase: bad rate %q: %w", s, err)
	}
	return NewTimebase(num, 1)
}

// ExpandWrap turns a 32-bit wrapping timestamp plus an epoch into an
// unwrapped 64-bit tick count. Timestamps are non-negative after expansion;
// the first sample being below the previous unwrapped one by more than half
// the 32-bit range is treated as a wrap.
func ExpandWrap(raw uint32, epoch int64) (int64, error) {
	// An epoch of N means we already observed N wraps; the raw value lives in
	// the window [N*2^32, (N+1)*2^32).
	if epoch > (1<<31)-1 || epoch < 0 {
		return 0, fmt.Errorf("timebase: wrap epoch %d out of range", epoch)
	}
	out := epoch<<32 | int64(raw)
	if out < 0 {
		return 0, ErrWrapExceeded
	}
	return out, nil
}

// UnwrapSequence expands a sequence of 32-bit timestamps, detecting each wrap
// when the delta from the previous raw value is more than half the 32-bit
// range (a backwards jump) or the current value wraps past the modulus.
// The returned epochs slice gives the wrap count for each sample; the count of
// transitions where the epoch increased is the number of observed wraps.
func UnwrapSequence(raws []uint32, startEpoch int64) (ticks []int64, epochs []int64, err error) {
	if startEpoch < 0 || startEpoch > (1<<31)-1 {
		return nil, nil, fmt.Errorf("timebase: start epoch %d out of range", startEpoch)
	}
	ticks = make([]int64, len(raws))
	epochs = make([]int64, len(raws))
	epoch := startEpoch
	var prev uint32
	for i, r := range raws {
		if i > 0 {
			// A decrease in the raw value (any amount) is only a wrap when the
			// forward delta is smaller than the backward delta.
			if r < prev {
				fwd := int64(r) + (1 << 32) - int64(prev)
				back := int64(prev) - int64(r)
				if fwd <= back {
					epoch++
				}
			}
		}
		epochs[i] = epoch
		ticks[i] = epoch<<32 | int64(r)
		if ticks[i] < 0 {
			return nil, nil, ErrWrapExceeded
		}
		prev = r
	}
	return ticks, epochs, nil
}

// CountWraps returns how many times the epoch increased across a sequence of
// per-sample epochs.
func CountWraps(epochs []int64) int {
	n := 0
	for i := 1; i < len(epochs); i++ {
		if epochs[i] > epochs[i-1] {
			n += int(epochs[i] - epochs[i-1])
		}
	}
	return n
}

// WrapCount32 returns wraps assuming a 32-bit modulus and the given raw
// timestamps in arrival order.
func WrapCount32(raws []uint32) int {
	_, epochs, err := UnwrapSequence(raws, 0)
	if err != nil {
		return 0
	}
	return CountWraps(epochs)
}

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

// roundDiv divides n by d (d > 0) rounding half to even.
func roundDiv(n, d int64) int64 {
	if d <= 0 {
		panic(fmt.Sprintf("timebase: roundDiv non-positive divisor %d", d))
	}
	q, r := n/d, n%d
	if r < 0 {
		r += d
		q--
	}
	// q = floor(n/d); compare 2r against d
	switch {
	case 2*r < d:
		return q
	case 2*r > d:
		return q + 1
	}
	if q%2 == 0 {
		return q
	}
	return q + 1
}
