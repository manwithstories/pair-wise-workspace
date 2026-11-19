// Package segment cuts the unified timeline into candidate clips, flagging
// every boundary that cannot be honoured because of a gap or a missing
// keyframe.
package segment

import (
	"fmt"
	"sort"

	"mediasync/internal/media"
	"mediasync/internal/mp4"
)

// Limit bounds how many clips one request may produce.
const MaxSegments = 10_000

// DegradeReason explains why a segment boundary was not honoured exactly.
type DegradeReason string

const (
	DegradeNone         DegradeReason = ""
	DegradeGap          DegradeReason = "packet_gap"
	DegradeNoKeyframe   DegradeReason = "no_keyframe"
	DegradeSnappedStart DegradeReason = "start_snapped_to_keyframe"
	DegradeShortTail    DegradeReason = "short_tail"
	DegradeNonMonotonic DegradeReason = "non_monotonic_samples"
)

// Request describes how to cut a track.
type Request struct {
	Name      string
	TargetMS  int64
	OverlapMS int64
	MinMS     int64
	StartNS   int64
	EndNS     int64
	Gaps      []Gap
}

// Gap is a half-open [StartNS, EndNS) interval with no recoverable packets.
type Gap struct {
	StartNS int64 `json:"start_ns"`
	EndNS   int64 `json:"end_ns"`
}

// Contains reports whether ns falls inside the gap.
func (g Gap) Contains(ns int64) bool { return ns >= g.StartNS && ns < g.EndNS }

// Segment is one candidate clip.
type Segment struct {
	Index         int             `json:"index"`
	StartNS       int64           `json:"start_ns"`
	EndNS         int64           `json:"end_ns"`
	StartSample   int             `json:"start_sample"`
	EndSample     int             `json:"end_sample"`
	Degrade       []DegradeReason `json:"degrade,omitempty"`
	DegradeDetail []string        `json:"degrade_detail,omitempty"`
	SourceFile    string          `json:"source_file"`
}

// DurationNS is the segment length in nanoseconds.
func (s Segment) DurationNS() int64 { return s.EndNS - s.StartNS }

// Degraded reports whether any boundary was adjusted.
func (s Segment) Degraded() bool { return len(s.Degrade) > 0 }

// Result is the outcome of cutting one track.
type Result struct {
	Segments []Segment      `json:"segments"`
	Issues   []*media.Issue `json:"issues"`
	Track    string         `json:"track"`
}

func (r *Result) add(is *media.Issue) { r.Issues = append(r.Issues, is) }

func (r *Result) issue(off int64, rec int, code media.Code, format string, args ...any) {
	r.add(media.New(media.StageSegment, media.InputMP4, code, format, args...).WithOffset(off).WithRecord(rec))
}

