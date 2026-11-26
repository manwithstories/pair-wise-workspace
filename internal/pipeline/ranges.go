package pipeline

import (
	"sort"

	"mediad/internal/fault"
	"mediad/internal/manifest"
	"mediad/internal/timeline"
)

// buildRanges merges per-fault corrupt frame spans into half-open time
// ranges in seconds.
func buildRanges(clock timeline.Clock, m *manifest.Manifest, fs []fault.Fault) []timeline.HalfOpen {
	steps := map[string]int64{}
	ptsOf := map[string]map[int64]int64{}
	for i := range m.Tracks {
		t := &m.Tracks[i]
		ptsOf[t.ID] = map[int64]int64{}
		for _, f := range t.Frames {
			ptsOf[t.ID][f.Seq] = f.PTS
		}
		if t.Kind != "video" || t.FPS == nil {
			continue
		}
		if step, err := clock.StepPTS(*t.FPS); err == nil {
			steps[t.ID] = step
		}
	}
	var ranges []timeline.HalfOpen
	for _, f := range fs {
		step, ok := steps[f.Track]
		if !ok || f.Frame < 0 {
			continue
		}
		pts, ok := ptsOf[f.Track][f.Frame]
		if !ok {
			continue
		}
		ranges = append(ranges, timeline.HalfOpen{
			Start: clock.Seconds(pts), End: clock.Seconds(pts + step)})
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].Start < ranges[j].Start })
	var merged []timeline.HalfOpen
	for _, r := range ranges {
		if n := len(merged); n > 0 && r.Start <= merged[n-1].End {
			if r.End > merged[n-1].End {
				merged[n-1].End = r.End
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

// countRecovered counts valid frames processed after the first corrupt
// frame of their track.
func countRecovered(res *Result) int {
	firstCorrupt := map[string]int64{}
	for _, f := range res.Faults {
		if f.Frame < 0 {
			continue
		}
		if cur, ok := firstCorrupt[f.Track]; !ok || f.Frame < cur {
			firstCorrupt[f.Track] = f.Frame
		}
	}
	n := 0
	for _, st := range res.FrameStats {
		if fc, ok := firstCorrupt[st.Track]; ok && st.Seq > fc {
			n++
		}
	}
	return n
}
