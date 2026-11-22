// Package clock defines the single media time base used by every subsystem.
//
// Manifests, fragment probes, keyframe indexes, audio processing and
// checkpoints all exchange tick values in the same canonical time base.
// Native representation time bases are retained alongside canonical values so
// that HLS/DASH round trips remain exact.
package clock

import (
	"errors"
	"fmt"
	"math/big"
	"time"
)

// DefaultTimescale is the canonical time base: 90 kHz, the MPEG-TS clock.
const DefaultTimescale uint32 = 90000

const maxTimescale uint32 = 1_000_000_000

// ErrInvalidTimebase is returned for zero or absurdly large time bases.
var ErrInvalidTimebase = errors.New("clock: invalid timescale")

// Timebase converts between seconds, durations and integer ticks.
type Timebase struct {
	Timescale uint32
}

// New validates and returns a Timebase.
func New(timescale uint32) (Timebase, error) {
	if timescale == 0 || timescale > maxTimescale {
		return Timebase{}, fmt.Errorf("%w: %d", ErrInvalidTimebase, timescale)
	}
	return Timebase{Timescale: timescale}, nil
}

// MustNew is New for constants and tests; it panics on an invalid rate.
func MustNew(timescale uint32) Timebase {
	tb, err := New(timescale)
	if err != nil {
		panic(err)
	}
	return tb
}

// Valid reports whether tb can be used for conversions.
func (tb Timebase) Valid() bool { return tb.Timescale > 0 && tb.Timescale <= maxTimescale }

// Default returns the canonical 90 kHz time base.
func Default() Timebase { return Timebase{Timescale: DefaultTimescale} }

// TicksFromDuration converts a duration, rounding half away from zero.
func (tb Timebase) TicksFromDuration(d time.Duration) int64 {
	if !tb.Valid() {
		return 0
	}
	// duration is nanoseconds; 1e9 ticks per second.
	ticks := d.Nanoseconds() * int64(tb.Timescale)
	return divRound(ticks, 1_000_000_000)
}

// TicksFromSeconds converts whole seconds.
func (tb Timebase) TicksFromSeconds(sec int64) int64 {
	if !tb.Valid() {
		return 0
	}
	return sec * int64(tb.Timescale)
}

// Duration converts ticks to time.Duration, rounding to the nearest nanosecond.
func (tb Timebase) Duration(ticks int64) time.Duration {
	if !tb.Valid() {
		return 0
	}
	nsec := divRound(ticks*1_000_000_000, int64(tb.Timescale))
	return time.Duration(nsec)
}

// Seconds returns the tick interval as fractional seconds.
func (tb Timebase) Seconds(ticks int64) float64 {
	if !tb.Valid() {
		return 0
	}
	return float64(ticks) / float64(tb.Timescale)
}

// Convert re-bases ticks into to, rounding half away from zero.
// Multiplication is performed with big.Int so large tick values cannot wrap.
func (tb Timebase) Convert(ticks int64, to Timebase) int64 {
	if !tb.Valid() || !to.Valid() {
		return 0
	}
	if tb.Timescale == to.Timescale {
		return ticks
	}
	num := new(big.Int).Mul(big.NewInt(ticks), big.NewInt(int64(to.Timescale)))
	den := big.NewInt(int64(tb.Timescale))
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	// Round half away from zero: compare 2*|r| with |den|.
	twiceR := new(big.Int).Abs(r)
	twiceR.Lsh(twiceR, 1)
	if twiceR.Cmp(new(big.Int).Abs(den)) >= 0 {
		if num.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	if !q.IsInt64() {
		return 0
	}
	return q.Int64()
}

// End returns start+duration in this time base.
func (tb Timebase) End(start, duration int64) int64 { return start + duration }

// divRound divides n by d with half-away-from-zero rounding.
func divRound(n, d int64) int64 {
	if d == 0 {
		return 0
	}
	if (n < 0) != (d < 0) {
		return (n - d/2) / d
	}
	return (n + d/2) / d
}
