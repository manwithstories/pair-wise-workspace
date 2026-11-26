// Package segment computes representation-independent 90 kHz cut points.
package segment

import (
	"fmt"

	"github.com/relayteam/relay/internal/media"
	"github.com/relayteam/relay/internal/timeline"
)

// DefaultSegmentTicks is a one-second HLS segment at 90 kHz.
const DefaultSegmentTicks int64 = media.TickHz

// Cut is one shared boundary used by every representation.
type Cut struct {
	Index         int    `json:"index"`
	Start90k      int64  `json:"start90k"`
	End90k        int64  `json:"end90k"`
	Clip          string `json:"clip,omitempty"`
	Transition    string `json:"transition,omitempty"`
	Discontinuity bool   `json:"discontinuity,omitempty"`
}

// Fragment attaches a source record to a shared cut interval.
type Fragment struct {
	Record        timeline.Record `json:"record"`
	SegmentIndex  int             `json:"segment_index"`
	CutStart90k   int64           `json:"cut_start90k"`
	CutEnd90k     int64           `json:"cut_end90k"`
	AtCutBoundary bool            `json:"at_cut_boundary"`
}

// Cutter maps integer PTS values onto one shared grid. It is stateless apart
// from configuration and therefore safe to replay after a checkpoint.
type Cutter struct {
	Grid media.Tick
}

// New returns a cutter on gridTicks. Zero selects the one-second default.
func New(gridTicks int64) *Cutter {
	if gridTicks <= 0 {
		gridTicks = DefaultSegmentTicks
	}
	return &Cutter{Grid: media.Tick(gridTicks)}
}

// Assign maps rec to its shared half-open interval [start,end).
func (c *Cutter) Assign(rec timeline.Record) (Fragment, error) {
	if rec.PTS90k < 0 || rec.Dur90k <= 0 {
		meta := rec.Meta(rec.RecordNo, rec.LineNo)
		return Fragment{}, media.E(media.ErrMalformedRecord, "segment.cut", meta,
			fmt.Errorf("invalid pts=%d dur=%d", rec.PTS90k, rec.Dur90k))
	}
	pts := media.Tick(rec.PTS90k)
	start := media.AlignDown(pts, c.Grid)
	end := start + c.Grid
	f := Fragment{
		Record:        rec,
		SegmentIndex:  int(start / c.Grid),
		CutStart90k:   int64(start),
		CutEnd90k:     int64(end),
		AtCutBoundary: pts == start,
	}
	if rec.Transition != nil {
		// A transition more than one tick from its shared cut is a boundary
		// alignment failure; exact fixtures report precision zero.
		delta := media.BoundaryDelta(pts, start)
		if delta > 1 {
			meta := rec.Meta(rec.RecordNo, rec.LineNo)
			return Fragment{}, media.E(media.ErrBoundaryMismatch, "segment.cut", meta,
				fmt.Errorf("transition %q is %d ticks from cut %d",
					rec.Transition.ID, delta, start))
		}
	}
	return f, nil
}

// BuildCuts constructs the complete shared table covering [firstPTS,lastEnd).
func BuildCuts(firstPTS, lastEnd int64, gridTicks int64) []Cut {
	c := New(gridTicks)
	start := media.AlignDown(media.Tick(firstPTS), c.Grid)
	lastBoundary := media.AlignUp(media.Tick(lastEnd), c.Grid)
	var cuts []Cut
	for t := start; t < lastBoundary; t += c.Grid {
		cuts = append(cuts, Cut{
			Index:    int(t / c.Grid),
			Start90k: int64(t),
			End90k:   int64(t + c.Grid),
		})
	}
	return cuts
}

// AnnotateError moves a unified error through the cutting stage. Source
// location is deliberately unchanged.
func AnnotateError(err error) error {
	return media.Annotate(err, "segment.cut")
}
