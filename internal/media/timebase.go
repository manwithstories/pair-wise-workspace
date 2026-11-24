// Package media contains the shared rational time base and error model used by
// scanning, clock repair, indexing, metrics and transaction recovery.
package media

import (
	"fmt"
	"math"
	"math/big"
	"time"
)

// SharedTimescale is the broadcast common time base: 90 kHz ticks.  Using a
// rational keeps conversions from 24/25/30 kHz, 48 kHz and 90 kHz tracks
// explicit and testable.
const SharedTimescale int64 = 90_000

// Rational is num/den in reduced form.  It describes either a tick duration or
// a conversion ratio depending on the field that uses it.
type Rational struct {
	Num int64
	Den int64
}

// Timebase returns the canonical shared time base, one tick at 1/90000 second.
func Timebase() Rational { return Rational{Num: 1, Den: SharedTimescale} }

// TrackTimebase returns the rational tick duration for an MP4 timescale.
func TrackTimebase(timescale uint32) Rational {
	return Rational{Num: 1, Den: int64(timescale)}
}

// Float64 returns the rational as float64.
func (r Rational) Float64() float64 { return float64(r.Num) / float64(r.Den) }

// Seconds converts a tick count in this rational to seconds.
func (r Rational) Seconds(ticks int64) float64 {
	return float64(ticks) * r.Float64()
}

// Milliseconds converts ticks in this rational to milliseconds.
func (r Rational) Milliseconds(ticks int64) float64 {
	return r.Seconds(ticks) * 1000
}

// Duration converts ticks to time.Duration.  The result is saturated rather
// than wrapped when it exceeds duration range.
func (r Rational) Duration(ticks int64) time.Duration {
	secs := r.Seconds(ticks)
	if secs > float64(math.MaxInt64)/float64(time.Second) {
		return time.Duration(math.MaxInt64)
	}
	if secs < float64(math.MinInt64)/float64(time.Second) {
		return time.Duration(math.MinInt64)
	}
	return time.Duration(secs * float64(time.Second))
}

// MulChecked returns a*b and an error if the signed product overflows int64.
func MulChecked(a, b int64) (int64, error) {
	var product big.Int
	product.Mul(big.NewInt(a), big.NewInt(b))
	if !product.IsInt64() {
		return 0, fmt.Errorf("integer overflow: %d * %d", a, b)
	}
	return product.Int64(), nil
}

// ConvertTicks converts ticks from one rational time base to another while
// checking multiplication overflow.  Rounding is to nearest.
func ConvertTicks(v int64, from, to Rational) (int64, error) {
	if from.Den <= 0 || to.Num <= 0 || to.Den <= 0 {
		return 0, fmt.Errorf("invalid rational conversion %v -> %v", from, to)
	}
	// v seconds = v * from.Num / from.Den.  Target ticks = seconds * to.Den / to.Num.
	num, err := MulChecked(v, from.Num)
	if err != nil {
		return 0, err
	}
	num, err = MulChecked(num, to.Den)
	if err != nil {
		return 0, err
	}
	den, err := MulChecked(from.Den, to.Num)
	if err != nil {
		return 0, err
	}
	if den == 0 {
		return 0, fmt.Errorf("zero denominator")
	}
	q := num / den
	rem := num % den
	if rem != 0 {
		// Round half away from zero.
		if rem > 0 && rem*2 >= den {
			q++
		} else if rem < 0 && (-rem)*2 >= den {
			q--
		}
	}
	return q, nil
}

// SampleTiming is the timing information retained from one trun sample entry.
type SampleTiming struct {
	Index             int    `json:"index"`
	Duration          uint32 `json:"duration"`
	Size              uint32 `json:"size"`
	Flags             uint32 `json:"flags"`
	CompositionOffset int64  `json:"composition_offset"`
	Sync              bool   `json:"sync"`
	PayloadOffset     int64  `json:"payload_offset"`
}

// MaxBatchMemory is the deliberate whole-batch metadata ceiling.  Media
// payloads are streamed and are not charged as resident memory.
const MaxBatchMemory = 256 << 20
