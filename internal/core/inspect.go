// Package core holds the format-agnostic processing kernel: inspection,
// recovery planning and remux. Adapters feed it unified-time frames and
// structured faults; vendor format details never enter here.
package core

import (
	"fmt"

	"media-check/internal/model"
)

// ColorDecision records one traceable colour-tag action. Source tags are
// never silently overwritten; every fill or conflict is logged here and
// surfaced in the manifest.
type ColorDecision struct {
	Segment int
	Action  string // "carry-forward" | "conflict-keep-first"
	Before  model.ColorMeta
	After   model.ColorMeta
	Reason  string
}

// InspectReport summarises one scanned stream.
type InspectReport struct {
	FrameCount     int
	KeyframeIdx    []int // frame indices that are keyframes
	MaxGap         int   // largest keyframe interval in frames
	ColorDecisions []ColorDecision
	Faults         model.Faults
}

// Inspect scans frames on the unified time base: it measures keyframe
// intervals and resolves colour tags per segment. Missing tags are
// carried forward from the last specified frame with a logged decision;
// conflicting specified tags keep the first and raise a fault.
func Inspect(frames []model.Frame, segmentFrames int) InspectReport {
	rep := InspectReport{FrameCount: len(frames)}
	if segmentFrames <= 0 {
		segmentFrames = 1
	}
	var lastColor model.ColorMeta
	var haveColor bool
	segSeen := map[int]bool{}

	for i, f := range frames {
		if f.Keyframe {
			if n := len(rep.KeyframeIdx); n > 0 {
				if gap := i - rep.KeyframeIdx[n-1]; gap > rep.MaxGap {
					rep.MaxGap = gap
				}
			}
			rep.KeyframeIdx = append(rep.KeyframeIdx, i)
		}
		seg := i / segmentFrames
		switch {
		case f.Color.Specified():
			if haveColor && f.Color != lastColor && segSeen[seg] {
				rep.Faults = append(rep.Faults, model.Fault{
					Code: model.FaultColorConflict, Segment: seg,
					Range: model.TimeRange{Start: f.Time, End: f.Time},
					Detail: fmt.Sprintf("frame %d colour %+v conflicts with %+v; keeping first",
						f.Seq, f.Color, lastColor),
				})
				rep.ColorDecisions = append(rep.ColorDecisions, ColorDecision{
					Segment: seg, Action: "conflict-keep-first",
					Before: f.Color, After: lastColor,
					Reason: "conflicting specified tags in segment",
				})
			} else {
				lastColor = f.Color
				haveColor = true
			}
		case haveColor:
			rep.ColorDecisions = append(rep.ColorDecisions, ColorDecision{
				Segment: seg, Action: "carry-forward",
				Before: f.Color, After: lastColor,
				Reason: fmt.Sprintf("frame %d missing colour tags", f.Seq),
			})
			rep.Faults = append(rep.Faults, model.Fault{
				Code: model.FaultColorMissing, Segment: seg,
				Range:  model.TimeRange{Start: f.Time, End: f.Time},
				Detail: fmt.Sprintf("frame %d colour tags unspecified, carried forward", f.Seq),
			})
		default:
			rep.Faults = append(rep.Faults, model.Fault{
				Code: model.FaultColorMissing, Segment: seg,
				Range:  model.TimeRange{Start: f.Time, End: f.Time},
				Detail: fmt.Sprintf("frame %d colour tags unspecified, no prior reference", f.Seq),
			})
		}
		segSeen[seg] = true
	}
	return rep
}
