// Package clock provides the unified integer timebase shared by all
// remuxed tracks, plus wrap-around correction for 32-bit media clocks.
package clock

import "archive/internal/errs"

// CommonTimescale is the integer timebase every track is rescaled to
// during remuxing (90 kHz, matching MPEG-TS and RTP video clocks).
const CommonTimescale = 90000

// wrap32 is the modulus of a 32-bit timestamp clock.
const wrap32 = int64(1) << 32

// halfWrap is half of the 32-bit clock modulus: the maximum forward jump
// that is still interpreted as progress rather than a wrap.
const halfWrap = int64(1) << 31

// Rescale converts v from a source timescale to a destination timescale,
// rounding to the nearest tick. Zero timescales are rejected.
func Rescale(v int64, src, dst uint32) (int64, error) {
	if src == 0 || dst == 0 {
		return 0, errs.New(errs.KindClock, "", "rescale", "zero timescale", nil)
	}
	r := v * int64(dst)
	s := int64(src)
	if r >= 0 {
		return (r + s/2) / s, nil
	}
	return -((-r + s/2) / s), nil
}

// ToMilliseconds converts common-timebase ticks to milliseconds.
func ToMilliseconds(ticks int64) float64 {
	return float64(ticks) * 1000 / CommonTimescale
}

// Unwrapper extends a wrapped 32-bit timestamp sequence to monotonic
// 64-bit values. Forward wraps (the raw value drops by ~2^32) are
// corrected; backward jumps that cannot be explained by wrap-around are
// rejected so the caller can refuse the track.
type Unwrapper struct {
	cycles int64 // accumulated multiples of 2^32
	prev   int64 // last emitted extended value
	init   bool
}

// Unwrap extends one raw 32-bit timestamp. It returns a KindClock error
// when the sequence cannot be kept monotonic.
func (u *Unwrapper) Unwrap(v uint32) (int64, error) {
	ext := u.cycles + int64(v)
	if u.init {
		if ext+halfWrap < u.prev {
			// forward wrap: raw clock crossed 2^32
			u.cycles += wrap32
			ext += wrap32
		} else if ext < u.prev {
			return 0, errs.Newf(errs.KindClock, "", "unwrap",
				"non-monotonic timestamp: %d after %d cannot be recovered", ext, u.prev)
		}
	}
	u.init = true
	u.prev = ext
	return ext, nil
}

// UnwrapSequence extends a whole sequence of raw 32-bit timestamps.
func UnwrapSequence(vals []uint32) ([]int64, error) {
	out := make([]int64, len(vals))
	var u Unwrapper
	for i, v := range vals {
		ext, err := u.Unwrap(v)
		if err != nil {
			return nil, err
		}
		out[i] = ext
	}
	return out, nil
}
