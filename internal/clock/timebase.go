// Package clock provides rational media time bases, clock unification and a
// single error model shared by every stage of the media core.
//
// All timestamps are integer tick counts in a rational time base (num/den
// seconds per tick). The package refuses invalid time bases, negative
// durations and any conversion that would overflow int64. Errors always carry
// the offending track and the time position so that corrupt records can be
// located instead of being mistaken for valid media.
package clock

import (
	"errors"
	"fmt"
	"math"
	"math/big"
)

// NoTimestamp marks an unknown timestamp/duration (FFmpeg's AV_NOPTS_VALUE).
const NoTimestamp int64 = math.MinInt64

// ErrCode is a stable machine-readable classification for clock failures.
type ErrCode string

const (
	CodeInvalidTimeBase  ErrCode = "invalid_timebase"
	CodeInvalidTimestamp ErrCode = "invalid_timestamp"
	CodeNegativeDuration ErrCode = "negative_duration"
	CodeOverflow         ErrCode = "timestamp_overflow"
	CodeTimelineConflict ErrCode = "timeline_conflict"
	CodeOutOfRange       ErrCode = "out_of_range"
)

// Error is the unified error type. It is comparable by ErrCode through
// errors.Is against the sentinel values below, and it always reports the
// track and time position involved in the failure.
type Error struct {
	Code     ErrCode
	Op       string
	Track    int
	Position int64
	TimeBase Rational
	Detail   string
	Err      error
}

// Sentinel errors for the shared error model.
var (
	ErrInvalidTimeBase  = &Error{Code: CodeInvalidTimeBase}
	ErrInvalidTimestamp = &Error{Code: CodeInvalidTimestamp}
	ErrNegativeDuration = &Error{Code: CodeNegativeDuration}
	ErrOverflow         = &Error{Code: CodeOverflow}
	ErrTimelineConflict = &Error{Code: CodeTimelineConflict}
	ErrOutOfRange       = &Error{Code: CodeOutOfRange}
)

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	loc := e.location()
	msg := string(e.Code)
	if e.Op != "" {
		msg = e.Op + ": " + msg
	}
	if loc != "" {
		msg += " " + loc
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return "clock: " + msg
}

// Unwrap exposes the underlying cause, if any.
func (e *Error) Unwrap() error { return e.Err }

// Is matches errors.Is against the package sentinels by code.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return e.Code == t.Code
}

