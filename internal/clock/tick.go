// Package clock implements the shared 90 kHz rational time base used for
// PTS/DTS, sample clocks, cut points and playlist timelines. All cross-track
// comparisons happen in 90 kHz integer ticks so no floating point drift can
// accumulate.
package clock

import (
	"fmt"
	"math/big"

	"aircheck/internal/fault"
)

// Timescale is the canonical 90 kHz tick rate (MPEG PCR/PTS base).
const Timescale int64 = 90000

// Wrap33 is the 33-bit MPEG-TS timestamp modulus.
const Wrap33 int64 = 1 << 33

// Ticks is an instant or duration on the 90 kHz base.
type Ticks = int64

// FromRational converts value/timescale (e.g. PTS in stream time_base)
// into 90 kHz ticks with exact rational arithmetic. Overflow is reported
// as a fault carrying the offending track.
func FromRational(value, timescale int64, track string) (Ticks, error) {
	if timescale <= 0 {
		return 0, fault.New(fault.CodeRuntime, track, -1, false, "invalid timescale %d", timescale)
	}
	r := new(big.Rat).SetFrac64(value, timescale)
	r.Mul(r, new(big.Rat).SetInt64(Timescale))
	if !r.IsInt() {
		// Round to nearest tick; sub-tick precision is not representable.
		num, den := r.Num(), r.Denom()
		q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
		rem.Mul(rem, big.NewInt(2))
		if rem.CmpAbs(den) >= 0 {
			q.Add(q, big.NewInt(1))
		}
		if !q.IsInt64() {
			return 0, fault.New(fault.CodeOverflow, track, -1, false, "tick overflow for %d/%d", value, timescale)
		}
		return q.Int64(), nil
	}
	q := r.Num()
	if !q.IsInt64() {
		return 0, fault.New(fault.CodeOverflow, track, -1, false, "tick overflow for %d/%d", value, timescale)
	}
	return q.Int64(), nil
}

// ToRational converts 90 kHz ticks into the given timescale, rounding down.
func ToRational(t Ticks, timescale int64) int64 {
	r := new(big.Rat).SetFrac64(t, Timescale)
	r.Mul(r, new(big.Rat).SetInt64(timescale))
	q := new(big.Int).Quo(r.Num(), r.Denom())
	return q.Int64()
}

// Seconds renders ticks as fractional seconds with microsecond precision.
func Seconds(t Ticks) float64 { return float64(t) / float64(Timescale) }

// Millis renders ticks as fractional milliseconds.
func Millis(t Ticks) float64 { return float64(t) / 90.0 }

// FromMillis converts milliseconds to ticks (rounded).
func FromMillis(ms float64) Ticks { return Ticks(ms*90 + 0.5) }

// Unwrap33 resolves a 33-bit wrapped timestamp against its predecessor:
// it shifts raw by multiples of 2^33 so the result is the closest
// plausible successor of prev. A backward jump larger than half the wrap
// range is treated as wraparound; anything else is a real regression and
// is returned unchanged for the caller to flag.
func Unwrap33(prev, raw int64) int64 {
	const half = Wrap33 / 2
	v := raw
	for v-prev > half {
		v -= Wrap33
	}
	for prev-v > half {
		v += Wrap33
	}
	return v
}

// CheckDuration rejects negative durations with track and tick context.
func CheckDuration(d Ticks, track string) error {
	if d < 0 {
		return fault.New(fault.CodeNegativeDur, track, d, false, "negative duration %d ticks", d)
	}
	return nil
}

// CheckMonotonic verifies that cur does not regress relative to prev.
// Both values must already be unwrap-resolved. A regression reports the
// track and the offending tick.
func CheckMonotonic(prev, cur Ticks, track string) error {
	if cur < prev {
		return fault.New(fault.CodeBackwardPTS, track, cur, false,
			"timestamp regressed: prev=%d cur=%d", prev, cur)
	}
	return nil
}

// String formats ticks as HH:MM:SS.mmm for logs.
func String(t Ticks) string {
	if t < 0 {
		return "-" + String(-t)
	}
	ms := t * 1000 / Timescale
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}
