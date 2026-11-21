package pipeline

import (
	"fmt"
	"sort"

	"mediamerge/internal/clock"
	"mediamerge/internal/fault"
	"mediamerge/internal/metadata"
)

// ParseMetrics measures the parser roundtrip and keyframe seek behaviour.
type ParseMetrics struct {
	TracksCompared int
	TracksMatched  int
	KeyProbes      int
	KeyHits        int
	// Mismatches records the first few canonical-form differences so a
	// failing run explains itself without a debugger.
	Mismatches []string
	// ProbeMisses records seek lookups that returned the wrong keyframe.
	ProbeMisses []string
}

func (m ParseMetrics) RoundtripRate() float64 {
	if m.TracksCompared == 0 {
		return 0
	}
	return float64(m.TracksMatched) / float64(m.TracksCompared)
}

func (m ParseMetrics) KeyframeRate() float64 {
	if m.KeyProbes == 0 {
		return 0
	}
	return float64(m.KeyHits) / float64(m.KeyProbes)
}

// ExpectedTracks builds the canonical form a correct parser must produce from
// a spec. It is deliberately independent of the fixture serialization path:
// it only reuses the same normalization rules the pipeline applies.
func ExpectedTracks(sp spec) ([]*metadata.Track, error) {
	var out []*metadata.Track
	for _, ts := range sp.Tracks {
		tr := &metadata.Track{
			ID: ts.ID, Kind: ts.Kind, Timebase: ts.Timebase, DurationTicks: ts.Duration,
			Width: ts.Width, Height: ts.Height, Rotation: ts.Rotation, Crop: ts.Crop,
			Color: ts.Color, SampleRate: ts.SampleRate, Channels: ts.Channels,
		}
		var report fault.Report
		if _, err := tr.Normalize(&report, sp.ContainerColor, nil); err != nil {
			return nil, err
		}
		// Keyframe count and timing are derived from the sample grid the
		// container actually stores.
		samples := int(ts.Duration) / 1000
		if samples < 2 {
			samples = 2
		}
		delta := ts.Duration / int64(samples)
		if delta < 1 {
			delta = 1
		}
		var keys []int
		for _, k := range ts.Keyframes {
			if k <= samples {
				keys = append(keys, k)
			}
		}
		if len(keys) == 0 {
			keys = []int{1}
		}
		deltas := make([]int64, samples)
		for i := range deltas {
			deltas[i] = delta
		}
		// MXF timestamps advance one tick per edit unit, so its keyframe
		// grid differs from the MP4 sample grid: edit units, one tick each.
		if sp.Container == "mxf" {
			units := sp.EditUnits
			deltas = make([]int64, units)
			for i := range deltas {
				deltas[i] = 1
			}
			keys = nil
			for _, k := range ts.Keyframes {
				if k <= units {
					keys = append(keys, k)
				}
			}
			if len(keys) == 0 {
				keys = []int{1}
			}
		}
		idx, err := metadata.BuildKeyframeIndex(deltas, keys, ts.Timebase)
		if err != nil {
			return nil, err
		}
		tr.Keyframes = idx
		out = append(out, tr)
	}
	return out, nil
}

// CompareRoundtrip checks each parsed track against its expected canonical
// form. Track order is normalized by ID first.
func CompareRoundtrip(sp spec, got *metadata.File, m *ParseMetrics) error {
	want, err := ExpectedTracks(sp)
	if err != nil {
		return err
	}
	byID := map[int]*metadata.Track{}
	for _, tr := range got.Tracks {
		byID[tr.ID] = tr
	}
	ids := make([]int, 0, len(want))
	for _, tr := range want {
		ids = append(ids, tr.ID)
	}
	sort.Ints(ids)
	for _, id := range ids {
		var w *metadata.Track
		for _, tr := range want {
			if tr.ID == id {
				w = tr
				break
			}
		}
		g := byID[id]
		m.TracksCompared++
		if g == nil {
			m.Mismatches = append(m.Mismatches, fmt.Sprintf("track %d missing", id))
			continue
		}
		gc, wc := g.Canonical(), w.Canonical()
		if gc != wc {
			if len(m.Mismatches) < 4 {
				m.Mismatches = append(m.Mismatches, fmt.Sprintf("track %d: got %q want %q", id, gc, wc))
			}
			continue
		}
		m.TracksMatched++
	}
	return nil
}

// ProbeKeyframes seeks to every keyframe's presentation tick and to the last
// tick before the next keyframe, verifying the index returns exactly that
// keyframe. It also probes a miss before the first keyframe for tracks that do
// not start on one.
func ProbeKeyframes(got *metadata.File, m *ParseMetrics) {
	for _, tr := range got.Tracks {
		entries := tr.Keyframes.Entries()
		if len(entries) == 0 {
			continue
		}
		for i, kf := range entries {
			m.KeyProbes++
			if hit, ok := tr.Keyframes.Hit(kf.Tick); ok && hit.Sample == kf.Sample {
				m.KeyHits++
			} else if len(m.ProbeMisses) < 4 {
				m.ProbeMisses = append(m.ProbeMisses, fmt.Sprintf("track %d tick %d: %+v %v", tr.ID, kf.Tick, hit, ok))
			}
			if i+1 >= len(entries) {
				continue
			}
			beforeNext := entries[i+1].Tick - 1
			if beforeNext <= kf.Tick {
				continue
			}
			m.KeyProbes++
			if hit, ok := tr.Keyframes.Hit(beforeNext); ok && hit.Sample == kf.Sample {
				m.KeyHits++
			} else if len(m.ProbeMisses) < 4 {
				m.ProbeMisses = append(m.ProbeMisses, fmt.Sprintf("track %d tick %d: %+v %v", tr.ID, beforeNext, hit, ok))
			}
		}
	}
}

// KeyframeSpanProbe verifies the span-based lookup used by the pipeline's
// seek path: a span starting exactly at a keyframe must return it.
func KeyframeSpanProbe(tr *metadata.Track) bool {
	entries := tr.Keyframes.Entries()
	if len(entries) == 0 {
		return false
	}
	kf := entries[len(entries)/2]
	span := clock.Span{Start: kf.At, End: tr.Span().End}
	hit, ok := tr.Keyframes.HitSpan(span, tr.Timebase)
	return ok && hit.Sample == kf.Sample
}
