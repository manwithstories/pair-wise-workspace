// Package plan derives the single set of cut points shared by every
// rendition and assigns each timed event to exactly one segment.
package plan

import (
	"aircheck/internal/clock"
	"aircheck/internal/fault"
	"aircheck/internal/scan"
)

// CutPlan is the shared segmentation timeline for all renditions.
type CutPlan struct {
	SegmentTicks int64        `json:"segment_ticks"`
	Cuts         []int64      `json:"cuts"` // boundaries in 90 kHz ticks, starts at 0
	Events       []scan.Event `json:"events"`
	Duration     int64        `json:"duration_ticks"`
}

// Build computes cuts from the scan result. Every rendition shares the
// same boundaries; an event at tick t belongs to segment floor(t/seg).
// An event that cannot be placed is a fault, never silently dropped.
func Build(res *scan.Result) (*CutPlan, error) {
	if res.SegmentTicks <= 0 {
		return nil, fault.New(fault.CodeRuntime, "", -1, false, "non-positive segment ticks")
	}
	var maxDur int64
	for _, r := range res.Renditions {
		if r.DurationTicks > maxDur {
			maxDur = r.DurationTicks
		}
	}
	if maxDur <= 0 {
		return nil, fault.New(fault.CodeMissingTrack, res.Package, -1, false,
			"no probed rendition duration")
	}
	p := &CutPlan{SegmentTicks: res.SegmentTicks, Duration: maxDur}
	for c := int64(0); c < maxDur; c += res.SegmentTicks {
		p.Cuts = append(p.Cuts, c)
	}
	p.Cuts = append(p.Cuts, maxDur)
	nSeg := len(p.Cuts) - 1
	for _, e := range res.Events {
		if e.Tick < 0 {
			return nil, fault.New(fault.CodeMalformedEvent, e.Track, e.Tick, false,
				"negative event tick")
		}
		if e.Tick >= maxDur+clock.Timescale/2 {
			return nil, fault.New(fault.CodeMalformedEvent, e.Track, e.Tick, false,
				"%s event beyond last cut %s", e.Type, clock.String(maxDur))
		}
		seg := int(e.Tick / res.SegmentTicks)
		if seg >= nSeg {
			seg = nSeg - 1 // event inside the final partial segment
		}
		e.SegmentID = seg
		p.Events = append(p.Events, e)
	}
	return p, nil
}

// SegmentOf returns the owning segment index for a tick.
func (p *CutPlan) SegmentOf(tick int64) int {
	seg := int(tick / p.SegmentTicks)
	if seg >= len(p.Cuts)-1 {
		return len(p.Cuts) - 2
	}
	return seg
}

// SegmentBounds returns [start,end) ticks of segment i.
func (p *CutPlan) SegmentBounds(i int) (int64, int64) {
	return p.Cuts[i], p.Cuts[i+1]
}

// NumSegments reports the segment count.
func (p *CutPlan) NumSegments() int { return len(p.Cuts) - 1 }
