// Package ladder aligns the segment boundaries of multiple bitrate
// representations onto one unified cut plan. When a representation has no
// keyframe within the configured tolerance of a master boundary, the gap is
// reported as a missing interval instead of silently drifting.
package ladder

import (
	"mediapack/internal/clock"
	"mediapack/internal/demux"
	"mediapack/internal/segment"
)

// RepAlignment holds the alignment result of one representation track
// against the master cut plan.
type RepAlignment struct {
	TrackID uint32
	// CutTicks[i] is the snapped boundary time for master boundary i;
	// it equals the master boundary when the track is the master itself.
	CutTicks []int64
	// ErrTicks[i] is CutTicks[i] - master boundary i (signed).
	ErrTicks []int64
	// Aligned[i] is false when no keyframe within tolerance existed.
	Aligned []bool
}

// Alignment is the aggregate result for all representations.
type Alignment struct {
	Reps []RepAlignment
	// MaxErrorTicks is the largest absolute snapping error observed.
	MaxErrorTicks int64
	// Missing lists intervals a representation cannot cover.
	Missing []demux.Interval
}

// Align snaps every master boundary to the nearest available keyframe of
// each representation track within tolTicks. The master track itself is
// expected to be the first entry of reps and always aligns exactly.
func Align(reps []*demux.Track, bounds []segment.Boundary, tolTicks int64) *Alignment {
	a := &Alignment{}
	for _, rep := range reps {
		ra := RepAlignment{
			TrackID:  rep.ID,
			CutTicks: make([]int64, len(bounds)),
			ErrTicks: make([]int64, len(bounds)),
			Aligned:  make([]bool, len(bounds)),
		}
		kfs := availableKeyframes(rep)
		for i, b := range bounds {
			best, bestDist := int64(0), int64(-1)
			found := false
			for _, kf := range kfs {
				d := kf - b.Ticks
				if d < 0 {
					d = -d
				}
				if !found || d < bestDist {
					best, bestDist, found = kf, d, true
				}
			}
			if found && bestDist <= tolTicks {
				ra.CutTicks[i] = best
				ra.ErrTicks[i] = best - b.Ticks
				ra.Aligned[i] = true
				if bestDist > a.MaxErrorTicks {
					a.MaxErrorTicks = bestDist
				}
			} else {
				// No keyframe within tolerance: keep the master time so
				// numbering stays consistent, flag the gap explicitly.
				ra.CutTicks[i] = b.Ticks
				ra.Aligned[i] = false
				end := b.Ticks + tolTicks
				if i+1 < len(bounds) {
					end = bounds[i+1].Ticks
				}
				a.Missing = append(a.Missing, demux.Interval{
					StartTicks: b.Ticks,
					EndTicks:   end,
					Reason:     "representation track has no keyframe within tolerance",
				})
			}
		}
		a.Reps = append(a.Reps, ra)
	}
	return a
}

// availableKeyframes returns sorted canonical keyframe times of a track.
func availableKeyframes(track *demux.Track) []int64 {
	var out []int64
	for i := range track.Samples {
		s := &track.Samples[i]
		if s.Keyframe && s.Available {
			out = append(out, clock.Ticks(s.PTS, track.Timescale))
		}
	}
	return out
}
