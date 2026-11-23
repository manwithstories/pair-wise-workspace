// Package segments cuts a demuxed program into deliverable segments on a
// unified time base, preferring video keyframes for boundaries.
package segments

import (
	"fmt"

	"bcast/media"
)

// Boundary records where a cut actually landed versus where it was asked
// to land.
type Boundary struct {
	Ideal     media.Time // requested cut point (video timescale)
	Actual    media.Time // chosen cut point (video timescale)
	Deviation  int64      // Actual-Ideal in video timescale units
	OnKeyframe bool       // Actual is exactly Ideal and a keyframe
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
// track's timescale). Boundaries prefer keyframes; when no keyframe covers
// the ideal point the nearest available keyframe is used and the deviation
// recorded. Audio cuts align to the same absolute time base.
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
	total := vid[len(vid)-1].End().Value
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
	// Cut points: 0, aligned boundaries..., total.
	cuts := []Boundary{{Ideal: media.Time{Timescale: vt.Timescale}, Actual: media.Time{Timescale: vt.Timescale}, OnKeyframe: true}}
	for ideal := target; ideal < total; ideal += target {
		cuts = append(cuts, alignBoundary(ideal, keys, vt.Timescale))
	}
	at, hasAudio := f.TrackByKind(media.TrackAudio)
	var aud []media.Sample
	if hasAudio {
		aud = f.SamplesOf(at.ID)
	}
	var segs []Segment
	for i, b := range cuts {
		end := total
		if i+1 < len(cuts) {
			end = cuts[i+1].Actual.Value
		}
		seg := Segment{
			Index: i, Boundary: b,
			Start: media.Time{Value: b.Actual.Value, Timescale: vt.Timescale},
			End:   media.Time{Value: end, Timescale: vt.Timescale},
		}
		for _, s := range vid {
			if s.DTS.Value >= seg.Start.Value && s.DTS.Value < end {
				seg.Video = append(seg.Video, s)
			}
		}
		if hasAudio {
			aStart := seg.Start.ToTimescale(at.Timescale).Value
			aEnd := seg.End.ToTimescale(at.Timescale).Value
			for _, s := range aud {
				if s.DTS.Value >= aStart && s.DTS.Value < aEnd {
					seg.Audio = append(seg.Audio, s)
				}
			}
		}
		segs = append(segs, seg)
	}
	return segs, nil
}

// alignBoundary lands the cut on the frame at the ideal point. When that
// frame is not a keyframe, Deviation records the signed distance to the
// nearest keyframe (ties go earlier) so downstream knows how far the GOP
// boundary is; the cut itself stays frame-exact to keep segments aligned.
func alignBoundary(ideal int64, keys []int64, ts uint32) Boundary {
	b := Boundary{
		Ideal:  media.Time{Value: ideal, Timescale: ts},
		Actual: media.Time{Value: ideal, Timescale: ts},
	}
	nearest := keys[0]
	for _, k := range keys {
		if k == ideal {
			b.OnKeyframe = true
			return b
		}
		if abs(k-ideal) < abs(nearest-ideal) {
			nearest = k
		}
	}
	b.Deviation = nearest - ideal
	return b
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// KeyframeHitRate is the fraction of non-trivial boundaries that landed
// exactly on the ideal cut point.
func KeyframeHitRate(segs []Segment) (hit, total int) {
	for _, s := range segs {
		if s.Boundary.Ideal.Value == 0 {
			continue // the opening boundary is trivially aligned
		}
		total++
		if s.Boundary.OnKeyframe {
			hit++
		}
	}
	return hit, total
}
