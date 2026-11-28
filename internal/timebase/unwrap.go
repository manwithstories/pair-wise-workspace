package timebase

import "archive/internal/media"

// PTSWidth is the bit width of wrapping MPEG-style presentation
// timestamps.
const PTSWidth = 33

const ptsModulus = int64(1) << PTSWidth
const ptsHalf = ptsModulus / 2

// Unwrap33 expands a sequence of raw 33-bit PTS values into a monotonic
// int64 tick sequence. A wrap is only applied when the adjacent
// difference is decidable: a delta of exactly half the modulus is
// ambiguous and raises an unwrap-stage Fault.
func Unwrap33(raw []int64, track int) ([]int64, error) {
	out := make([]int64, len(raw))
	if len(raw) == 0 {
		return out, nil
	}
	for i, v := range raw {
		if v < 0 || v >= ptsModulus {
			return nil, media.Faultf(media.StageUnwrap, track, int64(i),
				"raw PTS %d outside 33-bit range", v)
		}
	}
	out[0] = raw[0]
	for i := 1; i < len(raw); i++ {
		d := (raw[i] - raw[i-1]) & (ptsModulus - 1)
		if d == ptsHalf {
			return nil, media.Faultf(media.StageUnwrap, track, int64(i),
				"ambiguous PTS delta of exactly 2^%d between packets %d and %d",
				PTSWidth-1, i-1, i)
		}
		if d > ptsHalf {
			d -= ptsModulus
		}
		out[i] = out[i-1] + d
	}
	return out, nil
}