// Cut slices a video track into clips of about TargetMS. Start boundaries are
// snapped forward to the nearest keyframe; if that lands inside a gap the clip
// is marked as degraded rather than silently shifted again.
func Cut(tr *mp4.Track, req Request) *Result {
	res := &Result{Track: req.Name}
	if req.TargetMS <= 0 {
		res.issue(0, 0, media.CodeInvalidArgument, "target duration must be positive, got %dms", req.TargetMS)
		return res
	}
	if len(tr.Samples) == 0 {
		res.issue(0, 0, media.CodeMissingEntry, "track %q has no samples to cut", req.Name)
		return res
	}
	if len(tr.Keyframes) == 0 {
		res.issue(0, 0, media.CodeNoKeyframe, "track %q has no keyframes; boundaries degrade to sample starts", req.Name)
	}
	targetNS := req.TargetMS * 1_000_000
	overlapNS := req.OverlapMS * 1_000_000
	minNS := req.MinMS * 1_000_000
	if overlapNS >= targetNS {
		res.issue(0, 0, media.CodeInvalidArgument, "overlap %dms must be smaller than target %dms", req.OverlapMS, req.TargetMS)
		return res
	}
	endNS := req.EndNS
	if endNS <= 0 || endNS > tr.DurationNS {
		endNS = trackEnd(tr)
	}
	start := req.StartNS
	if start < 0 {
		start = 0
	}
	if start >= endNS {
		res.issue(0, 0, media.CodeOutOfRange, "requested range starts at %dns but track ends at %dns", start, endNS)
		return res
	}
	gaps := append([]Gap(nil), req.Gaps...)
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].StartNS < gaps[j].StartNS })

	idx := 0
	floor := -1 // start sample of the previous clip; pull-backs may not revisit it
	for cursor := start; cursor < endNS; {
		if idx >= MaxSegments {
			res.issue(0, idx, media.CodeLimitExceeded, "more than %d segments requested", MaxSegments)
			break
		}
		sampleStart, degrades, details := snapStart(tr, cursor, gaps, floor, overlapNS > 0)
		if sampleStart < 0 {
			// No sample covers the cursor: advance to the next sample.
			next := nextSampleAtOrAfter(tr, cursor)
			if next < 0 || tr.Samples[next].DTSNS >= endNS || next <= floor {
				break
			}
			res.issue(tr.Samples[next].Offset, idx+1, media.CodeOffsetInsideGap,
				"cursor %dns fell between samples; advanced to %dns", cursor, tr.Samples[next].DTSNS)
			cursor = tr.Samples[next].DTSNS
			continue
		}
		if idx > 0 && sampleStart <= floor {
			// The only reachable start is the previous clip's own start, e.g.
			// when no keyframe exists beyond it. Extend the previous clip to
			// the track end rather than emitting duplicates.
			last := &res.Segments[len(res.Segments)-1]
			if end := sampleEndingAt(tr, endNS); end > last.EndSample {
				last.EndSample = end
				last.EndNS = tr.Samples[end].EndNS()
				last.DegradeDetail = append(last.DegradeDetail,
					fmt.Sprintf("extended to %dns: no later keyframe to start from", last.EndNS))
			}
			break
		}
		segStart := tr.Samples[sampleStart].DTSNS
		segEnd := segStart + targetNS
		if segEnd > endNS {
			segEnd = endNS
		}
		lastSample := sampleEndingAt(tr, segEnd)
		if lastSample < sampleStart {
			lastSample = sampleStart
		}
		actualEnd := tr.Samples[lastSample].EndNS()
		spanReported := false
		for _, g := range gaps {
			if g.Contains(actualEnd) {
				degrades = appendUnique(degrades, DegradeGap)
				details = append(details, fmt.Sprintf("end %dns inside gap [%d,%d)", actualEnd, g.StartNS, g.EndNS))
			}
			if g.Contains(segStart) {
				degrades = appendUnique(degrades, DegradeGap)
			}
			if g.StartNS < actualEnd && g.EndNS > segStart {
				// Report once per segment even when several gaps overlap.
				if !spanReported {
					res.issue(tr.Samples[sampleStart].Offset, idx+1, media.CodeOffsetInsideGap,
						"segment %d [%d,%d) spans packet gap [%d,%d)", idx, segStart, actualEnd, g.StartNS, g.EndNS)
					spanReported = true
				}
				degrades = appendUnique(degrades, DegradeGap)
				details = append(details, fmt.Sprintf("clip spans packet gap [%d,%d)", g.StartNS, g.EndNS))
			}
		}
		if minNS > 0 && actualEnd-segStart < minNS {
			if len(res.Segments) == 0 {
				degrades = appendUnique(degrades, DegradeShortTail)
				details = append(details, fmt.Sprintf("first segment is %dms, minimum is %dms",
					(actualEnd-segStart)/1_000_000, req.MinMS))
			} else {
				res.issue(tr.Samples[sampleStart].Offset, idx+1, media.CodeOffsetInsideGap,
					"tail segment of %dms dropped below minimum %dms", (actualEnd-segStart)/1_000_000, req.MinMS)
				break
			}
		}
		res.Segments = append(res.Segments, Segment{
			Index:         idx,
			StartNS:       segStart,
			EndNS:         actualEnd,
			StartSample:   sampleStart,
			EndSample:     lastSample,
			Degrade:       degrades,
			DegradeDetail: details,
			SourceFile:    tr.File,
		})
		idx++
		floor = sampleStart
		next := segStart + targetNS - overlapNS
		if next <= cursor {
			next = actualEnd
		}
		cursor = next
		if actualEnd >= endNS {
			break
		}
	}
	if len(res.Segments) == 0 {
		res.issue(0, 0, media.CodeOutOfRange, "no segments could be produced for %q", req.Name)
		return res
	}
	// The loop can stop inside the overlap of the last clip, leaving a short
	// uncovered tail. Extend the final clip instead of emitting a duplicate.
	if last := &res.Segments[len(res.Segments)-1]; last.EndNS < endNS {
		if idx := sampleEndingAt(tr, endNS); idx > last.EndSample {
			last.EndSample = idx
			last.EndNS = tr.Samples[idx].EndNS()
			last.DegradeDetail = append(last.DegradeDetail,
				fmt.Sprintf("tail extended to %dns to cover the track end", last.EndNS))
		}
	}
	return res
}

