// Package timeline defines the single media time base shared by transport,
// codec indexing, audio processing and reporting.
package timeline

import (
	"errors"
	"math"

	"mediamix/internal/core"
)

// Micros is a signed media time in microseconds. Negative values are always
// invalid input: callers must never guess an origin.
type Micros int64

// MicrosPerSecond is the conversion constant for Micros.
const MicrosPerSecond Micros = 1_000_000

// TimeBase maps integer ticks (RTP timestamps or audio sample positions) onto
// the shared microsecond timeline.
type TimeBase struct {
	// Timescale is ticks per second. It is never 0 for accepted input.
	Timescale uint32
	// Origin shifts tick 0 onto the presentation timeline.
	Origin Micros
}

// NewTimeBase rejects a zero timescale instead of silently assuming one.
func NewTimeBase(timescale uint32) (TimeBase, error) {
	if timescale == 0 {
		return TimeBase{}, &core.Error{
			Code:   core.CodeBadTimestamp,
			Detail: "timescale is zero: media time base cannot be determined",
			Fatal:  true,
		}
	}
	return TimeBase{Timescale: timescale}, nil
}

// NewTimeBaseAt builds a time base with an explicit presentation origin.
func NewTimeBaseAt(timescale uint32, origin Micros) (TimeBase, error) {
	tb, err := NewTimeBase(timescale)
	if err != nil {
		return TimeBase{}, err
	}
	if origin < 0 {
		return TimeBase{}, &core.Error{
			Code:   core.CodeBadTimestamp,
			Detail: "negative presentation origin",
			Fatal:  true,
		}
	}
	tb.Origin = origin
	return tb, nil
}

// Valid reports whether the time base can be used without guessing.
func (tb TimeBase) Valid() bool { return tb.Timescale > 0 }

// ToMicros converts non-negative ticks to presentation microseconds. Ticks must
// fit comfortably in int64 after scaling; absurd inputs are rejected.
func (tb TimeBase) ToMicros(ticks int64) (Micros, error) {
	if !tb.Valid() {
		return 0, &core.Error{Code: core.CodeBadTimestamp, Detail: "unset time base", Fatal: true}
	}
	if ticks < 0 {
		return 0, &core.Error{Code: core.CodeBadTimestamp, Detail: "negative timestamp", Fatal: true}
	}
	const maxTicks = int64(math.MaxInt64 / int64(MicrosPerSecond))
	if ticks > maxTicks {
		return 0, &core.Error{Code: core.CodeBadTimestamp, Detail: "timestamp overflows microseconds", Fatal: true}
	}
	us := (ticks*int64(MicrosPerSecond) + int64(tb.Timescale)/2) / int64(tb.Timescale)
	return Micros(us) + tb.Origin, nil
}

// FromMicros converts presentation microseconds back to ticks, rounding to the
// nearest tick. Micros before the origin are rejected.
func (tb TimeBase) FromMicros(us Micros) (int64, error) {
	if !tb.Valid() {
		return 0, &core.Error{Code: core.CodeBadTimestamp, Detail: "unset time base", Fatal: true}
	}
	rel := us - tb.Origin
	if rel < 0 {
		return 0, &core.Error{Code: core.CodeBadTimestamp, Detail: "timestamp precedes time base origin", Fatal: true}
	}
	if int64(rel) > math.MaxInt64/int64(tb.Timescale) {
		return 0, &core.Error{Code: core.CodeBadTimestamp, Detail: "timestamp overflows ticks", Fatal: true}
	}
	return (int64(rel)*int64(tb.Timescale) + int64(MicrosPerSecond)/2) / int64(MicrosPerSecond), nil
}

// TicksFor returns the exact tick offset for a media duration, rounding up.
func (tb TimeBase) TicksFor(d Micros) (int64, error) {
	if !tb.Valid() {
		return 0, &core.Error{Code: core.CodeBadTimestamp, Detail: "unset time base", Fatal: true}
	}
	if d < 0 {
		return 0, &core.Error{Code: core.CodeBadTimestamp, Detail: "negative duration", Fatal: true}
	}
	return (int64(d)*int64(tb.Timescale) + int64(MicrosPerSecond) - 1) / int64(MicrosPerSecond), nil
}

// Span is the half-open media interval [Start, End).
type Span struct {
	Start Micros `json:"start_us"`
	End   Micros `json:"end_us"`
}

