// Package timeline supplies overflow-checked integer time bases and the
// deterministic dependency graph used by recovery and verification.
package timeline

import (
	"fmt"
	"math"
	"math/big"
)

// Nanoseconds is the common cross-track integer time base. Keeping the base
// fixed avoids floating-point comparisons in manifests and gap journals.
const Nanoseconds uint32 = 1_000_000_000

// Timebase describes one media clock. Start is the non-zero raw origin; it is
// subtracted only when producing normalized cross-track time.
type Timebase struct {
	Timescale uint32
	Start     int64
}

func (tb Timebase) validate() error {
	if tb.Timescale == 0 {
		return New(CodeInvalidArgument, "timebase", "timescale must be non-zero")
	}
	return nil
}

// Normalize converts a raw tick to nanoseconds relative to tb.Start.
func (tb Timebase) Normalize(raw int64) (int64, error) {
	if err := tb.validate(); err != nil {
		return 0, err
	}
	delta, err := checkedSub(raw, tb.Start)
	if err != nil {
		return 0, Wrap(CodeOverflow, "normalize clock", err)
	}
	return ToNanoseconds(delta, tb.Timescale)
}

// ToNanoseconds performs an overflow-checked rational conversion. Truncation
// is toward zero and is less than one nanosecond for valid integer ticks.
func ToNanoseconds(ticks int64, timescale uint32) (int64, error) {
	if timescale == 0 {
		return 0, New(CodeInvalidArgument, "to nanoseconds", "timescale is zero")
	}
	n := new(big.Int).Mul(big.NewInt(ticks), big.NewInt(int64(Nanoseconds)))
	d := big.NewInt(int64(timescale))
	q := new(big.Int).Quo(n, d)
	if !q.IsInt64() {
		return 0, New(CodeOverflow, "to nanoseconds",
			fmt.Sprintf("ticks=%d timescale=%d exceeds int64", ticks, timescale))
	}
	return q.Int64(), nil
}

// ConvertTicks converts between two integer timescales without floating point.
func ConvertTicks(ticks int64, from, to uint32) (int64, error) {
	if from == 0 || to == 0 {
		return 0, New(CodeInvalidArgument, "convert ticks", "timescale is zero")
	}
	n := new(big.Int).Mul(big.NewInt(ticks), big.NewInt(int64(to)))
	q := new(big.Int).Quo(n, big.NewInt(int64(from)))
	if !q.IsInt64() {
		return 0, New(CodeOverflow, "convert ticks",
			fmt.Sprintf("ticks=%d %d->%d exceeds int64", ticks, from, to))
	}
	return q.Int64(), nil
}

func checkedSub(a, b int64) (int64, error) {
	if (b > 0 && a < math.MinInt64+b) || (b < 0 && a > math.MaxInt64+b) {
		return 0, fmt.Errorf("subtraction overflow: %d-%d", a, b)
	}
	return a - b, nil
}

// Extend16 turns a wrapping RTP 16-bit sequence number into a monotonic
// integer. prev is the previously observed extended maximum.
func Extend16(prev int64, raw uint16) int64 {
	if prev < 0 {
		return int64(raw)
	}
	prevRaw := uint16(uint64(prev) & 0xffff)
	delta := int16(raw - prevRaw)
	return prev + int64(delta)
}

// Extend32 performs sign-aware modular extension for wrapping 32-bit sequence
// numbers or RTP timestamps.
func Extend32(prev int64, raw uint32) int64 {
	if prev < 0 {
		return int64(raw)
	}
	prevRaw := uint32(uint64(prev) & 0xffffffff)
	delta := int32(raw - prevRaw)
	return prev + int64(delta)
}

// WrapState tracks the largest extended value observed on a wrapping clock.
type WrapState struct {
	Max      int64
	Has      bool
	Bits     int
	lastRaw  uint64
	extended int64
}

// Extend accepts a raw 16- or 32-bit value and returns its nearest extended
// representation relative to the high-water mark.
func (w *WrapState) Extend(raw uint64) (int64, error) {
	if w.Bits != 16 && w.Bits != 32 {
		return 0, New(CodeInvalidArgument, "wrap extend", "Bits must be 16 or 32")
	}
	mask := uint64(1<<w.Bits) - 1
	raw &= mask
	if !w.Has {
		w.Has = true
		w.Max = int64(raw)
		w.lastRaw = raw
		w.extended = int64(raw)
		return w.extended, nil
	}
	var ext int64
	if w.Bits == 16 {
		ext = Extend16(w.Max, uint16(raw))
	} else {
		ext = Extend32(w.Max, uint32(raw))
	}
	w.lastRaw = raw
	w.extended = ext
	if ext > w.Max {
		w.Max = ext
	}
	return ext, nil
}

// CheckDTSMonotonic rejects decreasing decode timestamps. Equal durations are
// permitted because some elementary streams can omit a duration on a sample.
func CheckDTSMonotonic(trackID uint32, dts []int64) error {
	for i := 1; i < len(dts); i++ {
		if dts[i] < dts[i-1] {
			return &Error{Code: CodeInvalidArgument, Op: "dts monotonicity", TrackID: trackID,
				Detail: fmt.Sprintf("sample %d dts=%d follows %d", i, dts[i], dts[i-1]), Offset: -1}
		}
	}
	return nil
}
