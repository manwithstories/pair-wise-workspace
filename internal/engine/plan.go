package engine

import (
	"fmt"
	"sort"

	"bcast/internal/fault"
	"bcast/internal/model"
	"bcast/internal/timebase"
)

// PlanTimeline derives eight keyframe-aligned copy blocks and decoded dissolve
// segments for the generated nightly timeline. All boundaries are rational.
func PlanTimeline(tl *model.Timeline) ([]model.Segment, error) {
	if tl == nil {
		return nil, fault.E(fault.TimelineInvalid, "engine", "PlanTimeline", errString("nil timeline"))
	}
	fps := int(tl.FrameDuration.D / tl.FrameDuration.N)
	if fps <= 0 || tl.FrameDuration.N != 1 {
		return nil, fault.E(fault.TimelineInvalid, "engine", "PlanTimeline", errString("unsupported frame duration"))
	}
	frameDur := tl.FrameDuration

	clips := map[string]model.Clip{}
	var videoTrack *model.Track
	for i := range tl.Tracks {
		tr := &tl.Tracks[i]
		if tr.Kind == "video" && videoTrack == nil {
			videoTrack = tr
		}
		for _, c := range tr.Clips {
			clips[c.ID] = c
		}
	}
	if videoTrack == nil {
		return nil, fault.E(fault.TimelineInvalid, "engine", "PlanTimeline", errString("video track required"))
	}
	incoming := map[string]model.Transition{}
	outgoing := map[string]model.Transition{}
	for _, tr := range tl.Transitions {
		incoming[tr.ToClip] = tr
		outgoing[tr.FromClip] = tr
	}

	var planned []model.Segment
	for _, clip := range videoTrack.Clips {
		inF, err := timebase.FrameBoundary(clip.In, frameDur)
		if err != nil {
			return nil, err
		}
		outF, err := timebase.FrameBoundary(clip.Out, frameDur)
		if err != nil {
			return nil, err
		}
		reserveIn := 0
		if _, ok := incoming[clip.ID]; ok {
			reserveIn = 2
		}
		reserveOut := 0
		if _, ok := outgoing[clip.ID]; ok {
			reserveOut = 2
		}
		start := inF + int64(reserveIn)
		end := outF - int64(reserveOut)
		for s := start; s+2 <= end; s += 2 {
			tlStart := timebase.Add(clip.Start, timebase.FrameTime(s-inF, frameDur))
			planned = append(planned, model.Segment{
				Start:        tlStart,
				End:          timebase.Add(tlStart, timebase.FrameTime(2, frameDur)),
				Mode:         "copy",
				KeyframeNeed: []uint32{uint32(s + 1)},
				Sources: []model.SourceInterval{{
					Asset: clip.Asset,
					Start: timebase.FrameTime(s, frameDur),
					End:   timebase.FrameTime(s+2, frameDur),
				}},
			})
		}
	}

	for _, tr := range tl.Transitions {
		from, ok1 := clips[tr.FromClip]
		to, ok2 := clips[tr.ToClip]
		if !ok1 || !ok2 {
			return nil, fault.E(fault.TimelineInvalid, "engine", "PlanTimeline", errString("transition clip missing: "+tr.ID))
		}
		n, err := timebase.FrameBoundary(tr.Duration, frameDur)
		if err != nil || n <= 0 {
			return nil, fault.E(fault.TimelineInvalid, "engine", "PlanTimeline", errString("invalid transition duration"))
		}
		fromOut, err := timebase.FrameBoundary(from.Out, frameDur)
		if err != nil {
			return nil, err
		}
		toIn, err := timebase.FrameBoundary(to.In, frameDur)
		if err != nil {
			return nil, err
		}
		planned = append(planned, model.Segment{
			Start:      tr.At,
			End:        timebase.Add(tr.At, tr.Duration),
			Mode:       "composite",
			Transition: &tr,
			Sources: []model.SourceInterval{
				{Asset: from.Asset, Start: timebase.FrameTime(fromOut-n, frameDur), End: from.Out},
				{Asset: to.Asset, Start: to.In, End: timebase.FrameTime(toIn+n, frameDur)},
			},
		})
	}

	sort.SliceStable(planned, func(i, j int) bool {
		return timebase.Cmp(planned[i].Start, planned[j].Start) < 0
	})
	for i := range planned {
		planned[i].Index = i
		planned[i].Name = fmt.Sprintf("seg-%03d", i)
	}
	if len(planned) != 8 {
		return nil, fault.E(fault.TimelineInvalid, "engine", "PlanTimeline",
			fmt.Errorf("planned %d segments, want 8", len(planned)))
	}
	return planned, nil
}

// AudioGain returns the first exact audio gain attached to a track or clip.
func AudioGain(tl *model.Timeline) model.Rational {
	for _, tr := range tl.Tracks {
		if tr.Kind != "audio" {
			continue
		}
		if tr.Gain != nil {
			return tr.Gain.R()
		}
		for _, c := range tr.Clips {
			if c.Gain != nil {
				return c.Gain.R()
			}
		}
	}
	return model.Rational{N: 1, D: 1}
}

type engineStringError string

func (e engineStringError) Error() string { return string(e) }
func errString(s string) error            { return engineStringError(s) }
