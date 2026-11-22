// Package sync enforces a single audio/video presentation timeline. It
// validates video timestamps (missing, regressive, clock-boundary crossings),
// applies explicit degradation policies instead of crashing, and pairs each
// video frame with the nearest audio block while measuring drift.
package sync

import (
	"mediakernel/internal/clock"
	"mediakernel/internal/model"
)

// AlignConfig bounds acceptable drift and selects degradation behavior.
type AlignConfig struct {
	// MaxDrift is the absolute audio/video drift that still counts as aligned.
	MaxDrift clock.Ticks
	// FrameStep is the expected nominal spacing between video frames.
	FrameStep clock.Ticks
	// Boundary is the crossing magnitude at which timestamps are treated as a
	// clock wrap rather than a regression.
	Boundary clock.Ticks
}

// DefaultAlignConfig matches 30fps video against 48kHz audio.
func DefaultAlignConfig() AlignConfig {
	return AlignConfig{
		MaxDrift:  clock.Milliseconds(60),
		FrameStep: clock.MillisecondsF(1000.0 / 30.0),
		Boundary:  clock.Milliseconds(1000),
	}
}

// Aligner is the stateful presentation-time policy engine.
type Aligner struct {
	cfg      AlignConfig
	log      *model.Log
	lastPTS  clock.Ticks
	hasLast  bool
	frameSeq int
	drifts   []int64
	degraded int
	syncErrs int
}

// NewAligner creates an aligner sharing the pipeline fault log.
func NewAligner(cfg AlignConfig, log *model.Log) *Aligner {
	if cfg.MaxDrift <= 0 || cfg.FrameStep <= 0 {
		cfg = DefaultAlignConfig()
	}
	return &Aligner{cfg: cfg, log: log}
}

// ValidateVideo applies the timestamp degradation policy to one frame. It
// returns the PTS the frame should be presented at, which is synthesized when
// the original was unusable, plus a degradation flag and reason.
func (a *Aligner) ValidateVideo(frame *model.VideoFrame) (pts clock.Ticks, degraded bool, reason string) {
	pts = frame.PTS
	if frame.PTS == 0 {
		degraded = true
		reason = "missing video timestamp; synthesized from frame step"
		pts = a.synthesizedPTS()
		a.recordDegrade(frame.FrameID, pts, reason)
	} else if a.hasLast && frame.PTS < a.lastPTS {
		if a.lastPTS-frame.PTS >= a.cfg.Boundary {
			degraded = true
			reason = "video clock boundary crossing; remapped onto unified timeline"
			pts = a.synthesizedPTS()
			a.recordDegrade(frame.FrameID, pts, reason)
		} else {
			degraded = true
			reason = "regressive video timestamp; replaced by monotonic projection"
			pts = a.synthesizedPTS()
			a.recordDegrade(frame.FrameID, pts, reason)
		}
	}
	a.lastPTS = pts
	a.hasLast = true
	return pts, degraded, reason
}

func (a *Aligner) synthesizedPTS() clock.Ticks {
	if !a.hasLast {
		return 0
	}
	return a.lastPTS + a.cfg.FrameStep
}

func (a *Aligner) recordDegrade(frameID uint32, pts clock.Ticks, reason string) {
	a.degraded++
	a.syncErrs++
	a.log.Add(model.MediaError{
		Category: model.CatSync,
		At:       pts,
		Detail:   reason,
		Decision: model.DecDegraded,
	})
	_ = frameID
}

// Align pairs one video frame with the nearest available audio block. The
// audio block may be nil when none has been produced yet, which is itself a
// degradation. Drift is measured as |frame PTS - block start| and accumulated
// for percentile reporting.
func (a *Aligner) Align(frame model.VideoFrame, pts clock.Ticks, degraded bool, reason string, audio *model.AudioBlock) model.AlignedFrame {
	out := model.AlignedFrame{
		Frame:       frame,
		Audio:       audio,
		Degraded:    degraded,
		Reason:      reason,
		OutputOrder: a.frameSeq,
	}
	a.frameSeq++
	if audio == nil {
		out.Degraded = true
		if out.Reason == "" {
			out.Reason = "no audio block available for frame"
		}
		a.syncErrs++
		a.log.Add(model.MediaError{
			Category: model.CatSync,
			At:       pts,
			Detail:   "frame presented without paired audio",
			Decision: model.DecDegraded,
		})
		out.DriftMS = a.cfg.MaxDrift.Milliseconds()
	} else {
		drift := pts - audio.Start
		if drift < 0 {
			drift = -drift
		}
		out.DriftMS = drift.Milliseconds()
	}
	a.drifts = append(a.drifts, out.DriftMS)
	if audio != nil && out.DriftMS > a.cfg.MaxDrift.Milliseconds() {
		a.syncErrs++
		a.log.Add(model.MediaError{
			Category: model.CatSync,
			At:       pts,
			Detail:   "audio/video drift exceeds constraint",
			Decision: model.DecDegraded,
		})
	}
	return out
}

// DriftSamples returns every measured drift in milliseconds, in order.
func (a *Aligner) DriftSamples() []int64 {
	out := make([]int64, len(a.drifts))
	copy(out, a.drifts)
	return out
}

// DegradedCount reports how many frames required a degradation policy.
func (a *Aligner) DegradedCount() int { return a.degraded }

// SyncErrors reports total sync-classified faults.
func (a *Aligner) SyncErrors() int { return a.syncErrs }
