// Package audio normalizes self-generated PCM media to a negotiated
// specification: cross-block resampling with preserved filter state, channel
// layout normalization and windowed loudness limiting.
//
// All audio data is mono float64 PCM inside the resampler; interleaved
// multi-channel frames are handled by Layout. Every edge case (empty block,
// short block, incomplete frame, sub-window tail) has an explicit, tested
// behavior instead of silently dropping samples.
package audio

import (
	"math"

	"mediapipeline/internal/mediaerr"
)

// Resampler converts a mono PCM stream between two sample rates while keeping
// interpolation state across input blocks. Linear interpolation is
// deterministic and needs no allocation tables, which keeps fixed-seed
// output reproducible.
//
// Output sample n maps to fractional input position t(n) = n*inRate/outRate.
// Process emits every output whose position lies inside the current block;
// positions at or beyond the block end are deferred because interpolating
// them needs samples from a future block. Flush closes the stream by emitting
// one endpoint-clamped tail sample when the output grid never reaches the
// final input sample.
type Resampler struct {
	inRate  int
	outRate int
	// inCount is the global index one past the last consumed input sample.
	inCount int64
	// outCount is the number of output samples already emitted.
	outCount int64
	// lastSample / hasLast retain the final sample of the previous block so
	// an output position landing before the current block origin
	// interpolates across the block boundary.
	lastSample float64
	hasLast    bool
	flushed    bool
}

// NewResampler validates both rates and builds a stateful resampler.
func NewResampler(inRate, outRate int) (*Resampler, *mediaerr.Error) {
	if inRate <= 0 || outRate <= 0 {
		return nil, mediaerr.New(mediaerr.StageResample, mediaerr.KindInvalidInput,
			"sample rates must be positive")
	}
	return &Resampler{inRate: inRate, outRate: outRate}, nil
}

// inputTime maps the n-th output sample to its fractional input position.
func (r *Resampler) inputTime(n int64) float64 {
	return float64(n) * float64(r.inRate) / float64(r.outRate)
}

// Process converts one input block. An empty block is a defined no-op: it
// returns an empty slice and leaves all state untouched. Samples that need a
// value before the stream start reuse the first sample (edge clamping).
func (r *Resampler) Process(block []float64) ([]float64, *mediaerr.Error) {
	if r.flushed {
		return nil, mediaerr.New(mediaerr.StageResample, mediaerr.KindInvalidInput,
			"resampler used after Flush; call Reset to reuse")
	}
	if len(block) == 0 {
		return []float64{}, nil
	}
	if r.inRate == r.outRate {
		out := make([]float64, len(block))
		copy(out, block)
		r.inCount += int64(len(block))
		r.outCount += int64(len(block))
		r.lastSample, r.hasLast = block[len(block)-1], true
		return out, nil
	}
	blockEnd := r.inCount + int64(len(block))
	var out []float64
	for {
		t := r.inputTime(r.outCount)
		if t >= float64(blockEnd) {
			break
		}
		out = append(out, r.interpolate(t, block))
		r.outCount++
	}
	r.inCount = blockEnd
	r.lastSample, r.hasLast = block[len(block)-1], true
	return out, nil
}

// interpolate evaluates the input signal at fractional time t against the
// current block, using the stored previous-block sample when the position
// falls before the block origin.
func (r *Resampler) interpolate(t float64, block []float64) float64 {
	i0 := int64(math.Floor(t))
	frac := t - float64(i0)
	v0 := r.sampleAt(i0, block)
	v1 := r.sampleAt(i0+1, block)
	return v0 + (v1-v0)*frac
}

// sampleAt fetches a global input sample, clamping at both signal edges.
func (r *Resampler) sampleAt(idx int64, block []float64) float64 {
	local := idx - r.inCount
	switch {
	case local >= 0 && int(local) < len(block):
		return block[local]
	case local < 0:
		if r.hasLast {
			return r.lastSample
		}
		return block[0] // edge clamp before any previous block exists
	default:
		return block[len(block)-1] // edge clamp past block end
	}
}

// Flush closes the stream. When the last emitted output position precedes the
// final input sample, exactly one endpoint-clamped tail sample is emitted so
// the stream's end is never lost; otherwise the tail is empty. An empty
// stream flushes to an empty slice. After Flush the resampler cannot be
// reused without Reset.
func (r *Resampler) Flush() ([]float64, *mediaerr.Error) {
	if r.flushed {
		return nil, mediaerr.New(mediaerr.StageResample, mediaerr.KindInvalidInput,
			"resampler flushed twice")
	}
	r.flushed = true
	if r.inCount == 0 {
		return []float64{}, nil
	}
	finalIdx := float64(r.inCount - 1)
	if r.outCount > 0 && r.inputTime(r.outCount-1) >= finalIdx {
		return []float64{}, nil // output grid already covers the final sample
	}
	r.outCount++
	return []float64{r.lastSample}, nil
}

// Reset restores the resampler to its initial state.
func (r *Resampler) Reset() {
	r.inCount, r.outCount = 0, 0
	r.lastSample, r.hasLast, r.flushed = 0, false, false
}

// Rates returns the configured input and output rates.
func (r *Resampler) Rates() (in, out int) { return r.inRate, r.outRate }

// Counts returns total consumed input and emitted output samples.
func (r *Resampler) Counts() (inSamples, outSamples int64) {
	return r.inCount, r.outCount
}
