// Package repair streams damaged PCM through bounded frame buffers and
// atomically commits deterministic recovered WAV output.
package repair

import (
	"archivectl/internal/core"
)

// Coverage is a sorted, merged set of frame ranges to conceal with silence.
type Coverage struct {
	spans []core.Span
}

// NewCoverage validates repair limits and merges overlapping damage ranges.
// Any range larger than the executor's frame-cache budget is rejected: the
// executor must not silently make an unbounded zero fill.
func NewCoverage(spans []core.Span) (*Coverage, error) {
	merged := core.MergeSpans(spans)
	for _, s := range merged {
		if !s.Recoverable {
			return nil, core.Newf(core.KindDamageUnrecoverable, "repair.NewCoverage",
				"damage %s [%d,%d) is marked unrecoverable", s.Kind, s.StartFrame, s.EndFrame)
		}
		if s.Frames() > int64(core.MaxCachedFrames) {
			return nil, core.Newf(core.KindDamageUnrecoverable, "repair.NewCoverage",
				"damage %s [%d,%d) is %d frames; limit is %d",
				s.Kind, s.StartFrame, s.EndFrame, s.Frames(), core.MaxCachedFrames)
		}
		if s.StartFrame < 0 || s.EndFrame < s.StartFrame {
			return nil, core.Newf(core.KindDamageUnrecoverable, "repair.NewCoverage",
				"invalid damage range [%d,%d)", s.StartFrame, s.EndFrame)
		}
	}
	return &Coverage{spans: merged}, nil
}

// Spans returns the canonical merged ranges.
func (c *Coverage) Spans() []core.Span {
	if c == nil {
		return nil
	}
	return append([]core.Span(nil), c.spans...)
}

// Frames returns total unique covered frames.
func (c *Coverage) Frames() int64 {
	var n int64
	for _, s := range c.spans {
		n += s.Frames()
	}
	return n
}

// Contains reports whether frame is in a covered range.
func (c *Coverage) Contains(frame int64) bool {
	if c == nil {
		return false
	}
	for _, s := range c.spans {
		if frame < s.StartFrame {
			return false
		}
		if frame < s.EndFrame {
			return true
		}
	}
	return false
}

// ZeroBuffer sets all samples belonging to covered frames to the canonical
// zero sample. It returns the number of frames zeroed in this call.
func (c *Coverage) ZeroBuffer(data []int, channels int, startFrame int64, frameCount int) int64 {
	if channels <= 0 || frameCount <= 0 {
		return 0
	}
	var zeroed int64
	for f := 0; f < frameCount; f++ {
		frame := startFrame + int64(f)
		if c.Contains(frame) {
			base := f * channels
			for ch := 0; ch < channels; ch++ {
				if base+ch < len(data) {
					data[base+ch] = 0
				}
			}
			zeroed++
		}
	}
	return zeroed
}
