package media

import "math/big"

// Clock is a rational ticks-per-second time base shared by stages. Timestamps
// are integer tick counts converted through the rational rate, so no floating
// point accumulation can drift.
type Clock struct {
	rate int64
}

// NewClock builds a clock at ticksPerSecond; a zero/negative rate is rejected.
func NewClock(ticksPerSecond int64) (*Clock, *Fault) {
	if ticksPerSecond == 0 {
		return nil, NewFault(CodeInvalidTimebase, "time base rate must not be zero")
	}
	if ticksPerSecond < 0 {
		return nil, NewFault(CodeInvalidTimebase, "time base rate must be positive, got %d", ticksPerSecond)
	}
	return &Clock{rate: ticksPerSecond}, nil
}

// Rate returns ticks per second.
func (c *Clock) Rate() int64 { return c.rate }

// TickDuration returns the exact duration of one tick in seconds.
func (c *Clock) TickDuration() Rational { return MustRat(1, c.rate) }

// PTS converts a tick count to exact rational seconds (ticks/rate).
func (c *Clock) PTS(ticks int64) (Rational, *Fault) {
	return Rat(ticks, c.rate)
}

// Range builds and validates a [startTicks,endTicks) interval. A backwards
// interval (end < start) is rejected.
func (c *Clock) Range(startTicks, endTicks int64) (TimeRange, *Fault) {
	start, f := c.PTS(startTicks)
	if f != nil {
		return TimeRange{}, f
	}
	end, f := c.PTS(endTicks)
	if f != nil {
		return TimeRange{}, f
	}
	if end.Cmp(start) < 0 {
		return TimeRange{}, NewFault(CodeBackwardRange, "end %s before start %s", end, start)
	}
	return TimeRange{Start: start, End: end}, nil
}

// NewTimeRange validates an exact rational interval.
func NewTimeRange(start, end Rational) (TimeRange, *Fault) {
	if end.Cmp(start) < 0 {
		return TimeRange{}, NewFault(CodeBackwardRange, "end %s before start %s", end, start)
	}
	return TimeRange{Start: start.norm(), End: end.norm()}, nil
}

// Duration returns End-Start.
func (r TimeRange) Duration() (Rational, *Fault) {
	return r.End.Sub(r.Start)
}

// RescaleTicks converts ticks from fromRate to toRate using exact arithmetic.
// When exact is true the conversion must be integral; otherwise floor is used.
// The result fitting in int64 is enforced, rejecting overflow.
func RescaleTicks(ticks, fromRate, toRate int64, exact bool) (int64, *Fault) {
	if fromRate <= 0 || toRate <= 0 {
		return 0, NewFault(CodeInvalidTimebase, "rates must be positive: from=%d to=%d", fromRate, toRate)
	}
	num := new(big.Int).Mul(big.NewInt(ticks), big.NewInt(toRate))
	q, rem := new(big.Int).QuoRem(num, big.NewInt(fromRate), new(big.Int))
	if exact && rem.Sign() != 0 {
		return 0, NewFault(CodeInvalidInput, "rescale %d from %d to %d is not exact", ticks, fromRate, toRate)
	}
	if !q.IsInt64() {
		return 0, NewFault(CodeOverflow, "rescaled tick count overflows int64")
	}
	return q.Int64(), nil
}
