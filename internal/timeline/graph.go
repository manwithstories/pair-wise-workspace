// Package timeline builds explicit per-track timelines from parsed
// playlists and provides the dependency graph used by the pipeline
// scheduler.
package timeline

import (
	"fmt"
	"sort"

	"mediacore/internal/clock"
	"mediacore/internal/manifest"
)

// TSegment is a segment placed on the explicit timeline.
type TSegment struct {
	TrackID string
	Index   int
	URI     string
	Start   clock.Tick
	End     clock.Tick
	Gap     bool
}

// Dur returns the segment duration.
func (s TSegment) Dur() clock.Tick {
	d, err := s.End.Sub(s.Start)
	if err != nil {
		return clock.Zero()
	}
	return d
}

// TrackTimeline is the explicit timeline of one track.
type TrackTimeline struct {
	Meta     manifest.TrackMeta
	Segments []TSegment
}

// Timeline holds one explicit timeline per track.
type Timeline struct {
	Format string
	Tracks []TrackTimeline
}

// Build assigns absolute start/end ticks to every segment. Segment
// durations come from the manifest; gaps keep their declared span.
func Build(pl *manifest.Playlist) (*Timeline, error) {
	const op = "timeline-build"
	tl := &Timeline{Format: pl.Format}
	for _, tr := range pl.Tracks {
		tt := TrackTimeline{Meta: tr.TrackMeta}
		cur := clock.Zero()
		for _, s := range tr.Segments {
			if s.Dur.Sign() < 0 {
				return nil, clock.At(clock.CodeNegativeDuration, op, tr.ID, s.Index,
					"negative duration %s", s.Dur)
			}
			end, err := cur.Add(s.Dur)
			if err != nil {
				return nil, clock.Wrap(clock.CodeTickOverflow, op, err)
			}
			tt.Segments = append(tt.Segments, TSegment{
				TrackID: tr.ID,
				Index:   s.Index,
				URI:     s.URI,
				Start:   cur,
				End:     end,
				Gap:     s.Gap,
			})
			cur = end
		}
		tl.Tracks = append(tl.Tracks, tt)
	}
	return tl, nil
}

// Track returns the timeline of one track, or nil.
func (tl *Timeline) Track(id string) *TrackTimeline {
	for i := range tl.Tracks {
		if tl.Tracks[i].Meta.ID == id {
			return &tl.Tracks[i]
		}
	}
	return nil
}

// AlignmentMs returns the maximum absolute difference of track start
// times between two timelines, matched by track ID, in milliseconds.
// Leading gap segments (declared offsets) are skipped.
func AlignmentMs(a, b *Timeline) float64 {
	maxDiff := 0.0
	for _, ta := range a.Tracks {
		tb := b.Track(ta.Meta.ID)
		if tb == nil {
			continue
		}
		sa, okA := firstMedia(ta.Segments)
		sb, okB := firstMedia(tb.Segments)
		if !okA || !okB {
			continue
		}
		diff, err := sa.Start.Sub(sb.Start)
		if err != nil {
			continue
		}
		if ms := abs(diff.Millis()); ms > maxDiff {
			maxDiff = ms
		}
	}
	return maxDiff
}

func firstMedia(segs []TSegment) (TSegment, bool) {
	for _, s := range segs {
		if !s.Gap {
			return s, true
		}
	}
	return TSegment{}, false
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// Node is a vertex of the pipeline dependency graph.
type Node struct {
	Name string
	Deps []string
}

// TopoSort orders nodes so dependencies come first. The order is
// deterministic: among ready nodes the input order wins. A cycle or
// an unknown dependency is an error.
func TopoSort(nodes []Node) ([]string, error) {
	index := make(map[string]int, len(nodes))
	indeg := make(map[string]int, len(nodes))
	dependents := make(map[string][]string, len(nodes))
	for i, n := range nodes {
		if _, dup := index[n.Name]; dup {
			return nil, fmt.Errorf("timeline: duplicate node %q", n.Name)
		}
		index[n.Name] = i
	}
	for _, n := range nodes {
		indeg[n.Name] = len(n.Deps)
		for _, d := range n.Deps {
			if _, ok := index[d]; !ok {
				return nil, fmt.Errorf("timeline: node %q depends on unknown %q", n.Name, d)
			}
			dependents[d] = append(dependents[d], n.Name)
		}
	}
	var order []string
	ready := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if indeg[n.Name] == 0 {
			ready = append(ready, n.Name)
		}
	}
	for len(ready) > 0 {
		sort.SliceStable(ready, func(i, j int) bool { return index[ready[i]] < index[ready[j]] })
		name := ready[0]
		ready = ready[1:]
		order = append(order, name)
		for _, dep := range dependents[name] {
			indeg[dep]--
			if indeg[dep] == 0 {
				ready = append(ready, dep)
			}
		}
	}
	if len(order) != len(nodes) {
		return nil, fmt.Errorf("timeline: dependency cycle detected")
	}
	return order, nil
}
