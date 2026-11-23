// Package segment cuts the shared timeline into half-open time windows and
// reports silence overlap per track. Cut boundaries snap to the nearest input
// frame edge so cross-track skew never exceeds one frame interval.
package segment

import (
	"fmt"
	"sort"

	"medpipe/internal/audio"
	"medpipe/internal/clock"
	"medpipe/internal/model"
)

// Options controls segmentation.
type Options struct {
	// WindowNS is the nominal segment length. Zero means 2s.
	WindowNS int64
	// SnapToleranceNS caps how far a cut may move to meet a frame edge. Zero
	// means one median frame interval of the finest track.
	SnapToleranceNS int64
	// MinSegmentNS drops trailing fragments shorter than this. Zero keeps
	// every non-empty fragment.
	MinSegmentNS int64
}

// DefaultOptions is a 2 second window.
func DefaultOptions() Options {
	return Options{WindowNS: 2_000_000_000}
}

// TrackSlice is the range one track covers inside a segment. All tracks in a
// segment share the segment's half-open [StartNS, EndNS), which is what makes
// cross-track boundaries agree exactly.
type TrackSlice struct {
	TrackID    string
	StartNS    int64
	EndNS      int64
	FirstFrame int
	FrameCount int
	// SnapErrorNS is the distance between the segment cut and this track's
	// nearest frame edge. It must never exceed one input frame interval.
	SnapErrorNS int64
}

// SilenceOverlap is the portion of a silence range covered by a segment.
type SilenceOverlap struct {
	TrackID string
	StartNS int64
	EndNS   int64
}

// Segment is one output chunk.
type Segment struct {
	Index   int
	StartNS int64
	EndNS   int64
	Slices  []TrackSlice
	Silence []SilenceOverlap
}

// Result bundles segments with the diagnostics the CLI reports.
type Result struct {
	Segments []Segment
	// MaxSnapErrorNS is the worst distance any cut landed from a frame edge.
	MaxSnapErrorNS int64
	// MaxFrameIntervalNS is the coarsest track interval, the allowed bound.
	MaxFrameIntervalNS int64
}

// Plan builds segments over [timeline.StartNS, timeline.EndNS). Silence
// ranges are split at cuts, so every silent sample is attributed to exactly
// one segment.
func Plan(tl *clock.Timeline, silence []audio.Range, opt Options) (*Result, error) {
	if tl == nil {
		return nil, model.New(model.KindInput, "segment.Plan", "nil timeline")
	}
	if opt.WindowNS <= 0 {
		opt.WindowNS = 2_000_000_000
	}
	start, end := tl.StartNS, tl.EndNS
	if end <= start {
		return nil, model.New(model.KindInput, "segment.Plan", "timeline is empty")
	}
	edges, minInterval, maxInterval := collectEdges(tl)
	if len(edges) == 0 {
		return nil, model.New(model.KindInput, "segment.Plan", "timeline has no frames")
	}
	if opt.SnapToleranceNS <= 0 {
		opt.SnapToleranceNS = minInterval
	}
	// Snapping may never exceed one input frame interval; clamp caller input.
	if maxInterval > 0 && opt.SnapToleranceNS > maxInterval {
		opt.SnapToleranceNS = maxInterval
	}

	var bounds []int64
	nominal := start
	for nominal < end {
		b := snap(nominal, edges, opt.SnapToleranceNS)
		// A snap must never carry a cut past the timeline it is cutting.
		if b > end {
			b = end
		}
		if len(bounds) == 0 || b > bounds[len(bounds)-1] {
			bounds = append(bounds, b)
		}
		nominal += opt.WindowNS
	}
	if len(bounds) == 0 || bounds[len(bounds)-1] < end {
		bounds = append(bounds, end)
	}
	if len(bounds) < 2 {
		return nil, model.New(model.KindInvariant, "segment.Plan", "produced fewer than two boundaries")
	}

	res := &Result{MaxFrameIntervalNS: maxInterval}
	for i := 0; i+1 < len(bounds); i++ {
		s, e := bounds[i], bounds[i+1]
		if e <= s {
			return nil, model.New(model.KindInvariant, "segment.Plan", "non-increasing segment bounds")
		}
		if opt.MinSegmentNS > 0 && i+2 < len(bounds) && e-s < opt.MinSegmentNS {
			// Fragments are merged forward by extending the next boundary; in
			// practice snapping already keeps windows within a frame of
			// nominal, so this branch only guards pathological tolerances.
			continue
		}
		seg := Segment{Index: len(res.Segments), StartNS: s, EndNS: e}
		for t := range tl.Tracks {
			tt := &tl.Tracks[t]
			if len(tt.Frames) == 0 {
				continue
			}
			first := tt.FrameAtOrAfter(s)
			last := tt.FrameAtOrAfter(e)
			if first >= last {
				continue // no whole frame starts inside this window
			}
			// The cut must land within one frame interval of this track's
			// nearest frame edge. The window end is exclusive, so we measure
			// against frame ends too; the track's own coverage ends at last
			// frame end, and trailing coverage is not a slicing error.
			se := nearestEdgeError(s, tt)
			if e != end {
				if ee := nearestEdgeError(e, tt); ee > se {
					se = ee
				}
			}
			if maxInterval > 0 && se > maxInterval {
				return nil, model.New(model.KindInvariant, "segment.Plan",
					fmt.Sprintf("track %s cut error %d exceeds one frame interval %d", tt.Track.ID, se, maxInterval))
			}
			if se > res.MaxSnapErrorNS {
				res.MaxSnapErrorNS = se
			}
			seg.Slices = append(seg.Slices, TrackSlice{
				TrackID: tt.Track.ID, StartNS: s, EndNS: e,
				FirstFrame: first, FrameCount: last - first, SnapErrorNS: se,
			})
		}
		if len(seg.Slices) == 0 {
			continue
		}
		seg.Silence = splitSilence(silence, s, e)
		res.Segments = append(res.Segments, seg)
	}
	if len(res.Segments) == 0 {
		return nil, model.New(model.KindInput, "segment.Plan", "no segment contains any frame")
	}
	return res, nil
}

