package timeline

import (
	"sort"

	"archive/internal/manifest"
	"archive/internal/media"
)

// Sample is one repaired sample in decode order, in unified track ticks
// (the track's own timescale, shifted so the first sample starts at 0).
type Sample struct {
	Index    int   // decode-order index
	DTS      int64 // repaired unified decode timestamp (ticks)
	CTS      int64 // repaired unified composition timestamp (ticks)
	Duration int64 // ticks
	Size     uint32
	Keyframe bool
}

// Decision records one repair action with the original and repaired value.
type Decision struct {
	Index    int
	Kind     string // "nonzero-start" | "backward" | "gap" | "unlocatable"
	Original int64
	Repaired int64
}

// RepairedTrack is the repaired, unified-timebase view of one track.
type RepairedTrack struct {
	TrackID   string
	Timescale uint32
	Samples   []Sample
	Originals []manifest.RawSample
	Decisions []Decision
	// Unlocatable holds unified-tick ranges that cannot be placed uniquely;
	// the index builder must not fabricate keyframes inside them.
	Unlocatable [][2]int64
	Shift       int64 // ticks subtracted to normalize the non-zero start
	edits       []manifest.RawEdit
	movieTS     uint32
}

// Repair normalizes a track: non-zero start is shifted to zero, backward
// timestamps are clamped, gaps are recorded, and intervals whose placement
// cannot be uniquely determined are marked unlocatable. Original values
// and every repair decision are preserved.
func Repair(tr *manifest.RawTrack, movieTimescale uint32) (*RepairedTrack, error) {
	rt := &RepairedTrack{
		TrackID:   tr.ID,
		Timescale: tr.Timescale,
		Originals: append([]manifest.RawSample(nil), tr.Samples...),
		edits:     tr.EditList,
		movieTS:   movieTimescale,
	}
	if len(tr.Samples) == 0 {
		return rt, nil
	}
	rt.Shift = tr.Samples[0].DTS
	if rt.Shift != 0 {
		rt.Decisions = append(rt.Decisions, Decision{
			Index: 0, Kind: "nonzero-start", Original: rt.Shift, Repaired: 0})
	}
	med := medianDuration(tr.Samples)
	ambigLimit := 10 * med
	prevDTS, prevDur := int64(-1), int64(0)
	for i, s := range tr.Samples {
		u := s.DTS - rt.Shift // unified
		cts := s.DTS + s.CTSOffset - rt.Shift
		rep := Sample{Index: i, DTS: u, CTS: cts, Duration: s.Duration,
			Size: s.Size, Keyframe: s.Keyframe}
		if prevDTS >= 0 {
			switch {
			case u <= prevDTS:
				back := prevDTS - u
				if back > ambigLimit {
					// Cannot be uniquely placed: clamp to keep decode order
					// but mark the interval unlocatable; no keyframe is
					// fabricated for it.
					rep.DTS = prevDTS + 1
					rep.CTS = rep.DTS + (cts - u)
					rt.Unlocatable = append(rt.Unlocatable, [2]int64{u, rep.DTS})
					rt.Decisions = append(rt.Decisions, Decision{
						Index: i, Kind: "unlocatable", Original: u, Repaired: rep.DTS})
				} else {
					rep.DTS = prevDTS + 1
					rep.CTS = rep.DTS + (cts - u)
					rt.Decisions = append(rt.Decisions, Decision{
						Index: i, Kind: "backward", Original: u, Repaired: rep.DTS})
				}
			case u-(prevDTS+prevDur) > 2*prevDur && prevDur > 0:
				rt.Decisions = append(rt.Decisions, Decision{
					Index: i, Kind: "gap", Original: u, Repaired: u})
			}
		}
		rt.Samples = append(rt.Samples, rep)
		prevDTS, prevDur = rep.DTS, rep.Duration
	}
	return rt, nil
}

// ProgramMicros maps a repaired sample to program time in microseconds via
// the edit list (media time = unified + shift).
func (rt *RepairedTrack) ProgramMicros(i int) (int64, error) {
	return rt.ProgramMicrosOf(rt.Samples[i].DTS, int64(i))
}

// ProgramMicrosOf maps an arbitrary unified tick value (e.g. a CTS) to
// program time in microseconds. offset only feeds error reporting.
func (rt *RepairedTrack) ProgramMicrosOf(unified int64, offset int64) (int64, error) {
	return MediaToProgram(unified+rt.Shift, rt.Timescale, rt.edits, rt.movieTS,
		Loc{Track: rt.TrackID, Offset: offset})
}

// InUnlocatable reports whether a unified tick falls in an unlocatable range.
func (rt *RepairedTrack) InUnlocatable(t int64) bool {
	for _, r := range rt.Unlocatable {
		if t >= r[0] && t <= r[1] {
			return true
		}
	}
	return false
}

// SyncErrorMicros returns the absolute program-time offset between two
// tracks at their first common sample, computed on repaired timestamps.
func SyncErrorMicros(a, b *RepairedTrack) (int64, error) {
	if len(a.Samples) == 0 || len(b.Samples) == 0 {
		return 0, media.Corruptf("timeline.sync", a.TrackID, -1, "empty track")
	}
	pa, err := a.ProgramMicros(0)
	if err != nil {
		return 0, err
	}
	pb, err := b.ProgramMicros(0)
	if err != nil {
		return 0, err
	}
	if d := pa - pb; d >= 0 {
		return d, nil
	} else {
		return -d, nil
	}
}

func medianDuration(samples []manifest.RawSample) int64 {
	if len(samples) == 0 {
		return 1
	}
	d := make([]int64, len(samples))
	for i, s := range samples {
		d[i] = s.Duration
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	m := d[len(d)/2]
	if m <= 0 {
		m = 1
	}
	return m
}
