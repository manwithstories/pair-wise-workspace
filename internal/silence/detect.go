// Package silence detects per-channel silent ranges from decoded PCM.
// Results keep the owning track and exact rational time range, and gaps
// (unrecovered blocks) are never treated as silence — only real samples
// can be classified.
package silence

import (
	"audiorestore/internal/model"
)

// Config parameterizes detection.
type Config struct {
	// Threshold is the inclusive per-sample absolute amplitude below
	// which a frame counts as silent for one channel.
	Threshold int
	// MinRunFrames is the shortest contiguous silent run that is
	// reported. Shallow dips stay unreported.
	MinRunFrames int64
}

// Range is one contiguous silent interval on one channel.
type Range struct {
	Track      string
	Channel    int
	StartFrame int64
	EndFrame   int64 // half-open
	StartPTS   model.Rational
	EndPTS     model.Rational
}

// Detect scans per-channel sample slices. samples[c] holds the frames in
// [frameBase, frameBase+len) on the track's clock; nil channels and gaps
// in coverage are skipped rather than counted as silence.
func Detect(trackID string, rate model.SampleRate, samples [][]int, frameBase int64, cfg Config) []Range {
	var out []Range
	for ch, frames := range samples {
		if frames == nil {
			continue
		}
		var runStart int64 = -1
		flush := func(end int64) {
			if runStart < 0 || end-runStart < cfg.MinRunFrames {
				runStart = -1
				return
			}
			out = append(out, Range{
				Track:      trackID,
				Channel:    ch,
				StartFrame: runStart,
				EndFrame:   end,
				StartPTS:   model.SampleToPTS(runStart, rate),
				EndPTS:     model.SampleToPTS(end, rate),
			})
			runStart = -1
		}
		for i, v := range frames {
			pos := frameBase + int64(i)
			if absInt(v) <= cfg.Threshold {
				if runStart < 0 {
					runStart = pos
				}
				continue
			}
			flush(pos)
		}
		flush(frameBase + int64(len(frames)))
	}
	return out
}

// SilentFrames counts individual silent channel-frames across ranges,
// feeding the silence_frames summary metric.
func SilentFrames(ranges []Range) int64 {
	var n int64
	for _, r := range ranges {
		n += r.EndFrame - r.StartFrame
	}
	return n
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