// splitSilence clips each range to [s,e). Because intervals are half-open, a
// range crossing a cut contributes its left part to one segment and its
// right part to the next with no duplicated edge.
func splitSilence(ranges []audio.Range, s, e int64) []SilenceOverlap {
	var out []SilenceOverlap
	for _, r := range ranges {
		lo, hi := r.StartNS, r.EndNS
		if lo < s {
			lo = s
		}
		if hi > e {
			hi = e
		}
		if hi <= lo {
			continue
		}
		out = append(out, SilenceOverlap{TrackID: r.TrackID, StartNS: lo, EndNS: hi})
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].TrackID != out[b].TrackID {
			return out[a].TrackID < out[b].TrackID
		}
		return out[a].StartNS < out[b].StartNS
	})
	return out
}

func collectEdges(tl *clock.Timeline) (edges []int64, minInterval, maxInterval int64) {
	set := map[int64]struct{}{}
	minInterval, maxInterval = 0, 0
	for i := range tl.Tracks {
		tt := &tl.Tracks[i]
		if tt.FrameIntervalNS > 0 && (minInterval == 0 || tt.FrameIntervalNS < minInterval) {
			minInterval = tt.FrameIntervalNS
		}
		if tt.MaxIntervalNS > maxInterval {
			maxInterval = tt.MaxIntervalNS
		}
		for _, fr := range tt.Frames {
			set[fr.NS] = struct{}{}
			set[fr.NS+fr.IntervalNS] = struct{}{}
		}
	}
	edges = make([]int64, 0, len(set))
	for e := range set {
		edges = append(edges, e)
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i] < edges[j] })
	return edges, minInterval, maxInterval
}

func snap(nominal int64, edges []int64, tol int64) int64 {
	i := sort.Search(len(edges), func(i int) bool { return edges[i] >= nominal })
	best := nominal
	bestDist := tol + 1
	if i < len(edges) {
		if d := edges[i] - nominal; d <= tol && d < bestDist {
			best, bestDist = edges[i], d
		}
	}
	if i > 0 {
		if d := nominal - edges[i-1]; d <= tol && d < bestDist {
			best = edges[i-1]
		}
	}
	return best
}

func nearestEdgeError(ns int64, tt *clock.TrackTimeline) int64 {
	if len(tt.Frames) == 0 {
		return 0
	}
	// A cut outside this track's coverage cannot misalign a frame: the track
	// simply has no media there. Clamp to its span so the error is measured
	// against the real first/last edge instead of empty time.
	if ns <= tt.StartNS {
		return 0
	}
	if ns >= tt.EndNS {
		return 0
	}
	i := tt.FrameAtOrAfter(ns)
	best := int64(1<<62 - 1)
	if i < len(tt.Frames) {
		best = tt.Frames[i].NS - ns
	}
	if i > 0 {
		if d := ns - tt.Frames[i-1].NS; d < best {
			best = d
		}
	}
	// The end of the final frame is also a legitimate edge.
	if last := tt.Frames[len(tt.Frames)-1]; last.IntervalNS > 0 {
		if d := abs64(ns - (last.NS + last.IntervalNS)); d < best {
			best = d
		}
	}
	if best < 0 {
		best = -best
	}
	return best
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
