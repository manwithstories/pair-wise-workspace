package audio

import (
	"mediapipeline/internal/mediaerr"
)

// Layout identifies a supported channel layout. Unknown layout identifiers are
// rejected with a locatable mediaerr error instead of producing silent data.
type Layout string

// Supported layouts; channel orders are documented on each constant.
const (
	LayoutMono   Layout = "mono"   // [C]
	LayoutStereo Layout = "stereo" // [L, R]
	Layout51     Layout = "5.1"    // [FL, FR, C, LFE, BL, BR]
)

// Channels returns the sample frame size of a layout.
func (l Layout) Channels() (int, *mediaerr.Error) {
	switch l {
	case LayoutMono:
		return 1, nil
	case LayoutStereo:
		return 2, nil
	case Layout51:
		return 6, nil
	default:
		return 0, mediaerr.New(mediaerr.StageLayout, mediaerr.KindLayout,
			"unknown channel layout: "+string(l))
	}
}

// 5.1 channel indices in the [FL, FR, C, LFE, BL, BR] order used here.
const (
	idxFL  = 0
	idxFR  = 1
	idxC   = 2
	idxLFE = 3
	idxBL  = 4
	idxBR  = 5
)

// Normalize converts interleaved PCM frames from src into the target layout.
// Missing channels are filled deterministically: duplicated from an existing
// channel for up-mix scenarios, silence for the LFE and surround channels
// that have no stereo counterpart. Incomplete trailing frames are reported,
// never padded with fake samples.
func Normalize(src Layout, target Layout, frames []float64) ([]float64, *mediaerr.Error) {
	srcCh, merr := src.Channels()
	if merr != nil {
		return nil, merr
	}
	dstCh, merr := target.Channels()
	if merr != nil {
		return nil, merr
	}
	if len(frames)%srcCh != 0 {
		return nil, mediaerr.New(mediaerr.StageLayout, mediaerr.KindIncompleteFrame,
			"sample count is not a whole number of source frames").
			WithOffset(int64(len(frames)))
	}
	if src == target {
		out := make([]float64, len(frames))
		copy(out, frames)
		return out, nil
	}
	frameCount := len(frames) / srcCh
	out := make([]float64, frameCount*dstCh)
	for f := 0; f < frameCount; f++ {
		in := frames[f*srcCh : (f+1)*srcCh]
		dst := out[f*dstCh : (f+1)*dstCh]
		convertFrame(src, target, in, dst)
	}
	return out, nil
}

// convertFrame maps one source frame into a pre-sized destination frame.
func convertFrame(src, target Layout, in, dst []float64) {
	switch {
	case src == LayoutMono && target == LayoutStereo:
		dst[0], dst[1] = in[0], in[0]
	case src == LayoutMono && target == Layout51:
		// Mono feeds the three front channels; LFE/surrounds have no source.
		dst[idxFL], dst[idxC], dst[idxFR] = in[0], in[0], in[0]
	case src == LayoutStereo && target == LayoutMono:
		dst[0] = 0.5 * (in[0] + in[1])
	case src == LayoutStereo && target == Layout51:
		// Stereo maps to front L/R; C mirrors the mono fold-down, LFE and
		// the surround pair are silence-filled.
		dst[idxFL], dst[idxFR] = in[0], in[1]
		dst[idxC] = 0.5 * (in[0] + in[1])
	case src == Layout51 && target == LayoutMono:
		dst[0] = foldMono51(in)
	case src == Layout51 && target == LayoutStereo:
		// ITU-R BS.775 style Lo/Ro down-mix; LFE is excluded by design.
		dst[0] = in[idxFL] + 0.7071*in[idxC] + 0.7071*in[idxBL]
		dst[1] = in[idxFR] + 0.7071*in[idxC] + 0.7071*in[idxBR]
	}
}

// foldMono51 down-mixes a 5.1 frame to mono, excluding the LFE channel.
func foldMono51(in []float64) float64 {
	const gain = 0.7071
	sum := in[idxFL] + in[idxFR] + gain*in[idxC] + gain*in[idxBL] + gain*in[idxBR]
	return sum / 3.0 // normalization keeps full-scale input near full-scale
}
