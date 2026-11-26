// Package timeline converts parsed fragments to a common 90 kHz grid and
// decides where tfdt regressions are safely repairable.
package timeline

import (
	"fmt"

	"github.com/pairwise/relay/internal/isobmff"
	"github.com/pairwise/relay/internal/media"
)

// Cell is one rendition's position at one common segment index.
type Cell struct {
	Segment             uint64       `json:"segment"`
	Rendition           string       `json:"rendition"`
	Present             bool         `json:"present"`
	Gap                 bool         `json:"gap"`
	RawTFDT90k          int64        `json:"raw_tfdt_90k"`
	Start90k            int64        `json:"start_90k"`
	Duration90k         int64        `json:"duration_90k"`
	ObservedDuration90k int64        `json:"observed_duration_90k"`
	Repaired            bool         `json:"repaired"`
	Discontinuity       bool         `json:"discontinuity"`
	Fault               *media.Fault `json:"fault,omitempty"`
}

// Boundary is the synchronized common boundary for one segment index.
type Boundary struct {
	Index         uint64         `json:"index"`
	Start90k      int64          `json:"start_90k"`
	Duration90k   int64          `json:"duration_90k"`
	End90k        int64          `json:"end_90k"`
	Gap           bool           `json:"gap"`
	Discontinuity bool           `json:"discontinuity"`
	Cells         []Cell         `json:"cells"`
	Faults        []*media.Fault `json:"faults,omitempty"`
}

// Report is the result of aligning all renditions.
type Report struct {
	Segments        int            `json:"segments"`
	Aligned         int            `json:"aligned"`
	Boundaries      []Boundary     `json:"boundaries"`
	Discontinuities []uint64       `json:"discontinuities"`
	Faults          []*media.Fault `json:"faults,omitempty"`
}

// FragmentAt links a logical one-based segment to its parsed fragment.
type FragmentAt struct {
	Segment uint64
	Frag    *isobmff.Fragment
	Gap     bool
}

// TrackInput is one rendition in logical segment order.
type TrackInput struct {
	Rendition string
	Timescale uint32
	Fragments []FragmentAt
}

