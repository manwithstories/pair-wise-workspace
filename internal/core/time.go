// Package core contains the integer media time model and the structured error
// type shared by every FieldPost stage.
package core

import (
	"fmt"
	"time"
)

// Scale is the number of integer ticks in one second.
const Scale uint32 = 1_000_000_000

// Ticks is a timestamp or duration in a track-specific integer timebase.
type Ticks int64

// Timebase describes an integer media time scale.
type Timebase struct {
	Scale uint32
}

// Nanoseconds is the common lossless-enough base used by manifests. Container
// tracks retain their own Scale so remuxing does not round source timestamps.
var Nanoseconds = Timebase{Scale: Scale}

// Duration converts ticks in tb to time.Duration.
func (tb Timebase) Duration(v Ticks) time.Duration {
	if tb.Scale == 0 {
		return 0
	}
	sec := int64(v) / int64(tb.Scale)
	rem := int64(v) % int64(tb.Scale)
	return time.Duration(sec)*time.Second +
		time.Duration(rem)*time.Nanosecond*time.Duration(Scale)/time.Duration(tb.Scale)
}

// FromDuration converts a duration to integer ticks in tb.
func (tb Timebase) FromDuration(d time.Duration) Ticks {
	if tb.Scale == 0 {
		return 0
	}
	return Ticks(int64(d) * int64(tb.Scale) / int64(time.Second))
}

// Convert converts exact integer ticks between timebases. Callers should only
// convert when divisibility is known; CanConvert reports that condition.
func Convert(v Ticks, from, to Timebase) Ticks {
	if from.Scale == 0 || to.Scale == 0 {
		return 0
	}
	return Ticks(int64(v) * int64(to.Scale) / int64(from.Scale))
}

// CanConvert reports whether conversion is exact without rounding.
func CanConvert(from, to Timebase) bool {
	return from.Scale != 0 && to.Scale != 0 &&
		(int64(to.Scale)%int64(from.Scale) == 0 ||
			int64(from.Scale)%int64(to.Scale) == 0)
}

// ParseSeconds parses a manifest decimal seconds value without floating point.
// It returns nanosecond-scale integer ticks and rejects negative values.
func ParseSeconds(s string) (Ticks, error) {
	neg := false
	i := 0
	if s != "" && (s[0] == '-' || s[0] == '+') {
		neg = s[0] == '-'
		i = 1
	}
	if i == len(s) {
		return 0, fmt.Errorf("empty seconds value")
	}
	var whole, frac int64
	var digits int
	fracSet := false
	for ; i < len(s); i++ {
		c := s[i]
		if c == '.' {
			if fracSet {
				return 0, fmt.Errorf("multiple decimal points")
			}
			fracSet = true
			continue
		}
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid seconds digit %q", c)
		}
		d := int64(c - '0')
		if fracSet {
			if digits < 9 {
				frac = frac*10 + d
				digits++
			}
		} else {
			whole = whole*10 + d
		}
	}
	for digits < 9 {
		frac *= 10
		digits++
	}
	for digits > 9 { // input had >9 fractional digits; truncate deterministically
		frac /= 10
		digits--
	}
	n := whole*int64(Scale) + frac
	if neg {
		if n == 0 {
			return 0, &NegativeTimeError{Value: s}
		}
		return 0, &NegativeTimeError{Value: s}
	}
	return Ticks(n), nil
}

// NegativeTimeError marks a negative manifest duration or timestamp.
type NegativeTimeError struct{ Value string }

func (e *NegativeTimeError) Error() string {
	return fmt.Sprintf("negative media time %q", e.Value)
}

// Timeline validates a monotonically non-decreasing sequence in one timebase.
// A non-zero first timestamp is retained rather than normalised to zero.
type Timeline struct {
	Base  Ticks
	Last  Ticks
	Seen  bool
	Scale uint32
}

// Observe records t and rejects backwards movement. Equal decode times are
// permitted because frame reordering may share a base while PTS carries order.
func (tl *Timeline) Observe(t Ticks) error {
	if t < 0 {
		return &Error{Stage: StageDemux, Code: ExitInvariant, Retryable: false,
			Message: fmt.Sprintf("negative timestamp %d", t)}
	}
	if !tl.Seen {
		tl.Base = t
		tl.Last = t
		tl.Seen = true
		return nil
	}
	if t < tl.Last {
		return &Error{Stage: StageDemux, Code: ExitInvariant, Retryable: false,
			Offset: int64(t), Message: fmt.Sprintf("timestamp rollback: %d < %d", t, tl.Last)}
	}
	tl.Last = t
	return nil
}
