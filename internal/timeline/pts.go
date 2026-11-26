package timeline

import (
	"fmt"

	"audiorestore/internal/model"
)

// TrackPTS is one sample's exact presentation time on the shared clock.
type TrackPTS struct {
	Track string
	Frame int64
	PTS   model.Rational // seconds
}

// PTSAt converts a frame position on a track's own clock into an exact
// shared-clock timestamp, including the track's start-frame offset.
func PTSAt(t *Track, frame int64) TrackPTS {
	abs := t.StartFrame + frame
	return TrackPTS{
		Track: t.ID,
		Frame: abs,
		PTS:   model.SampleToPTS(abs, t.Rate),
	}
}

// MapFrame maps a frame position from one sample rate onto another using
// integer conversion only (see model.ConvertSample).
func MapFrame(frame int64, from, to model.SampleRate) (int64, error) {
	return model.ConvertSample(frame, from, to)
}

// CheckMonotonic verifies that consecutive PTS values never go backwards.
// On violation it returns a structured error carrying the offending track
// and sample position rather than silently reordering anything.
func CheckMonotonic(pts []TrackPTS) error {
	for i := 1; i < len(pts); i++ {
		if pts[i].PTS.Cmp(pts[i-1].PTS) < 0 {
			return model.TrackedError{
				Kind:   model.ErrPTSNotMonotonic,
				Track:  pts[i].Track,
				Sample: pts[i].Frame,
				Msg: fmt.Sprintf("pts %s < previous %s",
					pts[i].PTS, pts[i-1].PTS),
			}
		}
	}
	return nil
}

// SyncErrorUS is the absolute A/V offset of an audio frame against an
// anchor, in microseconds, computed in exact rational arithmetic and
// rounded once at the boundary.
func SyncErrorUS(t *Track, frame int64, a Anchor) (int64, error) {
	audioPTS := PTSAt(t, frame).PTS
	refFrame, err := MapFrame(a.RefSample, a.RefRate, t.Rate)
	if err != nil {
		return 0, err
	}
	refPTS := model.SampleToPTS(refFrame, t.Rate)
	return audioPTS.Sub(refPTS).Abs().Microseconds(), nil
}

// MappingResidualUS measures the cross-sample-rate conversion error of one
// video anchor against a track's integer sample grid: the anchor's exact
// PTS versus the PTS of its nearest integer frame on the track clock.
// It isolates integer-conversion rounding (no float accumulation can
// occur) and is bounded by half a sample period on the target clock.
func MappingResidualUS(t *Track, a Anchor) (int64, error) {
	exactPTS := model.SampleToPTS(a.RefSample, a.RefRate)
	mapped, err := model.ConvertSample(a.RefSample, a.RefRate, t.Rate)
	if err != nil {
		return 0, err
	}
	gridPTS := model.SampleToPTS(mapped, t.Rate)
	return exactPTS.Sub(gridPTS).Abs().Microseconds(), nil
}

// CheckLayout flags tracks whose channel layout is missing or conflicts
// with another track's. The source mapping is always preserved: this only
// annotates degradation, never reorders channels.
func CheckLayout(g *Graph, log *model.ErrorLog) {
	seen := map[string]string{} // layout -> first track using it
	for _, t := range g.Tracks {
		if t.Layout == "" {
			t.Degraded = append(t.Degraded, "layout_missing")
			log.Addf(model.ErrLayoutMissing, t.ID, -1, model.NoPosition,
				"no channel layout; source mapping retained")
			continue
		}
		if first, ok := seen[t.Layout]; !ok {
			seen[t.Layout] = t.ID
		} else {
			_ = first
		}
	}
	// A conflict means tracks that share content ancestry (a dependency
	// edge) carry different layout tags.
	for _, e := range g.edges {
		from, to := g.ByID(e.From), g.ByID(e.To)
		if from != nil && to != nil && from.Layout != "" && to.Layout != "" &&
			from.Layout != to.Layout {
			to.Degraded = append(to.Degraded, "layout_conflict")
			log.Addf(model.ErrLayoutConflict, to.ID, -1, model.NoPosition,
				"layout %q conflicts with upstream %q", to.Layout, from.Layout)
		}
	}
}

// CheckColor flags conflicting color/label metadata along dependency
// edges and returns the number of conflicts.
func CheckColor(g *Graph, log *model.ErrorLog) int {
	n := 0
	for _, e := range g.edges {
		from, to := g.ByID(e.From), g.ByID(e.To)
		if from != nil && to != nil && from.Color != "" && to.Color != "" &&
			from.Color != to.Color {
			n++
			to.Degraded = append(to.Degraded, "color_conflict")
			log.Addf(model.ErrColorConflict, to.ID, -1, model.NoPosition,
				"color %q conflicts with upstream %q", to.Color, from.Color)
		}
	}
	return n
}
