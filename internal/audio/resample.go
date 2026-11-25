// Package audio implements the PCM transformation chain: rational
// resampling, channel-layout downmix and bounded loudness normalization.
package audio

import (
	"avpipe/internal/media"
)

// Resampler converts PCM streams between sample rates with a fractional
// accumulator. Output sample n is taken from input position
// (n * inRate / outRate); the residual accumulator guarantees the long-run
// sample-count error over any stream length never exceeds one output sample.
type Resampler struct {
	inRate  uint64
	outRate uint64
	// produced counts total output samples (per channel) since creation.
	produced uint64
	// consumed counts total input samples (per channel) since creation.
	consumed uint64
}

// NewResampler validates the rates and builds a resampler.
func NewResampler(inRate, outRate uint32) (*Resampler, error) {
	if inRate == 0 || outRate == 0 {
		return nil, media.NewError("audio", "bad_rate", 0, media.ActionFail,
			"rates must be non-zero: in=%d out=%d", inRate, outRate)
	}
	return &Resampler{inRate: uint64(inRate), outRate: uint64(outRate)}, nil
}

// Process converts one interleaved PCM block. The block must contain a whole
// number of frames (sample count divisible by channels); each channel is
// resampled independently and the output stays interleaved.
func (r *Resampler) Process(in []int16, channels int) []int16 {
	if channels <= 0 || len(in) == 0 {
		return nil
	}
	if r.inRate == r.outRate {
		out := make([]int16, len(in))
		copy(out, in)
		r.produced += uint64(len(in) / channels)
		r.consumed += uint64(len(in) / channels)
		return out
	}
	framesIn := uint64(len(in) / channels)
	framesOut := r.outputFramesFor(framesIn)
	out := make([]int16, framesOut*uint64(channels))

	for of := uint64(0); of < framesOut; of++ {
		// Absolute input position of this output sample.
		posNum := (r.produced + of + 1) * r.inRate
		needIn := posNum / r.outRate
		idx := needIn - 1 - r.consumed
		frac := posNum % r.outRate
		for c := 0; c < channels; c++ {
			cur := in[int(idx)*channels+c]
			if frac == 0 || idx == 0 {
				out[int(of)*channels+c] = cur
				continue
			}
			prev := in[int(idx-1)*channels+c]
			out[int(of)*channels+c] = lerpInt16(prev, cur, frac, r.outRate)
		}
	}
	r.produced += framesOut
	r.consumed += framesIn
	return out
}

// outputFramesFor returns how many output frames the next input segment adds,
// using the exact rate ratio so the running total drifts by at most one.
func (r *Resampler) outputFramesFor(framesIn uint64) uint64 {
	totalIn := r.consumed + framesIn
	totalOut := totalIn * r.outRate / r.inRate
	return totalOut - r.produced
}

// lerpInt16 linearly interpolates between a and b; frac/den is the position.
func lerpInt16(a, b int16, frac, den uint64) int16 {
	af := float64(a)
	bf := float64(b)
	return clampInt16(af + (bf-af)*float64(frac)/float64(den))
}

func clampInt16(v float64) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}

// ExpectedOutputFrames is the exact integer frame count for an input length.
func ExpectedOutputFrames(framesIn, inRate, outRate uint64) uint64 {
	return framesIn * outRate / inRate
}
