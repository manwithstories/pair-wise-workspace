// Package segment cuts the unified timeline into segments whose boundaries
// always land on available video random access points, and arbitrates audio
// sample ownership on the same timeline so segments never overlap in time.
package segment

import (
	"mediapack/internal/clock"
	"mediapack/internal/demux"
)

// Boundary is one cut point on the canonical timeline.
type Boundary struct {
	Ticks       int64  // canonical timeline position
	SampleIndex uint32 // keyframe sample index in the master video track
}

// Cut computes segment boundaries for the given target duration. Every cut
// lands on the nearest available keyframe of the master video track; the
// first boundary is always 0 and the implicit end is the track duration.
// Boundaries are strictly increasing and deduplicated.
func Cut(master *demux.Track, targetTicks int64) []Boundary {
	if targetTicks <= 0 {
		targetTicks = clock.TicksFromMillis(2000)
	}
	kfs := keyframeTimes(master)
	dur := clock.Ticks(master.Duration(), master.Timescale)
	bounds := []Boundary{{Ticks: 0}}
	if len(kfs) > 0 {
		// The first boundary sits on the first keyframe: there is no
		// presentable video before it, and anchoring here keeps every
		// representation exactly aligned at the stream start.
		bounds[0].Ticks = kfs[0].Ticks
		bounds[0].SampleIndex = kfs[0].Index
	}
	for target := targetTicks; target < dur; target += targetTicks {
		kf, ok := nearestKeyframe(kfs, target)
		if !ok {
			break
		}
		last := bounds[len(bounds)-1].Ticks
		if kf.Ticks <= last || kf.Ticks >= dur { // dedupe and stay inside
			continue
		}
		bounds = append(bounds, Boundary{Ticks: kf.Ticks, SampleIndex: kf.Index})
	}
	return bounds
}

type kfPoint struct {
	Ticks int64
	Index uint32
}

// keyframeTimes returns the canonical presentation times of available
// keyframe samples, sorted ascending.
func keyframeTimes(track *demux.Track) []kfPoint {
	var out []kfPoint
	for i := range track.Samples {
		s := &track.Samples[i]
		if s.Keyframe && s.Available {
			out = append(out, kfPoint{Ticks: clock.Ticks(s.PTS, track.Timescale), Index: s.Index})
		}
	}
	return out
}

// nearestKeyframe finds the keyframe closest to target ticks.
func nearestKeyframe(kfs []kfPoint, target int64) (kfPoint, bool) {
	best := -1
	for i, kf := range kfs {
		if best < 0 || abs(kf.Ticks-target) < abs(kfs[best].Ticks-target) {
			best = i
		}
	}
	if best < 0 {
		return kfPoint{}, false
	}
	return kfs[best], true
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// SegmentIndex returns the index of the segment owning a canonical
// timestamp: the last boundary <= pts. Ownership is half-open
// [boundary[i], boundary[i+1]) so audio samples never overlap segments.
func SegmentIndex(ptsTicks int64, bounds []Boundary) int {
	idx := 0
	for i, b := range bounds {
		if b.Ticks <= ptsTicks {
			idx = i
		} else {
			break
		}
	}
	return idx
}

// AssignAudio groups available audio sample indexes per segment using the
// unified timeline, guaranteeing non-overlapping timing between segments.
func AssignAudio(track *demux.Track, bounds []Boundary) [][]uint32 {
	out := make([][]uint32, len(bounds))
	for i := range track.Samples {
		s := &track.Samples[i]
		if !s.Available {
			continue
		}
		pts := clock.Ticks(s.PTS, track.Timescale)
		idx := SegmentIndex(pts, bounds)
		out[idx] = append(out[idx], s.Index)
	}
	return out
}
