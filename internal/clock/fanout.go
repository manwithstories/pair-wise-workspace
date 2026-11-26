// Package clock fans one rational shared clock out to every representation.
package clock

import (
	"sort"

	"mediad/internal/media"
)

// DefaultTimescale is the shared presentation clock (90 kHz is exact for the
// common 24/25/30/60 fps video durations).
const DefaultTimescale uint32 = 90_000

// Point is one sample timestamp projected onto the shared clock.
type Point struct {
	TrackID      uint32 `json:"track_id"`
	SampleNumber uint32 `json:"sample_number"`
	PTS          int64  `json:"pts_shared_ticks"`
	DTS          int64  `json:"dts_shared_ticks"`
	PTSNS        int64  `json:"pts_ns"`
	DTSNS        int64  `json:"dts_ns"`
	Sync         bool   `json:"sync"`
}

// Timeline is the shared-clock projection of one track.
type Timeline struct {
	TrackID uint32  `json:"track_id"`
	Points  []Point `json:"points"`
}

// Fanout contains all track projections.
type Fanout struct {
	Clock     media.Clock          `json:"clock"`
	Timescale uint32               `json:"timescale"`
	Tracks    map[uint32]*Timeline `json:"tracks"`
}

// Receiver is implemented by representation layers that consume the clock.
type Receiver interface {
	ClockID() string
	ObserveSharedClock(media.Clock, []Point) error
}

// New projects every track onto one shared rational timescale.
func New(p *media.Project, sharedTimescale uint32) (*Fanout, []*media.Fault) {
	if sharedTimescale == 0 {
		sharedTimescale = DefaultTimescale
	}
	shared, err := media.NewClock(sharedTimescale)
	if err != nil {
		f := media.NewFault(media.CodeInvalidInput, "%v", err).Fatal()
		return nil, []*media.Fault{f}
	}
	out := &Fanout{Clock: shared, Timescale: sharedTimescale, Tracks: map[uint32]*Timeline{}}
	var faults []*media.Fault
	for _, t := range p.Tracks {
		if t == nil {
			continue
		}
		tc, err := t.Clock()
		if err != nil {
			faults = append(faults, media.NewFault(media.CodeInvalidInput, "%v", err).WithTrack(t.ID).Fatal())
			continue
		}
		tl := &Timeline{TrackID: t.ID}
		for _, s := range t.Samples {
			pts, err := media.ConvertTicks(s.PTS, tc, shared)
			if err != nil {
				faults = append(faults, media.NewFault(media.CodeInvalidInput, "PTS conversion overflow: %v", err).WithTrack(t.ID).Fatal())
				continue
			}
			dts, err := media.ConvertTicks(s.DTS, tc, shared)
			if err != nil {
				faults = append(faults, media.NewFault(media.CodeInvalidInput, "DTS conversion overflow: %v", err).WithTrack(t.ID).Fatal())
				continue
			}
			ptsNS, err := shared.TicksToNanoseconds(pts)
			if err != nil {
				faults = append(faults, media.NewFault(media.CodeInvalidInput, "%v", err).WithTrack(t.ID).Fatal())
				continue
			}
			dtsNS, _ := shared.TicksToNanoseconds(dts)
			tl.Points = append(tl.Points, Point{
				TrackID: t.ID, SampleNumber: s.Index, PTS: pts, DTS: dts,
				PTSNS: ptsNS, DTSNS: dtsNS, Sync: s.Sync,
			})
		}
		sort.Slice(tl.Points, func(i, j int) bool { return tl.Points[i].SampleNumber < tl.Points[j].SampleNumber })
		out.Tracks[t.ID] = tl
	}
	return out, faults
}

// Dispatch sends each projected timeline to all receivers.
func (f *Fanout) Dispatch(receivers ...Receiver) []*media.Fault {
	var faults []*media.Fault
	if f == nil {
		return []*media.Fault{media.NewFault(media.CodeInvalidInput, "nil clock fanout").Fatal()}
	}
	for _, r := range receivers {
		if r == nil {
			continue
		}
		for _, tl := range f.Tracks {
			if err := r.ObserveSharedClock(f.Clock, tl.Points); err != nil {
				faults = append(faults, media.NewFault(media.CodeBoundaryMismatch, "receiver %s: %v", r.ClockID(), err).Fatal())
			}
		}
	}
	return faults
}

// BoundaryGapNS returns the maximum nearest-boundary distance between two
// tracks' presentation timestamps. This is the A/V sync error used by metrics.
func (f *Fanout) BoundaryGapNS(a, b uint32) int64 {
	ta, tb := f.Tracks[a], f.Tracks[b]
	if ta == nil || tb == nil || len(ta.Points) == 0 || len(tb.Points) == 0 {
		return 0
	}
	var maxGap int64
	maxA := ta.Points[len(ta.Points)-1].PTSNS
	maxB := tb.Points[len(tb.Points)-1].PTSNS
	limit := min64(maxA, maxB)
	for _, x := range ta.Points {
		if x.PTSNS > limit {
			break
		}
		if gap := nearestNS(x.PTSNS, tb.Points); gap > maxGap {
			maxGap = gap
		}
	}
	for _, x := range tb.Points {
		if x.PTSNS > limit {
			break
		}
		if gap := nearestNS(x.PTSNS, ta.Points); gap > maxGap {
			maxGap = gap
		}
	}
	return maxGap
}

func nearestNS(target int64, pts []Point) int64 {
	if len(pts) == 0 {
		return 0
	}
	i := sort.Search(len(pts), func(i int) bool { return pts[i].PTSNS >= target })
	best := absDiff(target, pts[0].PTSNS)
	if i < len(pts) {
		best = min64(best, absDiff(target, pts[i].PTSNS))
	}
	if i > 0 {
		best = min64(best, absDiff(target, pts[i-1].PTSNS))
	}
	return best
}

func absDiff(a, b int64) int64 {
	if a < b {
		return b - a
	}
	return a - b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
