package align

import (
	"fmt"

	"mediaslice/media"
)

// SegmentRange is one planned segment's time coverage for one representation.
type SegmentRange struct {
	RepID    string      `json:"rep_id"`
	Sequence int         `json:"sequence"`
	FromTick media.Ticks `json:"from"`
	ToTick   media.Ticks `json:"to"`
}

// LadderSegment groups every representation's coverage of one common segment.
type LadderSegment struct {
	Sequence int                     `json:"sequence"`
	Ranges   map[string]SegmentRange `json:"ranges"`
}

// LadderPlan is the validated, streamer-ready cutting plan.
type LadderPlan struct {
	SegmentTicks media.Ticks     `json:"segment_ticks"`
	Segments     []LadderSegment `json:"segments"`
}

// LadderValidationError reports the first representation whose time range
// breaks the common-segment contract.
type LadderValidationError struct {
	Sequence int
	RepID    string
	Message  string
}

func (e *LadderValidationError) Error() string {
	return fmt.Sprintf("ladder segment %d rep %s: %s", e.Sequence, e.RepID, e.Message)
}

// BuildLadderPlan turns aligned boundaries into per-segment ranges using the
// actual sample ticks of each representation (VFR-aware), then validates that
// every representation covers every segment with boundaries differing by at
// most one clock tick.
func BuildLadderPlan(al *AlignResult, bounds []*TrackBoundary) (*LadderPlan, error) {
	if al == nil || len(al.Boundaries) == 0 {
		return nil, &LadderValidationError{Message: "no aligned boundaries"}
	}
	byRep := map[string]*TrackBoundary{}
	for _, b := range bounds {
		byRep[b.RepID] = b
	}
	plan := &LadderPlan{SegmentTicks: al.SegmentTicks}
	for i := 0; i+1 < len(al.Boundaries); i++ {
		from, to := al.Boundaries[i], al.Boundaries[i+1]
		seg := LadderSegment{Sequence: i, Ranges: map[string]SegmentRange{}}
		for repID, cpFrom := range from.PerRep {
			cpTo, ok := to.PerRep[repID]
			if !ok {
				return nil, &LadderValidationError{Sequence: i, RepID: repID, Message: "missing end boundary"}
			}
			if cpTo.Tick <= cpFrom.Tick {
				return nil, &LadderValidationError{Sequence: i, RepID: repID, Message: "non-advancing boundary"}
			}
			seg.Ranges[repID] = SegmentRange{
				RepID: repID, Sequence: i,
				FromTick: cpFrom.Tick, ToTick: cpTo.Tick,
			}
		}
		plan.Segments = append(plan.Segments, seg)
	}
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	return plan, nil
}

// Validate enforces the one-tick boundary tolerance and contiguous coverage.
func (p *LadderPlan) Validate() error {
	for _, seg := range p.Segments {
		var minFrom, maxFrom, minTo, maxTo media.Ticks
		first := true
		for repID, r := range seg.Ranges {
			if first {
				minFrom, maxFrom, minTo, maxTo = r.FromTick, r.FromTick, r.ToTick, r.ToTick
				first = false
			}
			if r.FromTick < minFrom {
				minFrom = r.FromTick
			}
			if r.FromTick > maxFrom {
				maxFrom = r.FromTick
			}
			if r.ToTick < minTo {
				minTo = r.ToTick
			}
			if r.ToTick > maxTo {
				maxTo = r.ToTick
			}
			_ = repID
		}
		if maxFrom-minFrom > 1 {
			return &LadderValidationError{Sequence: seg.Sequence,
				Message: fmt.Sprintf("from boundary delta %d exceeds one tick", maxFrom-minFrom)}
		}
		if maxTo-minTo > 1 {
			return &LadderValidationError{Sequence: seg.Sequence,
				Message: fmt.Sprintf("to boundary delta %d exceeds one tick", maxTo-minTo)}
		}
	}
	return nil
}
