package clock

import (
	"fmt"
	"math"
	"sort"

	"medpipe/internal/model"
)

// Options controls timeline normalization.
type Options struct {
	// NoDriftCorrection disables the per-track drift estimate. It exists so
	// tests can compare raw and corrected timelines.
	NoDriftCorrection bool
}

// TimedFrame is a frame placed on the shared nanosecond timeline. Index is
// the frame's position in its track's frame slice.
type TimedFrame struct {
	Index      int
	Seq        uint64
	RawTS      uint64
	NS         int64
	IntervalNS int64
	Keyframe   bool
}

// TrackTimeline is one track normalized onto the common time base.
type TrackTimeline struct {
	Track           model.Track
	Unwrap          *UnwrapResult
	Frames          []TimedFrame
	StartNS         int64
	EndNS           int64
	FrameIntervalNS int64
	MaxIntervalNS   int64
	VFR             bool
	DriftPPM        float64
}

// Timeline is the whole collection normalized to nanoseconds.
type Timeline struct {
	Tracks        []TrackTimeline
	StartNS       int64
	EndNS         int64
	MaxIntervalNS int64
}

// Build unwraps every track, estimates drift, applies VFR-aware intervals and
// maps each frame to nanoseconds. frames is keyed by track ID. The returned
// timeline is a pure function of its inputs; building twice yields identical
// values.
func Build(tracks []model.Track, frames map[string][]model.Frame, opt Options) (*Timeline, error) {
	if len(tracks) == 0 {
		return nil, model.New(model.KindInput, "clock.Build", "no tracks supplied")
	}
	seen := make(map[string]bool, len(tracks))
	tl := &Timeline{Tracks: make([]TrackTimeline, 0, len(tracks))}
	first := true
	for _, tr := range tracks {
		if tr.ID == "" {
			return nil, model.New(model.KindInput, "clock.Build", "track with empty ID")
		}
		if seen[tr.ID] {
			return nil, model.New(model.KindInput, "clock.Build", fmt.Sprintf("duplicate track ID %q", tr.ID))
		}
		seen[tr.ID] = true
		if tr.Timescale == 0 {
			return nil, model.New(model.KindInput, "clock.Build", fmt.Sprintf("track %s has zero timescale", tr.ID))
		}
		fs, ok := frames[tr.ID]
		if !ok {
			return nil, model.New(model.KindInput, "clock.Build", fmt.Sprintf("track %s has no frame slice", tr.ID))
		}
		tt, err := buildTrack(tr, fs, opt)
		if err != nil {
			return nil, err
		}
		if len(tt.Frames) > 0 {
			if first || tt.StartNS < tl.StartNS {
				tl.StartNS = tt.StartNS
			}
			if first || tt.EndNS > tl.EndNS {
				tl.EndNS = tt.EndNS
			}
			if tt.MaxIntervalNS > tl.MaxIntervalNS {
				tl.MaxIntervalNS = tt.MaxIntervalNS
			}
			first = false
		}
		tl.Tracks = append(tl.Tracks, tt)
	}
	return tl, nil
}

func buildTrack(tr model.Track, fs []model.Frame, opt Options) (TrackTimeline, error) {
	tt := TrackTimeline{Track: tr}
	raw := make([]uint64, len(fs))
	for i := range fs {
		raw[i] = fs[i].RawTS
	}
	ur, err := Unwrap(raw, UnwrapOptions{Bits: tr.ClockBits, NominalDelta: tr.NominalTickDelta})
	if err != nil {
		return tt, err
	}
	tt.Unwrap = ur
	if !opt.NoDriftCorrection {
		tt.DriftPPM = ur.DriftPPM
	}
	tt.Frames = make([]TimedFrame, len(fs))
	// OriginNS is the declared wall-clock placement of the track's first
	// frame; counters typically start at an arbitrary value, so ticks are
	// measured relative to the first sample. This keeps declared offsets
	// meaningful while making the mapping independent of counter start.
	var tickBase float64
	if len(fs) > 0 {
		tickBase = CorrectedTicks(ur.Unwrapped[0], tt.DriftPPM)
	}
	for i := range fs {
		corrected := CorrectedTicks(ur.Unwrapped[i], tt.DriftPPM) - tickBase
		ns := float64(tr.OriginNS) + TickSeconds(corrected, float64(tr.Timescale))*1e9
		tt.Frames[i] = TimedFrame{
			Index:    i,
			Seq:      fs[i].Seq,
			RawTS:    fs[i].RawTS,
			NS:       RoundTicks(ns),
			Keyframe: fs[i].Keyframe,
		}
	}
	// VFR intervals come from the actual gap between adjacent frames, not
	// from a nominal rate.
	var deltas []int64
	for i := 0; i < len(tt.Frames)-1; i++ {
		d := tt.Frames[i+1].NS - tt.Frames[i].NS
		if d <= 0 {
			return tt, model.New(model.KindAmbiguous, "clock.Build",
				fmt.Sprintf("track %s frame %d does not advance on the shared timeline", tr.ID, i+1))
		}
		tt.Frames[i].IntervalNS = d
		deltas = append(deltas, d)
	}
	tt.FrameIntervalNS = medianInt64(deltas)
	if len(deltas) == 0 {
		// A single-frame track exposes no gap of its own. Fall back to the
		// declared nominal tick step so its duration is still meaningful;
		// failing that, give it a nominal one-nanosecond span.
		tt.FrameIntervalNS = 1
		if tr.NominalTickDelta > 0 {
			if ns := RoundTicks(TickSeconds(float64(tr.NominalTickDelta), float64(tr.Timescale)) * 1e9); ns > 0 {
				tt.FrameIntervalNS = ns
			}
		}
	}
	for i := 0; i < len(tt.Frames)-1; i++ {
		if tt.Frames[i].IntervalNS > tt.MaxIntervalNS {
			tt.MaxIntervalNS = tt.Frames[i].IntervalNS
		}
	}
	for _, d := range deltas {
		if d != tt.FrameIntervalNS {
			tt.VFR = true
			break
		}
	}
	if len(tt.Frames) > 0 {
		last := tt.Frames[len(tt.Frames)-1]
		last.IntervalNS = tt.FrameIntervalNS
		tt.Frames[len(tt.Frames)-1] = last
		tt.StartNS = tt.Frames[0].NS
		tt.EndNS = last.NS + last.IntervalNS
	} else {
		tt.StartNS = tr.OriginNS
		tt.EndNS = tr.OriginNS
	}
	return tt, nil
}

func medianInt64(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	cp := append([]int64(nil), v...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	return cp[len(cp)/2]
}

// Track returns the normalized timeline for a track.
func (tl *Timeline) Track(id string) (*TrackTimeline, bool) {
	for i := range tl.Tracks {
		if tl.Tracks[i].Track.ID == id {
			return &tl.Tracks[i], true
		}
	}
	return nil, false
}

// FrameAtOrAfter returns the index of the first frame whose NS is >= ns.
func (tt *TrackTimeline) FrameAtOrAfter(ns int64) int {
	return sort.Search(len(tt.Frames), func(i int) bool { return tt.Frames[i].NS >= ns })
}

// FrameBefore returns the index of the last frame whose NS is < ns.
func (tt *TrackTimeline) FrameBefore(ns int64) int {
	return sort.Search(len(tt.Frames), func(i int) bool { return tt.Frames[i].NS >= ns }) - 1
}

// DurationSeconds is the timeline span as a convenience for reporting.
func (tl *Timeline) DurationSeconds() float64 {
	return math.Max(0, float64(tl.EndNS-tl.StartNS)) / 1e9
}
