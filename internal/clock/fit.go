package clock

import (
	"math"
	"sort"

	"relay/internal/media"
)

// Point pairs a source clock reading (ticks) with a reference time in
// nanoseconds (capture wall clock). Anchor points are the observations used
// to estimate clock drift.
type Point struct {
	Ticks uint64
	RefNS int64
}

// Fit is an affine mapping ref_ns ≈ Slope*ticks + Intercept estimated from
// anchor points by ordinary least squares.
type Fit struct {
	Slope     float64 // nanoseconds per source tick
	Intercept float64
	Timescale uint64
}

// Apply converts source ticks to reference nanoseconds.
func (f Fit) Apply(ticks uint64) int64 {
	return int64(math.Round(f.Slope*float64(ticks) + f.Intercept))
}

// FitLine estimates slope and intercept from anchor points. With fewer than
// two points it falls back to the nominal timescale slope
// (1e9 nanoseconds per second / ticks per second).
func FitLine(points []Point, timescaleHz uint64) Fit {
	nominal := 1e9 / float64(timescaleHz)
	f := Fit{Slope: nominal, Timescale: timescaleHz}
	n := len(points)
	if n < 2 {
		return f
	}
	var sx, sy, sxx, sxy float64
	for _, p := range points {
		x := float64(p.Ticks)
		y := float64(p.RefNS)
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	denom := float64(n)*sxx - sx*sx
	if math.Abs(denom) < 1e-9 {
		return f
	}
	f.Slope = (float64(n)*sxy - sx*sy) / denom
	f.Intercept = (sy - f.Slope*sx) / float64(n)
	return f
}

// Estimator tracks anchor points for one source and converts its capture
// ticks into unified nanoseconds. The first anchor defines the run-local
// epoch, so the first sample always presents at PTS 0.
type Estimator struct {
	timescale   uint64
	anchors     []Point
	originTicks uint64
	originRef   int64
	fit         Fit
	dirty       bool
}

// NewEstimator creates an estimator for a clock running at timescaleHz ticks
// per second.
func NewEstimator(timescaleHz uint64) *Estimator {
	if timescaleHz == 0 {
		timescaleHz = 1
	}
	return &Estimator{timescale: timescaleHz, dirty: true}
}

// AddAnchor records one (ticks, reference nanoseconds) observation.
func (e *Estimator) AddAnchor(ticks uint64, refNS int64) {
	if len(e.anchors) == 0 {
		e.originTicks = ticks
		e.originRef = refNS
	}
	e.anchors = append(e.anchors, Point{Ticks: ticks, RefNS: refNS})
	e.dirty = true
}

// Fit returns the current drift fit, recomputing only when anchors changed.
func (e *Estimator) Fit() Fit {
	if e.dirty {
		e.fit = FitLine(e.anchors, e.timescale)
		e.dirty = false
	}
	return e.fit
}

// ToNS maps source ticks to unified presentation nanoseconds relative to the
// first anchor (PTS 0 at the origin), correcting estimated drift.
func (e *Estimator) ToNS(ticks uint64) int64 {
	f := e.Fit()
	origin := f.Apply(e.originTicks)
	if len(e.anchors) == 0 {
		origin = int64(math.Round(f.Slope * float64(e.originTicks)))
	}
	return f.Apply(ticks) - origin
}

// OriginRefNS returns the reference time of the first anchor.
func (e *Estimator) OriginRefNS() int64 { return e.originRef }

// AssignPresentation builds one estimator per source from the frames'
// capture anchors, fills every frame's PresentationNS and returns the frames
// ordered for presentation. Mixed frame rates stay correct: ordering is by
// unified PTS, with source ID and sequence breaking ties stably.
func AssignPresentation(frames []*media.RawFrame) []*media.RawFrame {
	estimators := map[uint32]*Estimator{}
	for _, fr := range frames {
		est, ok := estimators[fr.Meta.SourceID]
		if !ok {
			est = NewEstimator(fr.Meta.TimescaleHz)
			estimators[fr.Meta.SourceID] = est
		}
		est.AddAnchor(fr.Meta.ClockTicks, fr.Meta.CaptureRefNS)
	}
	for _, fr := range frames {
		fr.Meta.PresentationNS = estimators[fr.Meta.SourceID].ToNS(fr.Meta.ClockTicks)
	}
	ordered := make([]*media.RawFrame, len(frames))
	copy(ordered, frames)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i].Meta, ordered[j].Meta
		if a.PresentationNS != b.PresentationNS {
			return a.PresentationNS < b.PresentationNS
		}
		if a.SourceID != b.SourceID {
			return a.SourceID < b.SourceID
		}
		return a.Sequence < b.Sequence
	})
	return ordered
}
