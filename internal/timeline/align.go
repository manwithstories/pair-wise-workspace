// Package timeline aligns clip points and transitions onto the unified
// nanosecond axis and normalizes per-track audio/colour metadata.
package timeline

import (
	"fmt"

	"avscan/internal/model"
)

// AlignedPoint is one clip/transition boundary snapped to a real sample grid.
type AlignedPoint struct {
	Label      string
	TrackID    uint32
	RequestedNS int64 // operator-supplied position
	SnappedNS   int64 // position after snapping to a sample boundary
	// RoundingNS is the signed difference SnappedNS - RequestedNS. A non-zero
	// value is always surfaced; the pipeline never drifts silently.
	RoundingNS  int64
	SampleIndex int64 // 0-based sample carrying the snapped point; -1 if none
}

// AlignmentReport covers every requested point and the faults found.
type AlignmentReport struct {
	Points []AlignedPoint
	Faults []model.Fault
}

// AlignClips maps clip points onto each affected track's sample grid. A point
// with TrackID 0 applies to every track and is snapped independently per track.
func AlignClips(tl *model.Timeline, clips []model.ClipPoint) AlignmentReport {
	rep := AlignmentReport{}
	for _, cp := range clips {
		targets := tl.Tracks
		if cp.TrackID != 0 {
			targets = findTracks(tl, cp.TrackID)
			if len(targets) == 0 {
				rep.Faults = append(rep.Faults, model.Fault{
					Kind: model.FaultClipOutOfBounds, Source: "clip",
					TrackID: cp.TrackID, SampleIndex: -1,
					TimeNS: cp.AtNS, HasTime: true,
					Message: fmt.Sprintf("clip %q references unknown track %d", cp.Label, cp.TrackID),
				})
				continue
			}
		}
		for _, tr := range targets {
			pt, fault := alignOne(tr, cp)
			rep.Points = append(rep.Points, pt)
			if fault != nil {
				rep.Faults = append(rep.Faults, *fault)
			}
		}
	}
	return rep
}

// alignOne snaps one point to the nearest sample start of one track.
func alignOne(tr *model.Track, cp model.ClipPoint) (AlignedPoint, *model.Fault) {
	pt := AlignedPoint{
		Label: cp.Label, TrackID: tr.ID, RequestedNS: cp.AtNS,
		SampleIndex: -1,
	}
	if len(tr.Samples) == 0 {
		return pt, &model.Fault{
			Kind: model.FaultUnparseableTrack, Source: "clip",
			TrackID: tr.ID, SampleIndex: -1, TimeNS: cp.AtNS, HasTime: true,
			Message: "track has no samples to align to",
		}
	}
	last := tr.Samples[len(tr.Samples)-1]
	trackEnd := last.StartNS + last.DurationNS
	if cp.AtNS < 0 || cp.AtNS > trackEnd {
		return pt, &model.Fault{
			Kind: model.FaultClipOutOfBounds, Source: "clip",
			TrackID: tr.ID, SampleIndex: -1, TimeNS: cp.AtNS, HasTime: true,
			Message: fmt.Sprintf("clip %q at %d ns outside track [0,%d]",
				cp.Label, cp.AtNS, trackEnd),
		}
	}
	idx := nearestSample(tr.Samples, cp.AtNS)
	ev := tr.Samples[idx]
	pt.SampleIndex = int64(idx)
	pt.SnappedNS = ev.StartNS
	pt.RoundingNS = ev.StartNS - cp.AtNS
	return pt, nil
}

// nearestSample returns the index of the sample whose start is closest to t,
// breaking ties toward the earlier sample (deterministic, no hidden drift).
func nearestSample(samples []model.SampleEvent, t int64) int {
	lo, hi := 0, len(samples)-1
	for lo < hi {
		mid := (lo + hi) / 2
		if samples[mid].StartNS < t {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo > 0 {
		prev := samples[lo-1]
		cur := samples[lo]
		if t-prev.StartNS <= cur.StartNS-t {
			return lo - 1
		}
	}
	return lo
}

func findTracks(tl *model.Timeline, id uint32) []*model.Track {
	for _, tr := range tl.Tracks {
		if tr.ID == id {
			return []*model.Track{tr}
		}
	}
	return nil
}

// NonIntegralRounding returns the points that could not land exactly on a
// sample block boundary, so callers can surface the rounding explicitly.
func (r *AlignmentReport) NonIntegralRounding() []AlignedPoint {
	out := make([]AlignedPoint, 0)
	for _, p := range r.Points {
		if p.RoundingNS != 0 {
			out = append(out, p)
		}
	}
	return out
}
