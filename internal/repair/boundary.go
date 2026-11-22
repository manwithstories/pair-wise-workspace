package repair

import (
	"fmt"

	"mediacheck/internal/clock"
	"mediacheck/internal/fault"
)

// BoundaryEdge is one shared-clock cut point where two layers meet.
type BoundaryEdge struct {
	ATrackID uint32 `json:"a_track_id"`
	BTrackID uint32 `json:"b_track_id"`
	// AEdgeMS and BEdgeMS are the nearest switch-quality boundaries in each
	// layer; DeltaMS is their separation on the shared clock.
	AEdgeMS int64 `json:"a_edge_ms"`
	BEdgeMS int64 `json:"b_edge_ms"`
	DeltaMS int64 `json:"delta_ms"`
	// Aligned reports that the cut point satisfies the tolerance.
	Aligned bool `json:"aligned"`
}

// BoundaryReport is the cross-layer boundary agreement.
type BoundaryReport struct {
	ToleranceMS int64          `json:"tolerance_ms"`
	Accept      bool           `json:"accept"`
	Edges       []BoundaryEdge `json:"edges"`
	MaxDeltaMS  int64          `json:"max_delta_ms"`
	AlignmentMS int64          `json:"alignment_ms"`
	Reasons     []string       `json:"reasons,omitempty"`
	Issues      []*fault.Fault `json:"-"`
}

// crossCheck compares the first recovered boundary of each pair of usable
// layers. A pair is aligned when both boundaries fall within the tolerance on
// the shared clock. Any unaligned pair refuses the switch and records the
// difference so downstream tooling can explain it.
func crossCheck(tracks []*TrackResult, cfg Config) BoundaryReport {
	rep := BoundaryReport{ToleranceMS: cfg.ToleranceMS, Accept: true}
	var usable []*TrackResult
	for _, t := range tracks {
		if t.usable() {
			usable = append(usable, t)
		}
	}
	if len(usable) < 2 {
		return rep
	}
	for i := 0; i < len(usable); i++ {
		for j := i + 1; j < len(usable); j++ {
			a, b := usable[i], usable[j]
			edge := BoundaryEdge{
				ATrackID: a.TrackID,
				BTrackID: b.TrackID,
				AEdgeMS:  a.Recovered.StartMS,
				BEdgeMS:  b.Recovered.StartMS,
			}
			edge.DeltaMS = clock.AbsDiff(edge.AEdgeMS, edge.BEdgeMS)
			edge.Aligned = edge.DeltaMS <= cfg.ToleranceMS
			if edge.DeltaMS > rep.MaxDeltaMS {
				rep.MaxDeltaMS = edge.DeltaMS
			}
			if edge.Aligned {
				if clock.AbsDiff(edge.AEdgeMS, edge.BEdgeMS) > rep.AlignmentMS {
					rep.AlignmentMS = clock.AbsDiff(edge.AEdgeMS, edge.BEdgeMS)
				}
			} else {
				rep.Accept = false
				rep.Reasons = append(rep.Reasons, fmt.Sprintf(
					"track %d starts at %d ms but track %d starts at %d ms (delta %d ms > tolerance %d ms)",
					a.TrackID, edge.AEdgeMS, b.TrackID, edge.BEdgeMS, edge.DeltaMS, cfg.ToleranceMS))
				rep.Issues = append(rep.Issues, fault.ForTrack(fault.CodeBoundary, fault.SeverityPartial,
					"boundary", b.TrackID, -1, fmt.Sprintf("boundary disagrees with track %d by %d ms",
						a.TrackID, edge.DeltaMS)))
			}
			rep.Edges = append(rep.Edges, edge)
		}
	}
	return rep
}
