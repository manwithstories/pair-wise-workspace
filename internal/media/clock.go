// Package media holds the shared rational time base and the structured error
// model used by every stage of the audio pipeline.
package media

import (
	"fmt"
	"math"
	"math/big"
	"time"
)

// Rate is an exact number of sample frames per second. Storing the ratio as
// integers keeps every conversion reproducible; no block boundary is ever
// derived from a floating point accumulation.
type Rate struct {
	Num int64
	Den int64
}

// NewRate validates and constructs a sample rate.
func NewRate(num, den int64) (Rate, error) {
	r := Rate{Num: num, Den: den}
	if !r.Valid() {
		return Rate{}, fmt.Errorf("invalid sample rate %d/%d", num, den)
	}
	return r, nil
}

// Valid reports whether the rate is a positive rational number.
func (r Rate) Valid() bool { return r.Num > 0 && r.Den > 0 }

// Float returns the rate as a float. It is only used for filter design and
// reporting, never for timestamp arithmetic.
func (r Rate) Float() float64 { return float64(r.Num) / float64(r.Den) }

// String renders the exact rate.
func (r Rate) String() string { return fmt.Sprintf("%d/%d", r.Num, r.Den) }

// Clock converts between durations and frame counts at one exact rate.
type Clock struct {
	Rate Rate
}

// NewClock constructs a clock and rejects invalid rates.
func NewClock(rate Rate) (Clock, error) {
	if !rate.Valid() {
		return Clock{}, fmt.Errorf("invalid clock rate %s", rate.String())
	}
	return Clock{Rate: rate}, nil
}

// Frames converts a duration to a frame count, rounding to nearest.
func (c Clock) Frames(d time.Duration) int64 {
	if !c.Rate.Valid() {
		return 0
	}
	return mulDivRound([]int64{d.Nanoseconds(), c.Rate.Num}, []int64{1_000_000_000, c.Rate.Den})
}

// Duration converts a frame count to a duration, rounding to nearest.
func (c Clock) Duration(frames int64) time.Duration {
	if !c.Rate.Valid() {
		return 0
	}
	return time.Duration(mulDivRound(
		[]int64{frames, c.Rate.Den, 1_000_000_000},
		[]int64{c.Rate.Num},
	))
}

// Convert maps a frame count from the clock rate to dst using one exact
// rational division. Callers pass cumulative positions, so block boundaries
// cannot drift from per-block rounding.
func (c Clock) Convert(frames int64, dst Rate) (int64, error) {
	if !c.Rate.Valid() || !dst.Valid() {
		return 0, fmt.Errorf("invalid conversion %s -> %s", c.Rate.String(), dst.String())
	}
	return mulDivRound(
		[]int64{frames, dst.Num, c.Rate.Den},
		[]int64{dst.Den, c.Rate.Num},
	), nil
}

// Millis converts a frame count to whole milliseconds, rounding to nearest.
func (c Clock) Millis(frames int64) int64 {
	if !c.Rate.Valid() {
		return 0
	}
	return mulDivRound([]int64{frames, 1000, c.Rate.Den}, []int64{c.Rate.Num})
}

// Block is an interleaved audio block together with its exact timeline
// position. Samples are normalized to [-1, 1] until the final PCM16 step.
type Block struct {
	Track    string
	Seq      uint64
	Start    int64
	Rate     Rate
	Channels int
	Samples  []float64
}

// Frames returns the number of interleaved frames in the block.
func (b Block) Frames() int {
	if b.Channels <= 0 {
		return 0
	}
	return len(b.Samples) / b.Channels
}

// Validate checks the block invariants and the per-block frame limit.
func (b Block) Validate(maxFrames int) error {
	if b.Channels <= 0 {
		return NewError(PhaseConfig, ExitUsage, b.Track, b.Seq, false, "block",
			fmt.Errorf("channels must be positive, got %d", b.Channels))
	}
	if !b.Rate.Valid() {
		return NewError(PhaseConfig, ExitUsage, b.Track, b.Seq, false, "block",
			fmt.Errorf("invalid rate %s", b.Rate.String()))
	}
	if b.Start < 0 {
		return NewError(PhaseInput, ExitInput, b.Track, b.Seq, false, "block",
			fmt.Errorf("negative start frame %d", b.Start))
	}
	if len(b.Samples)%b.Channels != 0 {
		return NewError(PhaseInput, ExitInput, b.Track, b.Seq, false, "block",
			fmt.Errorf("sample count %d not divisible by %d channels", len(b.Samples), b.Channels))
	}
	frames := b.Frames()
	if maxFrames > 0 && frames > maxFrames {
		return NewError(PhaseInput, ExitInput, b.Track, b.Seq, false, "block",
			fmt.Errorf("block frames %d exceed limit %d", frames, maxFrames))
	}
	return nil
}

// mulDivRound computes num[0]*num[1]*.../den[0]/den[1]... exactly and rounds
// halves away from zero. big.Int keeps the arithmetic safe for adversarial
// rates; the result is clamped when it does not fit an int64.
func mulDivRound(num, den []int64) int64 {
	p := big.NewInt(1)
	for _, v := range num {
		p.Mul(p, big.NewInt(v))
	}
	q := big.NewInt(1)
	for _, v := range den {
		q.Mul(q, big.NewInt(v))
	}
	if q.Sign() == 0 {
		return 0
	}
	quo, rem := new(big.Int), new(big.Int)
	quo.QuoRem(p, q, rem)
	twice := new(big.Int).Lsh(new(big.Int).Abs(rem), 1)
	if twice.Cmp(new(big.Int).Abs(q)) >= 0 {
		if p.Sign() < 0 {
			quo.Sub(quo, big.NewInt(1))
		} else {
			quo.Add(quo, big.NewInt(1))
		}
	}
	if !quo.IsInt64() {
		if quo.Sign() < 0 {
			return math.MinInt64
		}
		return math.MaxInt64
	}
	return quo.Int64()
}