// location renders the track/time position when known.
func (e *Error) location() string {
	parts := make([]string, 0, 3)
	if e.Track >= 0 {
		parts = append(parts, fmt.Sprintf("track=%d", e.Track))
	}
	if e.Position != NoTimestamp {
		parts = append(parts, fmt.Sprintf("pos=%d", e.Position))
		if e.TimeBase.Valid() {
			parts = append(parts, "t="+e.TimeBase.SecondsString(e.Position))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	s := parts[0]
	for _, p := range parts[1:] {
		s += " " + p
	}
	return s
}

// WithPosition returns a copy carrying track and position information.
func (e *Error) WithPosition(track int, pos int64, tb Rational) *Error {
	cp := *e
	cp.Track = track
	cp.Position = pos
	cp.TimeBase = tb
	return &cp
}

// AsError extracts the unified error from err.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// Rational is a normalized positive rational number used as a time base:
// one tick is Num/Den seconds.
type Rational struct {
	Num int64
	Den int64
}

// Microsecond is the unified timeline base used by the media core.
var Microsecond = Rational{Num: 1, Den: 1_000_000}

// NewRational builds a reduced positive rational. Zero or negative
// numerator/denominator values are rejected as invalid time bases.
func NewRational(num, den int64) (Rational, error) {
	if num <= 0 || den <= 0 {
		return Rational{}, &Error{
			Code:   CodeInvalidTimeBase,
			Op:     "new_rational",
			Detail: fmt.Sprintf("num=%d den=%d must be positive", num, den),
		}
	}
	g := gcd(num, den)
	return Rational{Num: num / g, Den: den / g}, nil
}

// Valid reports whether r can be used as a time base.
func (r Rational) Valid() bool { return r.Num > 0 && r.Den > 0 }

// String renders the rational as num/den.
func (r Rational) String() string { return fmt.Sprintf("%d/%d", r.Num, r.Den) }

// Seconds returns ticks converted to seconds as float64.
func (r Rational) Seconds(ticks int64) float64 {
	return float64(ticks) * float64(r.Num) / float64(r.Den)
}

// SecondsString renders a tick count as seconds with microsecond precision.
func (r Rational) SecondsString(ticks int64) string {
	return fmt.Sprintf("%.6fs", r.Seconds(ticks))
}

// Compare orders two time bases.
func (r Rational) Compare(o Rational) int {
	l := new(big.Int).Mul(big.NewInt(r.Num), big.NewInt(o.Den))
	rr := new(big.Int).Mul(big.NewInt(o.Num), big.NewInt(r.Den))
	return l.Cmp(rr)
}

// Convert rescales ticks from src to dst with round-half-away-from-zero and
// refuses results outside int64. The computation is exact (big.Int), so a
// 1/1000 -> 1/90000 rescale never accumulates error.
func Convert(ticks int64, src, dst Rational) (int64, error) {
	if !src.Valid() || !dst.Valid() {
		return 0, &Error{
			Code:   CodeInvalidTimeBase,
			Op:     "convert",
			Detail: fmt.Sprintf("src=%s dst=%s", src, dst),
		}
	}
	if ticks == NoTimestamp {
		return NoTimestamp, nil
	}
	// value = ticks * src.Num / src.Den seconds -> dst ticks:
	// out = ticks * src.Num * dst.Den / (src.Den * dst.Num)
	num := new(big.Int).SetInt64(ticks)
	num.Mul(num, big.NewInt(src.Num))
	num.Mul(num, big.NewInt(dst.Den))
	den := new(big.Int).Mul(big.NewInt(src.Den), big.NewInt(dst.Num))

	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	// Round half away from zero.
	absRem := new(big.Int).Abs(rem)
	absRem.Lsh(absRem, 1)
	if absRem.Cmp(den) >= 0 {
		if num.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	if !q.IsInt64() {
		return 0, &Error{
			Code:     CodeOverflow,
			Op:       "convert",
			Position: ticks,
			TimeBase: src,
			Detail:   fmt.Sprintf("rescale %s->%s overflows int64", src, dst),
		}
	}
	return q.Int64(), nil
}

// AddTicks adds two tick counts rejecting int64 overflow. NoTimestamp in
// either operand yields NoTimestamp.
func AddTicks(a, b int64) (int64, error) {
	if a == NoTimestamp || b == NoTimestamp {
		return NoTimestamp, nil
	}
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, &Error{Code: CodeOverflow, Op: "add_ticks", Detail: fmt.Sprintf("%d + %d", a, b)}
	}
	return a + b, nil
}

// MulTicks multiplies a tick count by a positive integer factor, rejecting
// overflow and invalid factors.
func MulTicks(ticks, factor int64) (int64, error) {
	if factor < 0 {
		return 0, &Error{Code: CodeNegativeDuration, Op: "mul_ticks", Detail: fmt.Sprintf("factor=%d", factor)}
	}
	if ticks > 0 && factor > math.MaxInt64/ticks {
		return 0, &Error{Code: CodeOverflow, Op: "mul_ticks", Detail: fmt.Sprintf("%d * %d", ticks, factor)}
	}
	if ticks < 0 && factor > 0 && ticks < math.MinInt64/factor {
		return 0, &Error{Code: CodeOverflow, Op: "mul_ticks", Detail: fmt.Sprintf("%d * %d", ticks, factor)}
	}
	return ticks * factor, nil
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
