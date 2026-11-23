// Package timeline composes per-track aligned samples into a single
// output timeline. Frame placement is driven by sample timestamps (not
// a nominal frame rate), boundary ownership follows a fixed half-open
// interval rule, and every output frame stays traceable to its track,
// source byte offset and timebase.
package timeline

import (
	"sort"
)

// SampleRef is one aligned sample on the reference timeline.
type SampleRef struct {
	Seq       int    `json:"seq"`
	RefStart  int64  `json:"ref_start"` // reference nanoseconds
	SrcOffset int64  `json:"src_offset"`
	Length    int64  `json:"length"`
	Keyframe  bool   `json:"keyframe"`
	Digest    string `json:"digest"`
}

// Track is one aligned input track. Higher Priority wins overlaps.
type Track struct {
	ID       string
	Priority int
	Samples  []SampleRef // in capture order (Seq ascending)
}

// TrackReport counts timestamp anomalies isolated during composition.
type TrackReport struct {
	TrackID    string `json:"track"`
	Aligned    bool   `json:"aligned"`
	Kept       int    `json:"kept"`
	Duplicates int    `json:"duplicates"`
	Reversed   int    `json:"reversed"`
	Missing    int    `json:"missing"`
}

// Frame is one output frame, traceable to its source sample.
type Frame struct {
	Index     int    `json:"index"`
	RefStart  int64  `json:"ref_start"`
	RefEnd    int64  `json:"ref_end"`
	TrackID   string `json:"track"`
	SampleSeq int    `json:"sample_seq"`
	SrcOffset int64  `json:"src_offset"`
}

// Result is the composed timeline plus per-track anomaly reports.
type Result struct {
	Frames  []Frame
	Reports []TrackReport
}

// sanitize drops duplicate and reversed timestamps (counting both) and
// counts missing samples from gaps against the median delta.
func sanitize(tr Track, aligned bool) ([]SampleRef, TrackReport) {
	rep := TrackReport{TrackID: tr.ID, Aligned: aligned}
	if len(tr.Samples) == 0 {
		return nil, rep
	}
	kept := make([]SampleRef, 0, len(tr.Samples))
	kept = append(kept, tr.Samples[0])
	for _, s := range tr.Samples[1:] {
		prev := kept[len(kept)-1].RefStart
		switch {
		case s.RefStart == prev:
			rep.Duplicates++
		case s.RefStart < prev:
			rep.Reversed++
		default:
			kept = append(kept, s)
		}
	}
	rep.Kept = len(kept)
	// Median forward delta of the kept samples.
	if len(kept) >= 2 {
		deltas := make([]int64, 0, len(kept)-1)
		for i := 1; i < len(kept); i++ {
			deltas = append(deltas, kept[i].RefStart-kept[i-1].RefStart)
		}
		sort.Slice(deltas, func(i, j int) bool { return deltas[i] < deltas[j] })
		median := deltas[len(deltas)/2]
		if median > 0 {
			for _, d := range deltas {
				if d > 2*median {
					rep.Missing += int((d+median/2)/median) - 1
				}
			}
		}
	}
	return kept, rep
}

// Compose merges the tracks into one output frame sequence. Each kept
// sample covers the half-open interval [start, next start) on its
// track; output frame boundaries are the union of all interval starts,
// and each frame is owned by the highest-priority sample covering its
// start. A timestamp exactly on a boundary belongs to the interval
// that starts there.
func Compose(tracks []Track, aligned []bool) *Result {
	type tstate struct {
		id       string
		priority int
		samples  []SampleRef
		median   int64
	}
	var states []tstate
	res := &Result{}
	var boundaries []int64
	for i, tr := range tracks {
		al := true
		if i < len(aligned) {
			al = aligned[i]
		}
		kept, rep := sanitize(tr, al)
		res.Reports = append(res.Reports, rep)
		if len(kept) == 0 {
			continue
		}
		st := tstate{id: tr.ID, priority: tr.Priority, samples: kept}
		if len(kept) >= 2 {
			deltas := make([]int64, 0, len(kept)-1)
			for k := 1; k < len(kept); k++ {
				deltas = append(deltas, kept[k].RefStart-kept[k-1].RefStart)
			}
			sort.Slice(deltas, func(a, b int) bool { return deltas[a] < deltas[b] })
			st.median = deltas[len(deltas)/2]
		}
		for _, s := range kept {
			boundaries = append(boundaries, s.RefStart)
		}
		states = append(states, st)
	}
	if len(boundaries) == 0 {
		return res
	}
	sort.Slice(boundaries, func(i, j int) bool { return boundaries[i] < boundaries[j] })
	uniq := boundaries[:1]
	for _, b := range boundaries[1:] {
		if b != uniq[len(uniq)-1] {
			uniq = append(uniq, b)
		}
	}
	for i, start := range uniq {
		end := start
		if i+1 < len(uniq) {
			end = uniq[i+1]
		}
		// Highest-priority sample whose interval covers `start` wins.
		best := -1
		for si := range states {
			st := &states[si]
			k := sort.Search(len(st.samples), func(k int) bool {
				return st.samples[k].RefStart > start
			}) - 1
			if k < 0 {
				continue
			}
			s := st.samples[k]
			sEnd := endOf(st.samples, k, st.median)
			if start < s.RefStart || start >= sEnd {
				continue
			}
			if best < 0 || st.priority > states[best].priority {
				best = si
			}
		}
		if best < 0 {
			continue // uncovered boundary; cannot happen for sample starts
		}
		st := &states[best]
		k := sort.Search(len(st.samples), func(k int) bool {
			return st.samples[k].RefStart > start
		}) - 1
		s := st.samples[k]
		res.Frames = append(res.Frames, Frame{
			Index: len(res.Frames), RefStart: start, RefEnd: end,
			TrackID: st.id, SampleSeq: s.Seq, SrcOffset: s.SrcOffset,
		})
	}
	return res
}

// endOf returns the end of sample k's interval: the next sample's
// start, or one median delta after the last sample.
func endOf(samples []SampleRef, k int, median int64) int64 {
	if k+1 < len(samples) {
		return samples[k+1].RefStart
	}
	if median > 0 {
		return samples[k].RefStart + median
	}
	return samples[k].RefStart + 1
}