// Align builds common boundaries. duration90k is the exact common segment
// duration. A repairable tfdt regression is snapped to the proven grid; an
// unprovable regression records a fault and opens a common discontinuity.
func Align(tracks []TrackInput, segmentCount int, duration90k int64) (*Report, error) {
	if segmentCount <= 0 {
		return nil, media.NewFault(media.CodeInvalidArgument, media.FragmentID{}, -1, false, "segment count must be positive")
	}
	if duration90k <= 1 {
		return nil, media.NewFault(media.CodeInvalidArgument, media.FragmentID{}, -1, false, "duration must be > 1 tick")
	}
	if len(tracks) == 0 {
		return nil, media.NewFault(media.CodeInvalidArgument, media.FragmentID{}, -1, false, "at least one track is required")
	}

	report := &Report{Segments: segmentCount, Boundaries: make([]Boundary, 0, segmentCount)}
	cellsBySegment := make([][]Cell, segmentCount)
	commonFaultAt := make(map[uint64][]*media.Fault)
	commonDisc := make(map[uint64]bool)
	commonGap := make(map[uint64]bool)

	for _, track := range tracks {
		clock, err := media.NewClock(track.Timescale)
		if err != nil {
			return nil, err
		}
		bySegment := make(map[uint64]FragmentAt, len(track.Fragments))
		for _, f := range track.Fragments {
			bySegment[f.Segment] = f
		}

		var prevEnd int64
		havePrev := false
		for i := 1; i <= segmentCount; i++ {
			seg := uint64(i)
			expectedStart, err := media.CommonBoundary(seg-1, duration90k)
			if err != nil {
				return nil, err
			}
			cell := Cell{Segment: seg, Rendition: track.Rendition, Start90k: expectedStart, Duration90k: duration90k}
			at, ok := bySegment[seg]
			switch {
			case !ok || at.Gap || at.Frag == nil:
				cell.Gap = true
				cell.Fault = media.NewFault(media.CodeRecoveryBudget,
					media.FragmentID{Rendition: track.Rendition, Segment: seg}, -1, true,
					"segment unavailable after recovery budget")
				commonGap[seg] = true
				report.Faults = append(report.Faults, cell.Fault)
			default:
				cell.Present = true
				frag := at.Frag
				id := frag.ID
				id.Rendition = track.Rendition
				rawStart, convErr := clock.To90k(frag.TFDT)
				if convErr != nil {
					return nil, convErr
				}
				cell.RawTFDT90k = rawStart
				sampleDuration, continuous := SamplesContinuous(frag, clock, duration90k)
				cell.ObservedDuration90k = sampleDuration

				deviates := !media.WithinOneTick(rawStart, expectedStart)
				regresses := havePrev && rawStart < prevEnd-1
				if deviates && regresses {
					if continuous {
						cell.Repaired = true
						f := media.NewFault(media.CodeTFDTRegression, id, frag.TFDTOffset, true,
							"tfdt fell back to %d ticks; sample durations prove %d-tick duration, repaired to %d",
							rawStart, sampleDuration, expectedStart)
						cell.Fault = f
						report.Faults = append(report.Faults, f)
					} else {
						cell.Discontinuity = true
						f := media.NewFault(media.CodeTimelineBreak, id, frag.TFDTOffset, true,
							"tfdt regression to %d is not provably continuous; opening common discontinuity at %d",
							rawStart, seg)
						cell.Fault = f
						report.Faults = append(report.Faults, f)
						commonDisc[seg] = true
						commonFaultAt[seg] = append(commonFaultAt[seg], f)
					}
					cell.Start90k = expectedStart
				} else if deviates {
					// A forward jump or malformed timestamp cannot be
					// silently concatenated. Reset on the common grid and
					// mark a discontinuity.
					cell.Discontinuity = true
					f := media.NewFault(media.CodeTimelineBreak, id, frag.TFDTOffset, true,
						"tfdt start %d deviates from common boundary %d by %d ticks",
						rawStart, expectedStart, media.AbsTickDiff(rawStart, expectedStart))
					cell.Fault = f
					report.Faults = append(report.Faults, f)
					commonDisc[seg] = true
					commonFaultAt[seg] = append(commonFaultAt[seg], f)
					cell.Start90k = expectedStart
				}

				if !continuous && cell.Fault == nil {
					cell.Discontinuity = true
					f := media.NewFault(media.CodeTimelineBreak, id, -1, true,
						"sample durations total %d ticks, expected %d", sampleDuration, duration90k)
					cell.Fault = f
					report.Faults = append(report.Faults, f)
					commonDisc[seg] = true
					commonFaultAt[seg] = append(commonFaultAt[seg], f)
				}

				if !media.WithinOneTick(cell.Start90k, expectedStart) {
					commonDisc[seg] = true
				}
				prevEnd = cell.Start90k + duration90k
				havePrev = true
			}
			cellsBySegment[i-1] = append(cellsBySegment[i-1], cell)
		}
	}

	for i := 1; i <= segmentCount; i++ {
		seg := uint64(i)
		start, _ := media.CommonBoundary(seg-1, duration90k)
		b := Boundary{
			Index: seg, Start90k: start, Duration90k: duration90k,
			End90k: start + duration90k,
			Gap:    commonGap[seg], Discontinuity: commonDisc[seg],
			Cells: cellsBySegment[i-1], Faults: commonFaultAt[seg],
		}
		if b.Discontinuity {
			report.Discontinuities = append(report.Discontinuities, seg)
		}
		// A boundary is aligned when every rendition lands within one tick
		// after an explained repair or an explicit discontinuity reset.
		aligned := len(b.Cells) == len(tracks)
		for _, c := range b.Cells {
			if c.Gap {
				continue
			}
			if !media.WithinOneTick(c.Start90k, b.Start90k) {
				aligned = false
			}
			if !c.Discontinuity && !media.WithinOneTick(c.ObservedDuration90k, b.Duration90k) {
				aligned = false
			}
		}
		if aligned {
			report.Aligned++
		}
		report.Boundaries = append(report.Boundaries, b)
	}
	return report, nil
}

// GapSegments returns common boundaries containing an unavailable cell.
func (r *Report) GapSegments() []uint64 {
	var out []uint64
	for _, b := range r.Boundaries {
		if b.Gap {
			out = append(out, b.Index)
		}
	}
	return out
}

// SamplesContinuous proves that a trun has positive, non-overflowing sample
// durations whose total matches the expected common segment duration within
// the one-tick tolerance.
func SamplesContinuous(f *isobmff.Fragment, clock media.Clock, expected90k int64) (int64, bool) {
	if f == nil || len(f.Samples) == 0 {
		return 0, false
	}
	var total uint64
	for _, s := range f.Samples {
		if s.Duration == 0 {
			return 0, false
		}
		d := uint64(s.Duration)
		if d > ^uint64(0)-total {
			return 0, false
		}
		total += d
	}
	ticks, err := clock.To90k(total)
	if err != nil {
		return 0, false
	}
	return ticks, media.WithinOneTick(ticks, expected90k)
}

// ValidateRenditionCounts enforces equal segment counts before manifest build.
func ValidateRenditionCounts(counts map[string]int) error {
	want := -1
	for name, n := range counts {
		if want == -1 {
			want = n
		}
		if n != want {
			return media.NewFault(media.CodeInvalidArgument, media.FragmentID{Rendition: name}, -1, false,
				"rendition has %d segments, expected %d", n, want)
		}
	}
	if want == 0 {
		return fmt.Errorf("no rendition segments")
	}
	return nil
}
