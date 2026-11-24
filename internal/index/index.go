package index

import (
	"fmt"
	"sort"

	"mediascan/internal/clock"
)

// Build assembles the random-access index. Progressive tracks yield one point
// per sync sample; fragmented tracks yield one point per moof whose base
// media decode time is reconciled against the repair mapping.
func (s *source) Build(repair *clock.Map) (*Index, error) {
	idx := &Index{Warnings: append([]string{}, s.warnings...), Truncated: s.truncated}
	// One origin shared by every track, so a genuine audio/video offset stays
	// visible after rebasing instead of being cancelled per track.
	var origin int64
	first := true
	for _, ts := range s.tracks {
		if len(ts.samples) == 0 {
			continue
		}
		if first || ts.samples[0].timeNs < origin {
			origin = ts.samples[0].timeNs
			first = false
		}
	}
	for _, ts := range s.tracks {
		if len(ts.samples) == 0 {
			continue
		}
		tr := Track{ID: ts.id, Timescale: ts.timescale, Handler: ts.handler, Samples: len(ts.samples)}
		for _, sm := range ts.samples {
			if !sm.sync {
				continue
			}
			t := sm.timeNs - origin
			id := repairID(repair, t)
			// A point that falls inside a quarantined segment is isolated: it
			// is omitted from the index and the omission is reported, never
			// hidden by a fabricated mapping.
			if repair != nil {
				if e, ok := repair.SegmentAt(t); ok && !e.Available {
					idx.Quarantined++
					continue
				}
			}
			tr.Points = append(tr.Points, Point{
				TimeNs:     t,
				DurationNs: sm.durNs,
				Offset:     sm.offset,
				Size:       sm.size,
				Sync:       true,
				SegmentID:  id,
			})
		}
		idx.Tracks = append(idx.Tracks, tr)
	}
	// Drop tracks whose every point was quarantined.
	kept := idx.Tracks[:0]
	for _, tr := range idx.Tracks {
		if len(tr.Points) > 0 {
			kept = append(kept, tr)
		}
	}
	idx.Tracks = kept
	if len(idx.Tracks) == 0 {
		return nil, ErrNoSamples
	}
	sort.SliceStable(idx.Tracks, func(i, j int) bool { return idx.Tracks[i].ID < idx.Tracks[j].ID })
	if err := idx.reconcile(repair); err != nil {
		return nil, err
	}
	return idx, nil
}

// repairID finds the repair entry that covers the sample time: the entry whose
// start is nearest, preferring available entries so a sample inside a
// quarantined gap maps to a real neighbouring segment rather than the gap.
func repairID(m *clock.Map, tNs int64) string {
	if m == nil {
		return ""
	}
	best := ""
	bestDelta := int64(1<<62 - 1)
	for _, e := range m.Entries {
		if !e.Available {
			continue
		}
		d := e.Start - tNs
		if d < 0 {
			d = -d
		}
		if d < bestDelta {
			bestDelta = d
			best = e.ID
		}
	}
	return best
}

// reconcile checks every point's time against the repair mapping. A point that
// falls between repaired segments is a hard error.
func (idx *Index) reconcile(m *clock.Map) error {
	if m == nil {
		return nil
	}
	for ti := range idx.Tracks {
		for pi := range idx.Tracks[ti].Points {
			p := &idx.Tracks[ti].Points[pi]
			e, ok := m.Lookup(p.SegmentID)
			if !ok {
				continue
			}
			if !e.Available {
				return fmt.Errorf("%w: point %d belongs to quarantined %s", ErrTimeMismatch, pi, p.SegmentID)
			}
			delta := p.TimeNs - e.Start
			if delta < 0 {
				delta = -delta
			}
			if delta > e.Duration && e.Duration > 0 {
				return fmt.Errorf("%w: point at %dns is %dns into segment %s", ErrTimeMismatch, p.TimeNs, delta, e.ID)
			}
		}
	}
	return nil
}

// Verify checks that every point lands on a valid sample boundary of the
// source file and that the recorded range covers real bytes.
func (s *source) Verify(idx *Index) Report {
	var rep Report
	bounds := make(map[uint32]map[int64]int64)
	for _, ts := range s.tracks {
		m := make(map[int64]int64, len(ts.samples))
		for _, sm := range ts.samples {
			m[sm.offset] = sm.size
		}
		bounds[ts.id] = m
	}
	for _, tr := range idx.Tracks {
		rep.Points += len(tr.Points)
		bm := bounds[tr.ID]
		for _, p := range tr.Points {
			if p.Sync {
				rep.SyncPoints++
			}
			size, ok := bm[p.Offset]
			if !ok {
				rep.Problems = append(rep.Problems, fmt.Sprintf("track %d: point at %d is not a sample boundary", tr.ID, p.Offset))
				rep.Mismatches++
				continue
			}
			if p.Size < size || p.Size <= 0 {
				rep.Problems = append(rep.Problems, fmt.Sprintf("track %d: point at %d declares %d bytes, sample is %d", tr.ID, p.Offset, p.Size, size))
				rep.Mismatches++
				continue
			}
			if p.Offset+p.Size > s.size {
				rep.Problems = append(rep.Problems, fmt.Sprintf("track %d: point at %d+%d past EOF", tr.ID, p.Offset, p.Size))
				rep.Mismatches++
			}
		}
	}
	rep.Valid = rep.Points > 0 && rep.Mismatches == 0 && !s.truncated
	if s.truncated {
		rep.Problems = append(rep.Problems, "source truncated")
	}
	return rep
}

// Flatten returns every point across tracks in track then time order.
func (idx *Index) Flatten() []Point {
	var out []Point
	for _, tr := range idx.Tracks {
		out = append(out, tr.Points...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TimeNs != out[j].TimeNs {
			return out[i].TimeNs < out[j].TimeNs
		}
		return out[i].Offset < out[j].Offset
	})
	return out
}
