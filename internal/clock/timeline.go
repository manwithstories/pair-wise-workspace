package clock

import (
	"fmt"
	"math"
	"math/big"
)

// Timeline binds an integer tick timebase to a rational frame rate.
type Timeline struct {
	Tick Timebase
	FPS  Rational
}

// NewTimeline validates both rates. The frame rate must be positive.
func NewTimeline(tickRate, fps Rational) (Timeline, error) {
	tb, err := NewTimebase(tickRate)
	if err != nil {
		return Timeline{}, err
	}
	if !fps.validRate() {
		return Timeline{}, fmt.Errorf("%w: fps=%s", ErrInvalidRate, fps)
	}
	return Timeline{Tick: tb, FPS: fps}, nil
}

// FrameDurationTicks is the exact or rounded duration of one frame.
func (t Timeline) FrameDurationTicks(mode Rounding) (int64, error) {
	return Rescale(1, t.FPS, t.Tick.Rate, mode)
}

// FramesToTicks maps an integer frame index onto the tick axis.
func (t Timeline) FramesToTicks(frames int64, mode Rounding) (int64, error) {
	return Rescale(frames, t.FPS, t.Tick.Rate, mode)
}

// TicksToFrames maps ticks back onto the frame axis.
func (t Timeline) TicksToFrames(ticks int64, mode Rounding) (int64, error) {
	return Rescale(ticks, t.Tick.Rate, t.FPS, mode)
}

// SecondsToTicks converts an exact rational duration to ticks.
func (t Timeline) SecondsToTicks(seconds Rational, mode Rounding) (int64, error) {
	return Rescale(seconds.Num, seconds, t.Tick.Rate, mode)
}

// TicksToMillis is the reporting-only conversion. It keeps the fractional
// millisecond so percentiles are not distorted by per-sample rounding.
func (t Timeline) TicksToMillis(ticks int64) float64 {
	return float64(ticks) * 1000 * float64(t.Tick.Rate.Den) / float64(t.Tick.Rate.Num)
}

// MillisToTicks converts a reporting duration back onto the tick axis.
func (t Timeline) MillisToTicks(ms float64, mode Rounding) (int64, error) {
	num := new(big.Int).Mul(big.NewInt(int64(math.Round(ms))), big.NewInt(t.Tick.Rate.Num))
	den := new(big.Int).Mul(big.NewInt(1000), big.NewInt(t.Tick.Rate.Den))
	return bigToInt64(divRounded(num, den, mode))
}

// Interval is a half-open [Start, End) range on one tick axis.
type Interval struct {
	Start int64
	End   int64
}

// NewInterval validates a non-negative duration and overflow.
func NewInterval(start, dur int64) (Interval, error) {
	if dur < 0 {
		return Interval{}, fmt.Errorf("%w: start=%d dur=%d", ErrNegativeDuration, start, dur)
	}
	if start > math.MaxInt64-dur {
		return Interval{}, fmt.Errorf("%w: start=%d dur=%d", ErrOverflow, start, dur)
	}
	return Interval{Start: start, End: start + dur}, nil
}

// Duration returns End-Start for a valid interval.
func (i Interval) Duration() int64 {
	if i.End <= i.Start {
		return 0
	}
	return i.End - i.Start
}

// Valid reports a well-formed, non-negative range.
func (i Interval) Valid() bool { return i.End >= i.Start }

// Contains reports whether tick lies inside [Start, End).
func (i Interval) Contains(tick int64) bool { return tick >= i.Start && tick < i.End }

// Overlaps reports a non-empty intersection.
func (i Interval) Overlaps(o Interval) bool { return i.Start < o.End && o.Start < i.End }

// Intersect returns the overlap and whether it is non-empty.
func (i Interval) Intersect(o Interval) (Interval, bool) {
	start := max(i.Start, o.Start)
	end := min(i.End, o.End)
	if start >= end {
		return Interval{}, false
	}
	return Interval{Start: start, End: end}, true
}

// Union returns the smallest interval covering both.
func (i Interval) Union(o Interval) Interval {
	return Interval{Start: min(i.Start, o.Start), End: max(i.End, o.End)}
}

func (i Interval) String() string { return fmt.Sprintf("[%d,%d)", i.Start, i.End) }

// AddTicks adds with an explicit overflow check.
func AddTicks(a, b int64) (int64, error) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, fmt.Errorf("%w: %d+%d", ErrOverflow, a, b)
	}
	return a + b, nil
}
