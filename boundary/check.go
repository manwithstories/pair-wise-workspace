// Package boundary cross-checks GOP, subtitle and recovery-layer boundaries
// and marks conflict intervals as non-cuttable.
package boundary

import (
	"fmt"
	"sort"

	"medcheck/fault"
	"medcheck/index"
	"medcheck/subtitle"
)

// LayerState is the boundary-relevant result from one representation.
type LayerState struct {
	Name        string         `json:"name"`
	Hashes      map[int]string `json:"hashes"`
	BadSegments []int          `json:"bad_segments"`
}

// Interval is a time range or point that must not be handed to an editor.
type Interval struct {
	Segment    int    `json:"segment"`
	StartMS    int64  `json:"start_ms"`
	EndMS      int64  `json:"end_ms"`
	SourceLine int    `json:"source_line,omitempty"`
	Reason     string `json:"reason"`
}

// Report contains safe GOP cut points and unsafe conflict intervals.
type Report struct {
	CuttableBoundaries []int64    `json:"cuttable_boundaries"`
	Uncuttable         []Interval `json:"uncuttable"`
	ConflictCount      int        `json:"conflict_count"`
	HashOK             bool       `json:"hash_ok"`
}

// Check performs all cross-boundary and hash checks. Faults are added to fm so
// the shared error model propagates to the final report.
func Check(idx *index.Index, doc *subtitle.Document, layers []LayerState, fm *fault.Model) *Report {
	r := &Report{HashOK: true}
	if idx == nil {
		r.HashOK = false
		return r
	}

	bad := map[int]bool{}
	for _, f := range fm.UnrecoveredBadSegments() {
		bad[f.Segment] = true
	}
	for _, layer := range layers {
		for _, seg := range layer.BadSegments {
			bad[seg] = true
		}
	}

	for _, seg := range idx.Segments {
		if bad[seg.Index] {
			r.addUncuttable(Interval{Segment: seg.Index, StartMS: seg.StartMS, EndMS: seg.EndMS,
				Reason: "unrecovered segment boundary"})
			fm.Add(fault.Fault{Kind: fault.BoundaryConflict, Segment: seg.Index,
				Span: fault.Span{StartMS: seg.StartMS, EndMS: seg.EndMS}, Message: "bad GOP segment marked uncuttable"})
		}
		for _, layer := range layers {
			got := layer.Hashes[seg.Index]
			if len(got) != 64 || got != seg.Hash {
				r.HashOK = false
				r.addUncuttable(Interval{Segment: seg.Index, StartMS: seg.StartMS, EndMS: seg.EndMS,
					Reason: fmt.Sprintf("layer %s segment hash mismatch", layer.Name)})
			}
		}
	}

	if doc != nil {
		for _, cue := range doc.Cues {
			for _, seg := range idx.Segments {
				if !bad[seg.Index] {
					continue
				}
				if start, end, ok := overlap(cue.StartMS, cue.EndMS, seg.StartMS, seg.EndMS); ok {
					r.addUncuttable(Interval{Segment: seg.Index, StartMS: start, EndMS: end,
						SourceLine: cue.SourceLine, Reason: "subtitle overlaps damaged segment"})
					fm.Add(fault.Fault{Kind: fault.BoundaryConflict, Segment: seg.Index, SourceLine: cue.SourceLine,
						Span: fault.Span{StartMS: start, EndMS: end}, Message: "cue/damaged-segment overlap"})
				}
			}
		}
		for _, issue := range doc.Issues {
			if issue.Kind == fault.OverlappingCue || issue.Kind == fault.TruncatedCue {
				r.addUncuttable(Interval{StartMS: issue.StartMS, EndMS: issue.EndMS, SourceLine: issue.SourceLine,
					Reason: string(issue.Kind)})
				fm.Add(fault.Fault{Kind: fault.BoundaryConflict, SourceLine: issue.SourceLine,
					Span: fault.Span{StartMS: issue.StartMS, EndMS: issue.EndMS}, Message: issue.Message})
			}
		}
	}

	// A GOP boundary is cuttable only when no cue contains it and no unsafe
	// interval covers it.
	for _, seg := range idx.Segments {
		if seg.Index == 0 {
			continue
		}
		at := seg.StartMS
		reason := ""
		for _, cue := range doc.GetCues() {
			if cue.StartMS < at && at < cue.EndMS {
				reason = "subtitle sentence crosses GOP boundary"
				break
			}
		}
		if reason == "" && r.covered(at) {
			reason = "boundary lies in conflict interval"
		}
		if reason != "" {
			r.addUncuttable(Interval{Segment: seg.Index, StartMS: at, EndMS: at, Reason: reason})
		} else {
			r.CuttableBoundaries = append(r.CuttableBoundaries, at)
		}
	}

	sort.Slice(r.Uncuttable, func(i, j int) bool {
		if r.Uncuttable[i].StartMS != r.Uncuttable[j].StartMS {
			return r.Uncuttable[i].StartMS < r.Uncuttable[j].StartMS
		}
		return r.Uncuttable[i].SourceLine < r.Uncuttable[j].SourceLine
	})
	r.ConflictCount = len(r.Uncuttable)
	return r
}

func (r *Report) addUncuttable(in Interval) {
	for _, existing := range r.Uncuttable {
		if existing == in {
			return
		}
	}
	r.Uncuttable = append(r.Uncuttable, in)
}

func (r *Report) covered(ms int64) bool {
	for _, in := range r.Uncuttable {
		if in.StartMS <= ms && ms <= in.EndMS {
			return true
		}
	}
	return false
}

// IsCuttable reports whether an exact millisecond boundary is safe.
func (r *Report) IsCuttable(ms int64) bool {
	for _, safe := range r.CuttableBoundaries {
		if safe == ms {
			return true
		}
	}
	return false
}

func overlap(aStart, aEnd, bStart, bEnd int64) (int64, int64, bool) {
	start := maxInt64(aStart, bStart)
	end := minInt64(aEnd, bEnd)
	if start >= end {
		return 0, 0, false
	}
	return start, end, true
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
