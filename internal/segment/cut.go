// Package segment turns probe tracks into a single shared cut grid and
// aligns every bitrate representation onto it. Disagreement between
// representations is measured and resolved with a bounded fallback.
package segment

import (
	"sort"

	"mediacheck/internal/clock"
	"mediacheck/internal/model"
	"mediacheck/internal/probe"
)

// PlanConfig controls cut grid construction.
type PlanConfig struct {
	TargetTicks  int64 // desired segment length on the shared timeline
	WindowTicks  int64 // keyframe search window around each ideal cut
	MaxDriftTicks int64 // bounded fallback allowance between representations
}

// Cut is one shared cut point resolved to concrete sample boundaries.
type Cut struct {
	Index      int
	IdealAt    int64 // grid position on the shared timeline
	ChosenAt   int64 // actual boundary after keyframe alignment
	SampleIdx  uint32
	HasGap     bool
	Gap        model.TimeRange
}

// TrackPlan is the resolved cut list for one track of one representation.
type TrackPlan struct {
	Representation string
	TrackID        uint32
	IsVideo        bool
	Cuts           []Cut
}

// Planner builds shared grids from probe results.
type Planner struct {
	cfg PlanConfig
}

// NewPlanner validates and constructs a Planner.
func NewPlanner(cfg PlanConfig) (*Planner, error) {
	if cfg.TargetTicks <= 0 || cfg.WindowTicks < 0 || cfg.MaxDriftTicks < 0 {
		return nil, ErrInvalidPlanConfig
	}
	if cfg.WindowTicks == 0 {
		cfg.WindowTicks = cfg.TargetTicks
	}
	return &Planner{cfg: cfg}, nil
}

// IdealGrid returns the ideal cut positions 0, target, 2*target, ... up to
// the timeline end. All representations consume this identical grid, which
// is the structural guarantee that cut sets never diverge between bitrates.
func (p *Planner) IdealGrid(timelineEnd int64) []probe.CutPlan {
	var plans []probe.CutPlan
	for at := int64(0); at < timelineEnd; at += p.cfg.TargetTicks {
		plans = append(plans, probe.CutPlan{At: at})
	}
	return plans
}

// PlanTrack resolves the shared grid against one track's keyframes.
func (p *Planner) PlanTrack(rep string, tr *probe.Track) TrackPlan {
	end := trackEnd(tr)
	plans := p.IdealGrid(end)
	bounds := probe.ScanBoundaries(tr, plans, p.cfg.WindowTicks)
	tp := TrackPlan{Representation: rep, TrackID: tr.ID, IsVideo: tr.IsVideo}
	for i, b := range bounds {
		c := Cut{Index: i, IdealAt: b.PlanAt, ChosenAt: b.PlanAt, SampleIdx: 0}
		if b.Gap != nil {
			c.HasGap = true
			c.Gap = *b.Gap
		} else {
			c.ChosenAt = b.ChosenAt
			c.SampleIdx = b.SampleIndex
		}
		tp.Cuts = append(tp.Cuts, c)
	}
	return tp
}

// Align merges per-representation video plans onto one shared cut list.
// The anchor is the highest-resolution (first supplied) representation;
// every other representation must match within MaxDriftTicks, otherwise a
// bounded fallback snaps to the anchor and the drift is recorded.
func (p *Planner) Align(anchor TrackPlan, others []TrackPlan) (SharedManifestInput, []AlignmentError) {
	out := SharedManifestInput{
		TargetTicks:   p.cfg.TargetTicks,
		Representations: make([]string, 0, len(others)+1),
	}
	out.Representations = append(out.Representations, anchor.Representation)
	cutCount := len(anchor.Cuts)
	chosen := make([]int64, cutCount)
	for i, c := range anchor.Cuts {
		chosen[i] = c.ChosenAt
	}
	out.Cuts = append(out.Cuts, anchor.Cuts...)

	var errs []AlignmentError
	for _, o := range others {
		out.Representations = append(out.Representations, o.Representation)
		for i := 0; i < cutCount && i < len(o.Cuts); i++ {
			drift := o.Cuts[i].ChosenAt - chosen[i]
			if drift < 0 {
				drift = -drift
			}
			if drift > p.cfg.MaxDriftTicks {
				errs = append(errs, AlignmentError{
					Representation: o.Representation,
					CutIndex:       i,
					DriftTicks:     drift,
					AnchorTicks:    chosen[i],
					ObservedTicks:  o.Cuts[i].ChosenAt,
				})
				// Bounded fallback: this representation snaps to the anchor
				// boundary instead of inventing a private cut.
				o.Cuts[i].ChosenAt = chosen[i]
				o.Cuts[i].HasGap = anchor.Cuts[i].HasGap
			}
		}
	}
	out.Plans = append([]TrackPlan{anchor}, others...)
	sort.SliceStable(out.Representations, func(i, j int) bool {
		return out.Representations[i] < out.Representations[j]
	})
	return out, errs
}

func trackEnd(tr *probe.Track) int64 {
	if len(tr.Samples) == 0 {
		return 0
	}
	return tr.Samples[len(tr.Samples)-1].EndTicks
}

// MaxDrift exposes the bounded fallback allowance for downstream stages.
func (p *Planner) MaxDrift() int64 { return p.cfg.MaxDriftTicks }

// DefaultConfig returns the demo configuration: 6 s segments, a 3 s
// keyframe window and a 40 ms alignment allowance.
func DefaultConfig() PlanConfig {
	return PlanConfig{
		TargetTicks:   clock.DurationTicks(6),
		WindowTicks:   clock.DurationTicks(3),
		MaxDriftTicks: clock.MillisecondsToTicks(40),
	}
}
