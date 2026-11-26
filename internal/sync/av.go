// Package sync aligns recovered audio and video on the rational clock basis.
package sync

import (
	"medialab/internal/clock"
	"medialab/internal/model"
)

const (
	// DriftThresholdMS is the skew above which compensation is applied.
	DriftThresholdMS int64 = 20
	// MaxCorrectionMS bounds one sync correction.
	MaxCorrectionMS int64 = 80
	// MaxSilenceMS is the bounded audio concealment budget per gap.
	MaxSilenceMS int64 = 80
)

// Frame is one recovered media frame on a rational time base.
type Frame struct {
	Segment    int
	TrackID    uint32
	Kind       string
	PTS        clock.Time
	DurationMS int64
	Keyframe   bool
	Verified   bool
	Present    bool
	State      model.RecoveryState
}

// Decision records the action taken for one segment.
type Decision struct {
	Segment         int
	RawSkewMS       int64
	CorrectedSkewMS int64
	CorrectionMS    int64
	SilenceMS       int64
	VideoAction     string
	AudioAction     string
}

// Report is the deterministic A/V alignment result.
type Report struct {
	Decisions         []Decision
	SilenceInsertedMS int64
	VideoDropped      int
	KeyframeWaits     int
	MaxRawSkewMS      int64
	AVSkewMS          int64
}

// Align synchronizes segment-indexed audio and video frames. Video damaged
// frames are dropped until a present, hash-verified keyframe. Missing audio is
// compensated with bounded silence.
func Align(video, audio []Frame) *Report {
	vm := make(map[int]Frame, len(video))
	am := make(map[int]Frame, len(audio))
	maxSegment := -1
	for _, f := range video {
		vm[f.Segment] = f
		maxSegment = maxInt(maxSegment, f.Segment)
	}
	for _, f := range audio {
		am[f.Segment] = f
		maxSegment = maxInt(maxSegment, f.Segment)
	}
	rep := &Report{}
	waitingForKeyframe := false
	for seg := 0; seg <= maxSegment; seg++ {
		v, vok := vm[seg]
		a, aok := am[seg]
		d := Decision{Segment: seg, VideoAction: "render", AudioAction: "render"}

		if !vok || !v.Present || v.State == model.StateDropped || (v.Keyframe && !v.Verified) {
			d.VideoAction = "drop_wait_verified_keyframe"
			rep.VideoDropped++
			rep.KeyframeWaits++
			waitingForKeyframe = true
		} else if waitingForKeyframe {
			if v.Keyframe && v.Verified {
				waitingForKeyframe = false
				d.VideoAction = "resume_verified_keyframe"
			} else {
				d.VideoAction = "drop_wait_verified_keyframe"
				rep.VideoDropped++
				rep.KeyframeWaits++
			}
		}

		vms := ptsMS(v, vok, int64(seg)*33)
		ams := ptsMS(a, aok, int64(seg)*20)
		d.RawSkewMS = vms - ams
		rep.MaxRawSkewMS = maxAbs(rep.MaxRawSkewMS, d.RawSkewMS)

		if !aok || !a.Present || a.State == model.StateDropped {
			d.AudioAction = "silence_conceal"
			d.SilenceMS = minInt64(aDuration(a), MaxSilenceMS)
			rep.SilenceInsertedMS += d.SilenceMS
		}

		if abs64(d.RawSkewMS) > DriftThresholdMS {
			d.CorrectionMS = clamp64(d.RawSkewMS, -MaxCorrectionMS, MaxCorrectionMS)
		}
		d.CorrectedSkewMS = d.RawSkewMS - d.CorrectionMS
		rep.AVSkewMS = maxAbs(rep.AVSkewMS, d.CorrectedSkewMS)
		rep.Decisions = append(rep.Decisions, d)
	}
	return rep
}

func ptsMS(f Frame, ok bool, fallback int64) int64 {
	if !ok {
		return fallback
	}
	return f.PTS.Milliseconds()
}

func aDuration(f Frame) int64 {
	if f.DurationMS <= 0 {
		return 20
	}
	return f.DurationMS
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func maxAbs(cur int64, v int64) int64 {
	a := abs64(v)
	if a > cur {
		return a
	}
	return cur
}

func clamp64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
