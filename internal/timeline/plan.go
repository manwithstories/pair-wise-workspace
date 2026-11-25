// Package timeline converts source-relative samples onto one master timeline
// and validates edit/transition plans. Every conversion goes through the exact
// rational arithmetic in internal/media, so edit points and transition
// overlaps never accumulate floating-point drift.
package timeline

import (
	"archivecheck/internal/media"
)

// Transition kinds recognized by the planner.
const (
	TransCut      = "cut"
	TransDissolve = "dissolve"
)

// Point is one timestamp expressed in an explicit rational time base.
type Point struct {
	Ticks int64
	TB    media.Rational
}

// Transition links two adjacent edits. Overlap is measured in the master time
// base and must be positive for a dissolve; a cut has zero overlap.
type Transition struct {
	Kind    string
	Overlap int64 // master time-base ticks
}

// Edit is one clip on the master timeline. In/Out are source positions in the
// source's own time base; Start is its placement in master ticks.
type Edit struct {
	ID      string
	Segment string
	Track   int // 1-based source track
	In      Point
	Out     Point
	Start   int64 // master time-base ticks where the clip begins
	Trans   *Transition
}

// Plan is a validated ordering of edits on one master time base.
type Plan struct {
	MasterTB media.Rational
	Edits    []Edit
}

// Validate checks the whole plan using integer-only arithmetic. It returns the
// collected located faults; a non-nil result means the manifest is invalid
// (CLI exit code 2). Every fault carries the owning edit id so demux/index
// faults can be joined back to a clip.
func (p *Plan) Validate() []*media.Fault {
	var faults media.FaultSet
	if !p.MasterTB.Valid() {
		faults.Add(media.NewFault(media.KindInvalidManifest, "", 0, -1,
			"master time base %d/%d is invalid", p.MasterTB.Num, p.MasterTB.Den))
		return faults.All()
	}
	var prevEnd int64
	for i := range p.Edits {
		e := &p.Edits[i]
		// Time bases themselves must be usable.
		if !e.In.TB.Valid() || !e.Out.TB.Valid() {
			faults.Add(media.NewFault(media.KindInvalidManifest, e.Segment, e.Track, -1,
				"edit %s has an invalid source time base", e.ID).WithEdit(e.ID))
			continue
		}
		// Negative source timestamps are illegal.
		if err := media.CheckNonNegative(e.In.Ticks); err != nil {
			faults.Add(media.NewFault(media.KindNegativeTS, e.Segment, e.Track, -1,
				"edit %s in-point %d is negative", e.ID, e.In.Ticks).WithEdit(e.ID))
		}
		if err := media.CheckNonNegative(e.Out.Ticks); err != nil {
			faults.Add(media.NewFault(media.KindNegativeTS, e.Segment, e.Track, -1,
				"edit %s out-point %d is negative", e.ID, e.Out.Ticks).WithEdit(e.ID))
		}

		// Convert in/out into the master time base, then require out > in.
		inMaster, err1 := media.Rescale(e.In.Ticks, e.In.TB, p.MasterTB)
		outMaster, err2 := media.Rescale(e.Out.Ticks, e.Out.TB, p.MasterTB)
		if err1 != nil || err2 != nil {
			faults.Add(media.NewFault(media.KindInvalidManifest, e.Segment, e.Track, -1,
				"edit %s timestamp rescale failed: %v/%v", e.ID, err1, err2).WithEdit(e.ID))
			continue
		}
		dur := outMaster - inMaster
		if err := media.CheckDuration(dur); err != nil {
			faults.Add(media.NewFault(media.KindZeroDuration, e.Segment, e.Track, -1,
				"edit %s duration is %d master ticks (must be >0)", e.ID, dur).WithEdit(e.ID))
		}

		// Placement on the master timeline. A plain cut must start at or after
		// the previous edit's end. A dissolve (declared on the later edit)
		// intentionally overlaps by the declared amount, checked explicitly.
		isDissolve := e.Trans != nil && e.Trans.Kind == TransDissolve
		if i > 0 && !isDissolve {
			if err := media.CheckMonotonic(prevEnd, e.Start); err != nil {
				faults.Add(media.NewFault(media.KindWrap, e.Segment, e.Track, -1,
					"edit %s starts at %d before prior edit end %d", e.ID, e.Start, prevEnd).WithEdit(e.ID))
			}
		}
		if i > 0 && isDissolve {
			got := prevEnd - e.Start
			if got != e.Trans.Overlap {
				faults.Add(media.NewFault(media.KindInvalidManifest, e.Segment, e.Track, -1,
					"edit %s transition overlap is %d, plan declares %d", e.ID, got, e.Trans.Overlap).WithEdit(e.ID))
			}
			if e.Trans.Overlap <= 0 {
				faults.Add(media.NewFault(media.KindZeroDuration, e.Segment, e.Track, -1,
					"edit %s dissolve overlap must be positive", e.ID).WithEdit(e.ID))
			}
		}
		prevEnd = e.Start + dur
	}
	return faults.All()
}

// ResolvedEdit is an edit with all endpoints expressed in master ticks.
type ResolvedEdit struct {
	Edit      Edit
	InMaster  int64
	OutMaster int64
	Duration  int64
}

// Resolve converts every edit endpoint into the master time base. It assumes
// Validate has run; conversion errors are returned as located faults.
func (p *Plan) Resolve() ([]ResolvedEdit, []*media.Fault) {
	var faults media.FaultSet
	out := make([]ResolvedEdit, 0, len(p.Edits))
	for i := range p.Edits {
		e := p.Edits[i]
		inM, err := media.Rescale(e.In.Ticks, e.In.TB, p.MasterTB)
		if err != nil {
			faults.Add(media.NewFault(media.KindInvalidManifest, e.Segment, e.Track, -1,
				"edit %s in rescale: %s", e.ID, err).WithEdit(e.ID))
			continue
		}
		outM, err := media.Rescale(e.Out.Ticks, e.Out.TB, p.MasterTB)
		if err != nil {
			faults.Add(media.NewFault(media.KindInvalidManifest, e.Segment, e.Track, -1,
				"edit %s out rescale: %s", e.ID, err).WithEdit(e.ID))
			continue
		}
		out = append(out, ResolvedEdit{
			Edit: e, InMaster: inM, OutMaster: outM, Duration: outM - inM,
		})
	}
	return out, faults.All()
}
