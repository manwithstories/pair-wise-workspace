package pipeline

import (
	"io"

	"streamcut/internal/packager"
	"streamcut/internal/segment"
)

// writeSegment renders one cut segment as a self-contained fMP4 fragment.
// Video samples carry Annex B bytes; durations are expressed in the 90 kHz
// video timescale and 48 kHz audio timescale respectively.
func writeSegment(w io.WriteSeeker, seg *segment.Segment) error {
	ms := packager.MediaSegment{
		Sequence: uint32(seg.Index + 1),
		Samples:  map[uint32][]packager.Sample{},
	}
	if n := len(seg.Video); n > 0 {
		samples := make([]packager.Sample, 0, n)
		for i, au := range seg.Video {
			dur := uint32(3000) // 30 fps => 3000 ticks
			if i+1 < n {
				dur = uint32(seg.Video[i+1].PTS - au.PTS)
			}
			samples = append(samples, packager.Sample{
				Data: au.Data, Duration: dur, IsSync: au.IsKeyframe(),
			})
		}
		ms.Samples[1] = samples
	}
	if n := len(seg.Audio); n > 0 {
		samples := make([]packager.Sample, 0, n)
		for _, au := range seg.Audio {
			// 20 ms of 16-bit mono PCM = 960 samples at 48 kHz.
			samples = append(samples, packager.Sample{
				Data: au.Data, Duration: 960,
			})
		}
		ms.Samples[2] = samples
	}
	return packager.WriteMediaSegment(w, ms)
}
