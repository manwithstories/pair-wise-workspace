// Package clock aligns events from independent device time bases (RTP, MP4
// proxy, WAV audio) onto a single wall-clock timeline by fitting clock drift
// and mapping frames through rational rescale.
package clock

import (
	"errors"
	"sort"

	"mediaaudit/internal/model"
)

// Anchor maps a device-side position to wall-clock nanoseconds. At least two
// anchors per device are needed to fit drift; one anchor gives an offset only.
type Anchor struct {
	DeviceTicks int64
	WallNanos   int64
}

// Fit is an affine model wallNanos = slope*tick + intercept for one device.
type Fit struct {
	Slope     float64 // ns per device tick
	Intercept float64 // wall-clock ns at tick 0
	PPM       float64 // drift relative to the nominal tick duration
	DriftOnly bool    // true when only an offset (single anchor) was possible
}

// FitClock least-squares fits wall time against device ticks. nominalNanos is
// the expected ns-per-tick for an ideal clock; PPM measures deviation.
func FitClock(anchors []Anchor, nominalNanos float64) (Fit, error) {
	if len(anchors) == 0 {
		return Fit{}, errors.New("clock: no anchors")
	}
	if len(anchors) == 1 {
		return Fit{
			Slope:     nominalNanos,
			Intercept: float64(anchors[0].WallNanos) - nominalNanos*float64(anchors[0].DeviceTicks),
			DriftOnly: true,
		}, nil
	}
	var sumX, sumY, sumXX, sumXY float64
	for _, a := range anchors {
		x := float64(a.DeviceTicks)
		y := float64(a.WallNanos)
		sumX += x
		sumY += y
		sumXX += x * x
		sumXY += x * y
	}
	n := float64(len(anchors))
	den := n*sumXX - sumX*sumX
	if den == 0 {
		return Fit{}, errors.New("clock: degenerate anchors (constant tick)")
	}
	slope := (n*sumXY - sumX*sumY) / den
	intercept := (sumY - slope*sumX) / n
	ppm := 0.0
	if nominalNanos > 0 {
		ppm = (nominalNanos - slope) / nominalNanos * 1e6
	}
	return Fit{Slope: slope, Intercept: intercept, PPM: ppm}, nil
}

// ToWall maps a device tick to wall-clock nanoseconds using the fit.
func (f Fit) ToWall(tick int64) int64 {
	return int64(f.Slope*float64(tick) + f.Intercept + 0.5)
}

// Event is a point of interest from any track, normalized onto wall time.
type Event struct {
	WallNanos  int64
	Track      string
	Frame      model.Frame
	Kind       model.Kind
	ByteOffset int64
}

// Timeline merges events from multiple tracks into a deterministic, wall-time
// ordered sequence. Ties break by track name then frame, ensuring identical
// output across repeated runs.
type Timeline struct {
	events []Event
}

// Add inserts an event.
func (tl *Timeline) Add(e Event) { tl.events = append(tl.events, e) }

// Events returns events sorted by wall time, then track, then frame.
func (tl *Timeline) Events() []Event {
	out := make([]Event, len(tl.events))
	copy(out, tl.events)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].WallNanos != out[j].WallNanos {
			return out[i].WallNanos < out[j].WallNanos
		}
		if out[i].Track != out[j].Track {
			return out[i].Track < out[j].Track
		}
		return out[i].Frame < out[j].Frame
	})
	return out
}

// Len reports how many events have been recorded.
func (tl *Timeline) Len() int { return len(tl.events) }

// MapFrame converts a frame ordinal in rate `from` into a frame ordinal in
// rate `to`, preserving exact timing via the shared rational time base.
func MapFrame(frame model.Frame, from, to model.Rational) model.Frame {
	return model.Frame(from.Rescale(int64(frame), to))
}

// AlignRange maps a frame range between rates, expanding half-open endpoints
// so no mapped sample is lost to rounding.
func AlignRange(r model.FrameRange, from, to model.Rational) model.FrameRange {
	first := MapFrame(r.First, from, to)
	last := MapFrame(r.Last, from, to)
	if last < first {
		last = first
	}
	return model.FrameRange{First: first, Last: last}
}

// CrossTrackDamage projects a damaged frame range on `from` onto every target
// rate, returning one aligned range per (track, rate) pair. This is the
// cross-track damage map used to correlate WAV corruption with proxy frames.
type TrackRate struct {
	Track string
	Rate  model.Rational
}

// DamageMap binds a source fault to its projection on each target track.
type DamageMap struct {
	Source   Event
	Targets  []Event
}

// ProjectDamage aligns one source event across the supplied target tracks.
func ProjectDamage(src Event, srcRate model.Rational, targets []TrackRate) DamageMap {
	dm := DamageMap{Source: src}
	for _, t := range targets {
		if t.Track == src.Track {
			dm.Targets = append(dm.Targets, src)
			continue
		}
		dm.Targets = append(dm.Targets, Event{
			WallNanos:  src.WallNanos,
			Track:      t.Track,
			Frame:      MapFrame(src.Frame, srcRate, t.Rate),
			Kind:       src.Kind,
			ByteOffset: -1,
		})
	}
	return dm
}
