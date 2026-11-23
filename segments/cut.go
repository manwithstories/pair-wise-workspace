// Package segments cuts a demuxed program into deliverable segments on a
// unified time base, preferring video keyframes for boundaries.
package segments

import (
	"fmt"

	"bcast/media"
)

// Boundary records where a cut actually landed versus where it was asked to
// land. Cuts always land on a real video frame start; when that frame is not
// a keyframe the distance to the nearest keyframe is recorded so downstream
// knows how far the GOP boundary is.
type Boundary struct {
	Ideal     media.Time // requested cut point (video timescale)
	Actual    media.Time // chosen video frame start (video timescale)
	Deviation int64      // Actual-Ideal: frame-snap error, video timescale
	// KeyframeDist is nearest keyframe start minus Actual, signed. Zero when
	// the boundary frame is itself a keyframe.
	KeyframeDist int64
	OnKeyframe   bool // Actual is a keyframe start
}

// Segment is one cut of the program with its samples per track.
type Segment struct {
	Index    int
	Start    media.Time // video timescale
	End      media.Time // video timescale
	Video    []media.Sample
	Audio    []media.Sample
	Boundary Boundary // the boundary that opened this segment
}

// Cut splits f into segments of target duration (expressed in the video
// track's timescale). Every boundary is snapped to a real video frame; when
// that frame is not a keyframe the keyframe distance is recorded. Audio
// cuts are selected on the same absolute time base, so a cut at video time t
// starts at the first audio sample whose start is at or after t.
func Cut(f *media.File, target int64) ([]Segment, error) {
	vt, ok := f.TrackByKind(media.TrackVideo)
	if !ok {
		return nil, media.Fatal("segments.cut", fmt.Errorf("no video track"))
	}
	if target <= 0 {
		return nil, media.Fatal("segments.cut", fmt.Errorf("non-positive target"))
	}
	vid := f.SamplesOf(vt.ID)
	if len(vid) == 0 {
		return nil, media.Fatal("segments.cut", fmt.Errorf("no video samples"))
	}
	var keys []int64
	for _, s := range vid {
		if s.Keyframe {
			keys = append(keys, s.DTS.Value)
		}
	}
	if len(keys) == 0 {
		return nil, media.Blocked("segments.cut", "index.keyframes",
			fmt.Errorf("no keyframes available"))
	}
	total := vid[len(vid)-1].End().Value
	if total <= 0 {
		return nil, media.Fatal("segments.cut", fmt.Errorf("empty video timeline"))
	}
	// Requested cut points: 0, target, 2*target, ... below the program end.
	ideals := []int64{0}
	for ideal := target; ideal < total; ideal += target {
		ideals = append(ideals, ideal)
	}
	// Snap each requested point to a frame, then require strictly increasing
	// boundaries so a target shorter than the frame interval cannot emit
	// empty or overlapping segments.
	bounds := make([]Boundary, 0, len(ideals))
	for _, ideal := range ideals {
		b := snapBoundary(ideal, vid, keys, vt.Timescale)
		if n := len(bounds); n > 0 && b.Actual.Value <= bounds[n-1].Actual.Value {
			continue
		}
		bounds = append(bounds, b)
	}
	at, hasAudio := f.TrackByKind(media.TrackAudio)
	segs := make([]Segment, 0, len(bounds))
	vcursor, acursor := 0, 0
	for i, b := range bounds {
		end := total
		if i+1 < len(bounds) {
			end = bounds[i+1].Actual.Value
		}
		seg := Segment{
			Index: i, Boundary: b,
			Start: media.Time{Value: b.Actual.Value, Timescale: vt.Timescale},
			End:   media.Time{Value: end, Timescale: vt.Timescale},
		}
		for vcursor < len(vid) && vid[vcursor].DTS.Value < b.Actual.Value {
			vcursor++
		}
		for vcursor < len(vid) && vid[vcursor].DTS.Value < end {
			seg.Video = append(seg.Video, vid[vcursor])
			vcursor++
		}
		if hasAudio {
			aStart := seg.Start.ToTimescale(at.Timescale).Value
			aEnd := seg.End.ToTimescale(at.Timescale).Value
			aud := f.SamplesOf(at.ID)
			for acursor < len(aud) && aud[acursor].DTS.Value < aStart {
				acursor++
			}
			for acursor < len(aud) && aud[acursor].DTS.Value < aEnd {
				seg.Audio = append(seg.Audio, aud[acursor])
				acursor++
			}
		}
		segs = append(segs, seg)
	}
	return segs, nil
}

// snapBoundary lands the cut on the video frame start nearest to ideal,
// ties going earlier. KeyframeDist reports how far the nearest keyframe is
// from the chosen frame; OnKeyframe is set when the chosen frame is one.
func snapBoundary(ideal int64, vid []media.Sample, keys []int64, ts uint32) Boundary {
	idx := nearestFrame(ideal, vid)
	actual := vid[idx].DTS.Value
	b := Boundary{
		Ideal:     media.Time{Value: ideal, Timescale: ts},
		Actual:    media.Time{Value: actual, Timescale: ts},
		Deviation: actual - ideal,
	}
	if vid[idx].Keyframe {
		b.OnKeyframe = true
		return b
	}
	nearest := keys[0]
	for _, k := range keys {
		if abs(k-actual) < abs(nearest-actual) {
			nearest = k
		}
	}
	b.KeyframeDist = nearest - actual
	return b
}

// nearestFrame returns the index of the frame whose start is closest to t.
func nearestFrame(t int64, vid []media.Sample) int {
	lo, hi := 0, len(vid)-1
	for lo < hi {
		mid := (lo + hi) / 2
		if vid[mid].DTS.Value < t {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	// lo is the first frame at or after t; compare with its predecessor.
	if lo > 0 && abs(vid[lo-1].DTS.Value-t) <= abs(vid[lo].DTS.Value-t) {
		return lo - 1
	}
	return lo
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// KeyframeHitRate is the fraction of non-trivial boundaries that landed on a
// keyframe. The opening boundary is excluded: it is keyframe-aligned by
// definition and would inflate the rate.
func KeyframeHitRate(segs []Segment) (hit, total int) {
	for _, s := range segs {
		if s.Boundary.Ideal.Value == 0 {
			continue
		}
		total++
		if s.Boundary.OnKeyframe {
			hit++
		}
	}
	return hit, total
}