// NewSpan validates a start and duration and returns the resulting interval.
func NewSpan(start, duration Micros) (Span, error) {
	if start < 0 {
		return Span{}, &core.Error{Code: core.CodeBadTimestamp, Detail: "segment start is negative", Fatal: true}
	}
	if duration < 0 {
		return Span{}, &core.Error{Code: core.CodeBadTimestamp, Detail: "segment duration is negative", Fatal: true}
	}
	if duration > 0 && start > math.MaxInt64-Micros(duration) {
		return Span{}, &core.Error{Code: core.CodeBadTimestamp, Detail: "segment end overflows", Fatal: true}
	}
	return Span{Start: start, End: start + duration}, nil
}

// Duration returns the span length.
func (s Span) Duration() Micros { return s.End - s.Start }

// Valid reports whether the interval is ordered and non-negative.
func (s Span) Valid() bool { return s.Start >= 0 && s.End >= s.Start }

// Empty reports a zero-length interval.
func (s Span) Empty() bool { return s.End <= s.Start }

// Overlaps reports whether two half-open intervals share media time.
func (s Span) Overlaps(o Span) bool {
	return s.Start < o.End && o.Start < s.End
}

// Contains reports whether t lies in [Start, End).
func (s Span) Contains(t Micros) bool { return s.Start <= t && t < s.End }

// Shift moves the interval by d, rejecting a negative result.
func (s Span) Shift(d Micros) (Span, error) {
	return NewSpan(s.Start+d, s.Duration())
}

// Union returns the smallest interval containing both spans.
func (s Span) Union(o Span) Span {
	start, end := s.Start, s.End
	if o.Start < start {
		start = o.Start
	}
	if o.End > end {
		end = o.End
	}
	return Span{Start: start, End: end}
}

// Clock anchors a track's time base on the presentation timeline.
type Clock struct {
	Track  string   `json:"track"`
	Base   TimeBase `json:"-"`
	Origin Micros   `json:"origin_us"`
}

// NewClock creates a clock for a track with an explicit time base.
func NewClock(track string, base TimeBase, origin Micros) (*Clock, error) {
	if track == "" {
		return nil, &core.Error{Code: core.CodeBadArgument, Detail: "empty track id", Fatal: true}
	}
	if !base.Valid() {
		return nil, &core.Error{Code: core.CodeBadTimestamp, Track: track, Detail: "unset time base", Fatal: true}
	}
	if origin < 0 {
		return nil, &core.Error{Code: core.CodeBadTimestamp, Track: track, Detail: "negative clock origin", Fatal: true}
	}
	base.Origin = origin
	return &Clock{Track: track, Base: base, Origin: origin}, nil
}

// Stamp converts track ticks into presentation microseconds.
func (c *Clock) Stamp(ticks int64) (Micros, error) {
	if c == nil {
		return 0, &core.Error{Code: core.CodeInternal, Detail: "nil clock", Fatal: true}
	}
	us, err := c.Base.ToMicros(ticks)
	if err != nil {
		return 0, core.StampTrack(err, c.Track)
	}
	return us, nil
}

// Span stamps a tick position and duration into a media interval.
func (c *Clock) Span(startTicks, durTicks int64) (Span, error) {
	if durTicks < 0 {
		return Span{}, &core.Error{Code: core.CodeBadTimestamp, Track: c.Track, Detail: "negative tick duration", Fatal: true}
	}
	start, err := c.Stamp(startTicks)
	if err != nil {
		return Span{}, err
	}
	end, err := c.Stamp(startTicks + durTicks)
	if err != nil {
		return Span{}, err
	}
	return Span{Start: start, End: end}, nil
}

// Offset returns signed microseconds from other's origin to c's origin.
func (c *Clock) Offset(other *Clock) (Micros, error) {
	if c == nil || other == nil {
		return 0, &core.Error{Code: core.CodeInternal, Detail: "nil clock in offset", Fatal: true}
	}
	if c.Base.Timescale == 0 || other.Base.Timescale == 0 {
		return 0, &core.Error{Code: core.CodeBadTimestamp, Detail: "clock without time base", Fatal: true}
	}
	return c.Origin - other.Origin, nil
}

// SyncDelta returns the absolute presentation offset between two clocks.
func SyncDelta(a, b *Clock) (Micros, error) {
	d, err := a.Offset(b)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		d = -d
	}
	return d, nil
}

// Abs returns the absolute value of a Micros.
func (m Micros) Abs() Micros {
	if m < 0 {
		return -m
	}
	return m
}

// IsErrorCode reports whether err carries the given mediamix code.
func IsErrorCode(err error, code string) bool {
	var e *core.Error
	return errors.As(err, &e) && e.Code == code
}
