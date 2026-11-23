package mp4

import (
	"fmt"

	"mediascan/internal/fault"
)

// KeyframeReport summarizes keyframe spacing for one track.
type KeyframeReport struct {
	TrackID       uint32
	KeyframeCount int
	// MaxGapTicks is the largest presentation-time distance between two
	// consecutive keyframes, computed from actual per-sample durations.
	MaxGapTicks int64
	// MaxGapFrames counts the samples spanning that worst interval.
	MaxGapFrames int64
	// FirstBadFrame is the first non-keyframe sample that pushed the running
	// gap past the configured limit (zero-based), or -1 when within limit.
	FirstBadFrame int64
	// Faults collects every interval that exceeds maxGapTicks.
	Faults []fault.Fault
}

// AnalyzeKeyframes enforces a maximum keyframe interval. Distances are summed
// from individual sample durations, so variable frame rates are handled
// exactly; average frame rate is never assumed.
func AnalyzeKeyframes(info TrackInfo, maxGapTicks int64) KeyframeReport {
	rep := KeyframeReport{TrackID: info.TrackID, FirstBadFrame: -1}
	if len(info.Samples) == 0 {
		return rep
	}
	lastKey := -1
	var gapTicks int64
	var gapFrames int64
	for i, s := range info.Samples {
		if s.IsKeyframe {
			rep.KeyframeCount++
			if lastKey >= 0 && gapTicks > rep.MaxGapTicks {
				rep.MaxGapTicks = gapTicks
				rep.MaxGapFrames = gapFrames
			}
			lastKey = i
			gapTicks = 0
			gapFrames = 0
			continue
		}
		if lastKey < 0 {
			// Samples before the first keyframe are not measurable.
			continue
		}
		gapTicks += s.DurationTicks
		gapFrames++
		if maxGapTicks > 0 && gapTicks > maxGapTicks && rep.FirstBadFrame < 0 {
			rep.FirstBadFrame = int64(i)
			rep.Faults = append(rep.Faults, fault.New(
				fault.KindKeyframeGap, fault.SeverityWarning, info.TrackID,
				int64(i), s.Offset, s.StartTicks,
				fmt.Sprintf("keyframe gap %d ticks exceeds limit %d after frame %d",
					gapTicks, maxGapTicks, lastKey),
			).WithSpan(gapFrames, 0))
		}
	}
	// A stream ending without a closing keyframe still reports its open gap.
	if lastKey >= 0 && gapTicks > rep.MaxGapTicks {
		rep.MaxGapTicks = gapTicks
		rep.MaxGapFrames = gapFrames
	}
	return rep
}

// FirstTruncationFault converts a scanner error into a localized fault.
func FirstTruncationFault(trackID uint32, offset int64, span int64) fault.Fault {
	return fault.New(fault.KindTruncatedBox, fault.SeverityFatal,
		trackID, -1, offset, 0,
		fmt.Sprintf("truncated box at byte %d spanning %d bytes", offset, span),
	).WithSpan(0, span)
}
