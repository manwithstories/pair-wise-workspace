// Package sync aligns audio, video and multi-camera tracks onto one shared
// nanosecond time base, correcting device clock skew and reporting the
// residual A/V drift and any transport gaps that intersect the timeline.
package sync

import (
	"mediad/internal/index"
	"mediad/internal/model"
	"mediad/internal/transport"
)

// TrackView is one aligned track after clock correction.
type TrackView struct {
	TrackIdx int
	Handler  string
	IsVideo  bool
	IsAudio  bool
	OffsetNS model.Timestamp // constant offset applied to the raw PTS
	SkewPPM  float64         // measured clock skew in parts per million
	Samples  []index.Sample
}

// Report is the deterministic output of one alignment pass.
type Report struct {
	AudioVideoDriftMS int64           `json:"av_drift_ms"`
	MaxSkewPPM        float64         `json:"max_skew_ppm"`
	Tracks            []TrackView     `json:"tracks"`
	Gaps              []transport.Gap `json:"gaps"`
	Faults            []*model.Fault  `json:"faults"`
}

// Aligner corrects per-track offsets and skew against a chosen anchor. The
// anchor is the first video track (or the first track when no video exists),
// which makes multi-camera inputs share a single reference clock.
type Aligner struct {
	// SkewLimitPPM rejects implausible device clock drift above this value.
	SkewLimitPPM float64
}

// NewAligner returns an aligner with the default acceptance limit.
func NewAligner() *Aligner { return &Aligner{SkewLimitPPM: 50000} }

// estimateSkew derives the linear skew (ppm) of a track relative to the
// anchor by comparing the span between first and last PTS. Identical spans
// yield zero skew.
func estimateSkew(anchorSpan, trackSpan model.Timestamp) float64 {
	if anchorSpan <= 0 || trackSpan <= 0 {
		return 0
	}
	return float64(trackSpan-anchorSpan) / float64(anchorSpan) * 1e6
}

// Align builds corrected TrackViews for every indexed track and folds the
// transport gaps into the shared fault model so index/transport errors
// surface in the synchronization result.
func (a *Aligner) Align(idx *index.FileIndex, gaps []transport.Gap) Report {
	rep := Report{Gaps: gaps}
	if len(idx.Tracks) == 0 {
		rep.Faults = append(rep.Faults, model.NewFault(model.FaultMissingIndex,
			model.OffsetUnknown, idx.Name, "no tracks available for alignment"))
		return rep
	}
	anchor := 0
	for i, tr := range idx.Tracks {
		if tr.IsVideo {
			anchor = i
			break
		}
	}
	anchorSpan := spanOf(idx.Tracks[anchor].Samples)

	for i, tr := range idx.Tracks {
		view := TrackView{
			TrackIdx: i, Handler: tr.Handler, IsVideo: tr.IsVideo,
			IsAudio: tr.IsAudio, Samples: tr.Samples,
		}
		if i != anchor {
			view.SkewPPM = estimateSkew(anchorSpan, spanOf(tr.Samples))
			// constant offset aligns first presentation times
			if len(tr.Samples) > 0 && len(idx.Tracks[anchor].Samples) > 0 {
				view.OffsetNS = idx.Tracks[anchor].Samples[0].PTS - tr.Samples[0].PTS
			}
		}
		if view.SkewPPM > a.SkewLimitPPM || view.SkewPPM < -a.SkewLimitPPM {
			rep.Faults = append(rep.Faults, model.NewFault(
				model.FaultContainerBounds, model.OffsetUnknown, idx.Name,
				"clock skew beyond limit"))
		}
		if absF(view.SkewPPM) > absF(rep.MaxSkewPPM) {
			rep.MaxSkewPPM = view.SkewPPM
		}
		rep.Tracks = append(rep.Tracks, view)
	}
	rep.AudioVideoDriftMS = measureAVDrift(idx.Tracks)
	rep.Faults = append(rep.Faults, transport.GapsToFaults(gaps, idx.Name)...)
	rep.Faults = append(rep.Faults, idx.Faults...)
	return rep
}

// spanOf returns the PTS span covered by a sample slice.
func spanOf(samples []index.Sample) model.Timestamp {
	if len(samples) < 2 {
		return 0
	}
	return samples[len(samples)-1].PTS - samples[0].PTS
}

// measureAVDrift compares the average audio/video presentation interval at
// the shared boundary. The synthetic fixtures keep this near zero after
// alignment; the value is always reported as a non-negative millisecond int.
func measureAVDrift(tracks []index.Track) int64 {
	var audio, video *index.Track
	for i := range tracks {
		switch {
		case tracks[i].IsAudio:
			audio = &tracks[i]
		case tracks[i].IsVideo:
			video = &tracks[i]
		}
	}
	if audio == nil || video == nil {
		return 0
	}
	aMid := midPTS(audio.Samples)
	vMid := midPTS(video.Samples)
	if aMid < 0 || vMid < 0 {
		return 0
	}
	return model.DriftMs(aMid, vMid)
}

// midPTS returns the PTS of the median sample, or -1 when empty.
func midPTS(samples []index.Sample) model.Timestamp {
	if len(samples) == 0 {
		return -1
	}
	return samples[len(samples)/2].PTS
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
