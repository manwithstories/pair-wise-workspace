// Package index turns a validated MP4 summary into a keyframe table on the
// unified rational time base.  Every entry knows its sample number, its
// exact tick timestamp, and the representation (rate) it came from.  When a
// track has no sync-sample table every sample is treated as a keyframe,
// which is what the format means by an absent stss.
package index

import (
	"fmt"

	"mediapack/internal/clock"
	"mediapack/internal/fault"
	"mediapack/internal/mp4"
)

// Limits bounds index size.
type Limits struct{ MaxEntries int }

// DefaultLimits keeps the index small enough to reason about by hand.
func DefaultLimits() Limits { return Limits{MaxEntries: 100000} }

// Keyframe is one indexed sample.
type Keyframe struct {
	Sample         int
	Ticks          clock.Ticks
	DeltaTicks     clock.Ticks
	Representation string
}

// Table indexes one track's keyframes.
type Table struct {
	Track       uint32
	Timescale   uint32
	Durations   []clock.Ticks // per-sample duration in ticks
	Starts      []clock.Ticks // per-sample start timestamp in ticks
	Keyframes   []Keyframe
	EverySample bool
}

// Build indexes the requested track of summary.  The representation name is
// recorded verbatim so a multi-rate manifest can trace each entry.
func Build(summary *mp4.Summary, trackID uint32, representation string, limits Limits) (*Table, error) {
	if summary == nil {
		return nil, fault.New(fault.ClassInternal, "index", "*", "nil-summary", "nil summary")
	}
	var track *mp4.Track
	for i := range summary.Tracks {
		if summary.Tracks[i].ID == trackID {
			track = &summary.Tracks[i]
			break
		}
	}
	if track == nil {
		return nil, fault.New(fault.ClassInput, "index", fmt.Sprintf("%d", trackID), "missing-track",
			fmt.Sprintf("track %d is not present", trackID))
	}
	if track.Timescale == 0 {
		return nil, fault.New(fault.ClassInput, "index", fmt.Sprintf("%d", trackID), "zero-timescale",
			"track declares a zero timescale")
	}
	if len(track.SampleDeltas) > limits.MaxEntries {
		return nil, fault.New(fault.ClassLimit, "index", fmt.Sprintf("%d", trackID), "too-many-samples",
			fmt.Sprintf("%d samples exceeds limit %d", len(track.SampleDeltas), limits.MaxEntries))
	}

	t := &Table{Track: trackID, Timescale: track.Timescale}
	ts, _ := clock.New(1, int64(track.Timescale))
	cursor := clock.MustNew(0, 1)
	for i, d := range track.SampleDeltas {
		delta, err := ts.Mul(clock.MustNew(int64(d), 1))
		if err != nil {
			return nil, fault.Wrap(fault.ClassInput, "index", fmt.Sprintf("%d", trackID),
				"time-overflow", fmt.Sprintf("sample %d duration overflows the tick range", i+1), err)
		}
		t.Durations = append(t.Durations, delta)
		t.Starts = append(t.Starts, cursor)
		next, err := cursor.Add(delta)
		if err != nil {
			return nil, fault.Wrap(fault.ClassInput, "index", fmt.Sprintf("%d", trackID),
				"time-overflow", fmt.Sprintf("sample %d timestamp overflows the tick range", i+1), err)
		}
		cursor = next
	}

	if len(track.Keyframes) == 0 {
		t.EverySample = true
		for i := range track.SampleDeltas {
			t.Keyframes = append(t.Keyframes, Keyframe{Sample: i + 1, Ticks: t.Starts[i],
				DeltaTicks: t.Durations[i], Representation: representation})
		}
		return t, nil
	}

	seen := make(map[int]bool, len(track.Keyframes))
	for _, n := range track.Keyframes {
		if n < 1 || n > len(track.SampleDeltas) {
			return nil, fault.New(fault.ClassInput, "index", fmt.Sprintf("%d", trackID),
				"keyframe-out-of-range", fmt.Sprintf("sync sample %d is outside 1..%d", n, len(track.SampleDeltas)))
		}
		if seen[n] {
			return nil, fault.New(fault.ClassInput, "index", fmt.Sprintf("%d", trackID),
				"duplicate-keyframe", fmt.Sprintf("sync sample %d is listed twice", n))
		}
		seen[n] = true
	}
	for i := range track.SampleDeltas {
		if !seen[i+1] {
			continue
		}
		t.Keyframes = append(t.Keyframes, Keyframe{Sample: i + 1, Ticks: t.Starts[i],
			DeltaTicks: t.Durations[i], Representation: representation})
	}
	if len(t.Keyframes) == 0 {
		return nil, fault.New(fault.ClassInput, "index", fmt.Sprintf("%d", trackID),
			"no-keyframes", "track has no sync samples")
	}
	return t, nil
}

// At returns the keyframe covering ticks, or false when ticks precedes the
// first keyframe.
func (t *Table) At(ticks clock.Ticks) (Keyframe, bool) {
	var found Keyframe
	ok := false
	for _, k := range t.Keyframes {
		if k.Ticks.Cmp(ticks) > 0 {
			break
		}
		found, ok = k, true
	}
	return found, ok
}

// Duration returns the exact end timestamp of the track.
func (t *Table) Duration() clock.Ticks {
	if len(t.Starts) == 0 {
		return clock.MustNew(0, 1)
	}
	last := len(t.Starts) - 1
	end, err := t.Starts[last].Add(t.Durations[last])
	if err != nil {
		return t.Starts[last]
	}
	return end
}

// Coverage returns the fraction of keyframe boundaries in want that are also
// present in got, as a percentage.  It is used to report keyframe_hit_pct
// when several representations must agree on the same boundaries.
func Coverage(got []Keyframe, want []Keyframe) float64 {
	if len(want) == 0 {
		if len(got) == 0 {
			return 100
		}
		return 0
	}
	type point struct{ num, den int64 }
	index := make(map[point]bool, len(got))
	for _, k := range got {
		index[point{k.Ticks.Num(), k.Ticks.Den()}] = true
	}
	hit := 0
	for _, k := range want {
		if index[point{k.Ticks.Num(), k.Ticks.Den()}] {
			hit++
		}
	}
	return float64(hit) / float64(len(want)) * 100
}
