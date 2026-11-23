package render

import "mediasim/internal/clock"

// CutAudio slices an audio block at a tick boundary, padding with silence
// when the block starts after the cut. All arithmetic is in samples so the
// output is sample-exact.
func CutAudio(src []int16, blockStartTick, cutTick, blockTicks int64) (AudioBlock, error) {
	if blockTicks <= 0 {
		return AudioBlock{}, clock.Errorf(clock.CatArgument, clock.Span{}, clock.RecoveredNone,
			"audio block duration %d must be positive", blockTicks)
	}
	if cutTick < blockStartTick || cutTick > blockStartTick+blockTicks {
		return AudioBlock{}, clock.Errorf(clock.CatSync,
			clock.NewSpan("", "", clock.Interval{Start: cutTick, End: cutTick}), clock.RecoveredSkipped,
			"cut tick %d outside block [%d,%d]", cutTick, blockStartTick, blockStartTick+blockTicks)
	}
	perTick := float64(len(src)) / float64(blockTicks)
	offset := int(float64(cutTick-blockStartTick) * perTick)
	if offset < 0 {
		offset = 0
	}
	if offset > len(src) {
		offset = len(src)
	}
	out := append([]int16(nil), src[offset:]...)
	return AudioBlock{Start: cutTick, Samples: out}, nil
}

// PadSilence prepends n samples of silence to a block, reporting how many
// were added so the report can distinguish real audio from padding.
func PadSilence(b AudioBlock, samples int) AudioBlock {
	if samples <= 0 {
		return b
	}
	out := make([]int16, samples, samples+len(b.Samples))
	for i := range out {
		out[i] = SilenceSample
	}
	out = append(out, b.Samples...)
	b.Samples = out
	b.Silenced += samples
	return b
}
