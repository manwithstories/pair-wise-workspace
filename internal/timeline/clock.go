package timeline

import (
	"fmt"
	"strconv"
	"strings"

	"mediacheck/internal/fault"
)

// Rate is a rational frame rate: Num/Den frames per second.
// Supported: 24, 25, 30 and 30000/1001 (29.97).
type Rate struct {
	Num int64
	Den int64
}

// NanosPerSecond is the shared integer-nanosecond time base.
const NanosPerSecond = int64(1_000_000_000)

// DayNanos is one 24h day in nanoseconds; timecodes wrap past it.
const DayNanos = 24 * 3600 * NanosPerSecond

// MaxSpanNanos bounds any timeline to 48 hours.
const MaxSpanNanos = 2 * DayNanos

// ParseRate parses "24", "25", "30" or "30000/1001".
func ParseRate(s string) (Rate, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		parts := strings.SplitN(s, "/", 2)
		num, err1 := strconv.ParseInt(parts[0], 10, 64)
		den, err2 := strconv.ParseInt(parts[1], 10, 64)
		if err1 != nil || err2 != nil || den <= 0 || num <= 0 {
			return Rate{}, fault.New(fault.CatTimecode, -1, false, "invalid frame rate %q", s)
		}
		return Rate{Num: num, Den: den}, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return Rate{}, fault.New(fault.CatTimecode, -1, false, "invalid frame rate %q", s)
	}
	return Rate{Num: n, Den: 1}, nil
}

// NominalFPS returns the integer frame number ceiling used by timecode
// frame fields (30000/1001 counts frames at 30 per nominal second).
func (r Rate) NominalFPS() int64 {
	return (r.Num + r.Den - 1) / r.Den
}

// DropFrameAllowed reports whether SMPTE drop-frame is defined for this
// rate: only 30000/1001 (dropping 2 frame numbers per non-tenth minute).
func (r Rate) DropFrameAllowed() bool { return r.Num == 30000 && r.Den == 1001 }

// FrameToNanos converts an absolute frame count to nanoseconds with
// round-half-away-from-zero so Frame→Nanos→Frame round-trips exactly.
func (r Rate) FrameToNanos(f int64) int64 {
	v := f * r.Den * NanosPerSecond
	if v >= 0 {
		return (v + r.Num/2) / r.Num
	}
	return -((-v + r.Num/2) / r.Num)
}

// NanosToFrame converts nanoseconds back to the nearest frame.
func (r Rate) NanosToFrame(ns int64) int64 {
	v := ns * r.Num
	d := r.Den * NanosPerSecond
	if v >= 0 {
		return (v + d/2) / d
	}
	return -((-v + d/2) / d)
}

// WrapDay folds a nanosecond timestamp into [0, DayNanos), handling
// cross-midnight wrap of negative or over-day values.
func WrapDay(ns int64) int64 {
	m := ns % DayNanos
	if m < 0 {
		m += DayNanos
	}
	return m
}

// ApplyOffset adds a (possibly negative) offset to a base timestamp.
// Results outside ±MaxSpanNanos are rejected as overflow.
func ApplyOffset(base, offset int64) (int64, error) {
	sum := base + offset
	if (offset > 0 && sum < base) || (offset < 0 && sum > base) || sum > MaxSpanNanos || sum < -MaxSpanNanos {
		return 0, fault.New(fault.CatTimecode, -1, false,
			"offset overflow: base=%d offset=%d", base, offset)
	}
	return sum, nil
}

// ValidateSpan rejects timelines spanning more than 48 hours.
func ValidateSpan(start, end int64, offset int64) error {
	if end < start {
		return fault.New(fault.CatTimecode, offset, true,
			"reversed span: start=%d end=%d", start, end)
	}
	if end-start > MaxSpanNanos {
		return fault.New(fault.CatTimecode, offset, true,
			"span exceeds 48h: %d ns", end-start)
	}
	return nil
}

var _ = fmt.Sprintf
