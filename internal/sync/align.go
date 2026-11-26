package sync

import (
	"math"
	"sort"

	"mediaqc/internal/media"
)

// Frame is one time-aligned track position on the shared 90 kHz clock.
type Frame struct {
	Stream        string `json:"stream"`
	Index         int    `json:"index"`
	PTS           uint64 `json:"pts"`
	DurationTicks uint64 `json:"duration_ticks"`
	Gap           bool   `json:"gap"`
}

// OffsetSample is one video-to-audio alignment observation.
type OffsetSample struct {
	VideoIndex    int     `json:"video_index"`
	AtMS          float64 `json:"at_ms"`
	OffsetMS      float64 `json:"offset_ms"`
	AbsMS         float64 `json:"abs_ms"`
	GapPropagated bool    `json:"gap_propagated"`
}

// Report contains AV alignment samples and quantiles.
type Report struct {
	Samples        []OffsetSample `json:"samples"`
	P95AbsMS       float64        `json:"av_sync_p95_ms"`
	MaxAbsMS       float64        `json:"max_abs_ms"`
	PropagatedGaps int            `json:"propagated_gaps"`
	MaxGapOffsetMS float64        `json:"max_gap_offset_ms"`
}

// Align maps every video frame to the nearest audio position on the common
// media clock. Explicit audio gaps propagate to affected video frames instead
// of being hidden or synthesized.
func Align(audio, video []Frame) *Report {
	rep := &Report{}
	if len(video) == 0 {
		return rep
	}

	for _, v := range video {
		sample := OffsetSample{VideoIndex: v.Index, AtMS: media.TicksToMS(v.PTS)}

		if gapFrame, inGap := findGapInterval(audio, v); inGap {
			ref := nearestDecodable(audio, v.PTS)
			signed := signedTicksMS(v.PTS, ref)
			sample.OffsetMS = signed
			sample.AbsMS = math.Abs(signed)
			sample.GapPropagated = true
			rep.PropagatedGaps++
			if sample.AbsMS > rep.MaxGapOffsetMS {
				rep.MaxGapOffsetMS = sample.AbsMS
			}
			_ = gapFrame
		} else {
			ref := nearestAny(audio, v.PTS)
			signed := signedTicksMS(v.PTS, ref)
			sample.OffsetMS = signed
			sample.AbsMS = math.Abs(signed)
		}

		rep.Samples = append(rep.Samples, sample)
		if sample.AbsMS > rep.MaxAbsMS {
			rep.MaxAbsMS = sample.AbsMS
		}
	}

	values := make([]float64, 0, len(rep.Samples))
	for _, s := range rep.Samples {
		values = append(values, s.AbsMS)
	}
	rep.P95AbsMS = Percentile(values, 0.95)
	return rep
}

func findGapInterval(audio []Frame, v Frame) (Frame, bool) {
	for _, a := range audio {
		if !a.Gap || a.DurationTicks == 0 {
			continue
		}
		end := a.PTS + a.DurationTicks
		if v.PTS >= a.PTS && v.PTS < end {
			return a, true
		}
	}
	return Frame{}, false
}

func nearestAny(audio []Frame, pts uint64) uint64 {
	return nearest(audio, pts, false)
}

func nearestDecodable(audio []Frame, pts uint64) uint64 {
	return nearest(audio, pts, true)
}

func nearest(audio []Frame, pts uint64, decodableOnly bool) uint64 {
	best := uint64(0)
	bestDist := uint64(math.MaxUint64)
	found := false
	for _, a := range audio {
		if decodableOnly && a.Gap {
			continue
		}
		d := absUint(a.PTS, pts)
		if !found || d < bestDist {
			best, bestDist, found = a.PTS, d, true
		}
	}
	if !found {
		return pts
	}
	return best
}

func signedTicksMS(videoPTS, audioPTS uint64) float64 {
	if videoPTS >= audioPTS {
		return media.TicksToMS(videoPTS - audioPTS)
	}
	return -media.TicksToMS(audioPTS - videoPTS)
}

func absUint(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

// Percentile uses the nearest-rank method and never interpolates away a tail.
func Percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}