func trackEnd(tr *mp4.Track) int64 {
	end := int64(0)
	for _, s := range tr.Samples {
		if s.EndNS() > end {
			end = s.EndNS()
		}
	}
	return end
}

// snapStart returns the sample whose keyframe covers cursor, snapping forward
// when the cursor sits between keyframes. Past the last keyframe the start is
// pulled back to the preceding keyframe, unless that would revisit the clip
// already emitted; in that case the covering sample is used and the clip is
// marked as having no keyframe.
func snapStart(tr *mp4.Track, cursor int64, gaps []Gap, floor int, mayPullBack bool) (int, []DegradeReason, []string) {
	exact := tr.KeyframeAt(cursor)
	if exact >= 0 {
		return exact, nil, nil
	}
	best := -1
	for _, k := range tr.Keyframes {
		if tr.Samples[k].DTSNS >= cursor {
			best = k
			break
		}
	}
	if best < 0 {
		// Past the last keyframe: pull the start back to the preceding
		// keyframe so the tail clip stays decodable on its own.
		if k := tr.KeyframeAtOrBefore(cursor); k >= 0 && k > floor && mayPullBack {
			return k, []DegradeReason{DegradeSnappedStart},
				[]string{fmt.Sprintf("start %dns pulled back to keyframe at %dns", cursor, tr.Samples[k].DTSNS)}
		}
		if i := tr.SampleForTime(cursor); i >= 0 {
			return i, []DegradeReason{DegradeNoKeyframe},
				[]string{fmt.Sprintf("no keyframe at or before %dns", cursor)}
		}
		return -1, nil, nil
	}
	detail := fmt.Sprintf("start %dns snapped to keyframe at %dns", cursor, tr.Samples[best].DTSNS)
	for _, g := range gaps {
		if g.Contains(tr.Samples[best].DTSNS) {
			return best, []DegradeReason{DegradeSnappedStart, DegradeGap}, []string{detail}
		}
	}
	return best, []DegradeReason{DegradeSnappedStart}, []string{detail}
}

func nextSampleAtOrAfter(tr *mp4.Track, ns int64) int {
	for i := range tr.Samples {
		if tr.Samples[i].EndNS() > ns {
			return i
		}
	}
	return -1
}

func sampleEndingAt(tr *mp4.Track, ns int64) int {
	best := -1
	for i := range tr.Samples {
		if tr.Samples[i].EndNS() <= ns {
			best = i
		} else {
			break
		}
	}
	if best < 0 && len(tr.Samples) > 0 {
		return 0
	}
	return best
}

func appendUnique(list []DegradeReason, r DegradeReason) []DegradeReason {
	for _, v := range list {
		if v == r {
			return list
		}
	}
	return append(list, r)
}
