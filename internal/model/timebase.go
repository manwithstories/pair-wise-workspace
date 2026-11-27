// Package model defines the shared time base, rational arithmetic and the
// unified fault/error model used by every AVPK pipeline subsystem.
//
// All timestamps on the timeline are expressed as integer nanoseconds.
// Stream-local packet time stamps (PTS) are converted with the per-track
// rational time base (Num/Den seconds per tick).
package model

import "math/big"

// NanosPerSecond is the unified timeline resolution.
const NanosPerSecond int64 = 1_000_000_000

// Rational expresses Num/Den seconds per source tick.
type Rational struct {
	Num int64
	Den int64
}

// Valid reports whether the time base is strictly positive and usable.
func (r Rational) Valid() bool { return r.Num > 0 && r.Den > 0 }

// ToNanoseconds converts a (possibly negative) tick count to nanoseconds.
// The intermediate product is evaluated with arbitrary precision so that
// dense 10k-packet streams cannot overflow int64 during scaling.
func ToNanoseconds(pts int64, tb Rational) int64 {
	if !tb.Valid() {
		return 0
	}
	v := new(big.Int).SetInt64(pts)
	v.Mul(v, big.NewInt(tb.Num))
	v.Mul(v, big.NewInt(NanosPerSecond))
	v.Quo(v, big.NewInt(tb.Den))
	return v.Int64()
}

// DurationNanoseconds converts a tick-span duration to nanoseconds.
func DurationNanoseconds(duration uint64, tb Rational) int64 {
	if !tb.Valid() || duration == 0 {
		return 0
	}
	v := new(big.Int).SetUint64(duration)
	v.Mul(v, big.NewInt(tb.Num))
	v.Mul(v, big.NewInt(NanosPerSecond))
	v.Quo(v, big.NewInt(tb.Den))
	return v.Int64()
}

// SampleAligned reports whether ptsNS lands exactly on an audio sample grid
// for the given sample rate. The rational test (ptsNS*rate mod 1e9 == 0)
// stays exact for rates that do not divide 1e9 (e.g. 44100 Hz).
func SampleAligned(ptsNS int64, sampleRate uint32) bool {
	if sampleRate == 0 {
		return false
	}
	if ptsNS < 0 {
		ptsNS = -ptsNS
	}
	// (ptsNS * rate) mod 1e9; split to avoid overflow on 32-bit platforms.
	hi, lo := mul64(uint64(ptsNS), uint64(sampleRate))
	_ = hi
	return lo%uint64(NanosPerSecond) == 0 && hi%uint64(NanosPerSecond) == 0
}

// SampleOffsetNS returns the nanosecond distance from ptsNS to the previous
// audio sample grid point (0 when perfectly aligned).
func SampleOffsetNS(ptsNS int64, sampleRate uint32) int64 {
	if sampleRate == 0 || ptsNS < 0 {
		return 0
	}
	prod := new(big.Int).Mul(big.NewInt(ptsNS), big.NewInt(int64(sampleRate)))
	rem := new(big.Int).Mod(prod, big.NewInt(NanosPerSecond))
	// offset = rem / rate, rounded toward zero in nanoseconds.
	rem.Quo(rem, big.NewInt(int64(sampleRate)))
	return rem.Int64()
}

// FrameGridOffsetNS returns the offset of ptsNS from a video frame grid
// described by frames-per-second as a rational number.
func FrameGridOffsetNS(ptsNS int64, fps Rational) int64 {
	if !fps.Valid() || ptsNS < 0 {
		return 0
	}
	// Frame period in ns is Nanos*Den/Num; grid alignment uses the rational
	// identity ptsNS*Num mod (Nanos*Den) == 0.
	period := new(big.Int).Mul(big.NewInt(ptsNS), big.NewInt(fps.Num))
	grid := new(big.Int).Mul(big.NewInt(NanosPerSecond), big.NewInt(fps.Den))
	rem := new(big.Int).Mod(period, grid)
	// Convert remainder ticks back to nanoseconds: rem / Num.
	rem.Quo(rem, big.NewInt(fps.Num))
	return rem.Int64()
}

// ResamplePosition is a drift-free fractional resampler cursor. Each output
// sample consumes inRate/outRate of an input sample; the fractional part is
// kept as an integer accumulator, so no floating-point error accumulates.
type ResamplePosition struct {
	inRate uint64
	outRate uint64
	acc    uint64
	index  uint64
}

// NewResamplePosition builds a cursor mapping outRate back onto inRate.
func NewResamplePosition(inRate, outRate uint32) ResamplePosition {
	return ResamplePosition{inRate: uint64(inRate), outRate: uint64(outRate)}
}

// Advance returns the input sample index for the next output sample.
func (p *ResamplePosition) Advance() uint64 {
	p.acc += p.inRate
	step := p.acc / p.outRate
	p.acc %= p.outRate
	p.index += step
	return p.index
}

// Index returns the cursor's current input sample index.
func (p *ResamplePosition) Index() uint64 { return p.index }

// InRate returns the cursor's input sample rate.
func (p *ResamplePosition) InRate() uint64 { return p.inRate }

// OutRate returns the cursor's output sample rate.
func (p *ResamplePosition) OutRate() uint64 { return p.outRate }

func mul64(a, b uint64) (hi, lo uint64) {
	var mask uint64 = 1<<32 - 1
	aLo, aHi := a&mask, a>>32
	bLo, bHi := b&mask, b>>32
	lo = aLo * bLo
	mid := aHi*bLo + (lo>>32)
	lo = (lo & mask) | ((mid & mask) << 32)
	hi = aHi*bHi + (mid >> 32) + aLo*bHi
	return hi, lo
}
