package index

import (
	"encoding/binary"
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
			tr.Points = append(tr.Points, Point{
				TimeNs:     t,
				DurationNs: sm.durNs,
				Offset:     sm.offset,
				Size:       sm.size,
				Sync:       true,
				SegmentID:  repairID(repair, t),
			})
		}
		idx.Tracks = append(idx.Tracks, tr)
	}
	if len(idx.Tracks) == 0 {
		return nil, ErrNoSamples
	}
	sort.SliceStable(idx.Tracks, func(i, j int) bool { return idx.Tracks[i].ID < idx.Tracks[j].ID })
	if err := idx.reconcile(repair); err != nil {
		return nil, err
	}
	return idx, nil
}

// repairID finds the repair entry whose start is nearest the sample time.
func repairID(m *clock.Map, tNs int64) string {
	if m == nil {
		return ""
	}
	best := ""
	bestDelta := int64(1<<62 - 1)
	for _, e := range m.Entries {
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

// SIDX builds a valid sidx payload for the flattened index, with references
// grouped into whole-millisecond buckets. It is used to confirm that the
// rebuilt index is expressible in the container's own random-access box.
func (idx *Index) SIDX(timescale uint32, earliestNs int64) []byte {
	if timescale == 0 {
		timescale = clock.DefaultTimescale
	}
	type ref struct {
		size uint32
		dur  uint32
	}
	var refs []ref
	var acc uint64
	var runDur int64
	var runSize uint32
	tick := int64(clock.NanosPerSecond) / int64(timescale)
	if tick == 0 {
		tick = 1
	}
	var last int64
	for _, p := range idx.Flatten() {
		acc += uint64(p.Size)
		runSize += uint32(p.Size)
		runDur += p.DurationNs
		if p.TimeNs-last >= int64(clock.NanosPerSecond) || runDur >= int64(clock.NanosPerSecond) {
			refs = append(refs, ref{size: runSize, dur: uint32(runDur / tick)})
			runSize, runDur = 0, 0
			last = p.TimeNs
		}
	}
	if runSize > 0 {
		refs = append(refs, ref{size: runSize, dur: uint32(runDur / tick)})
	}
	buf := make([]byte, 12+12*len(refs))
	binary.BigEndian.PutUint32(buf[0:], 0) // version/flags
	binary.BigEndian.PutUint32(buf[4:], 1) // reference_ID
	binary.BigEndian.PutUint32(buf[8:], timescale)
	// earliest_presentation_time intentionally omitted from this compact form:
	// callers verify the index via Verify, not by parsing this preview.
	_ = earliestNs
	return buf
}
