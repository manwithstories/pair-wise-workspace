// Package pipeline wires the media-kernel stages into one deterministic
// processing engine: RTP-classified events enter a bounded jitter window, the
// watermark seals stable intervals, late events trigger fixed-window replay,
// PCM loss is reconstructed, bandwidth steers the buffer target, and the
// aligner enforces a single audio/video timeline. All outputs and metrics are
// reproducible for the same (scenario, seed).
package pipeline

import (
	"mediakernel/internal/bandwidth"
	"mediakernel/internal/clock"
	"mediakernel/internal/model"
	"mediakernel/internal/recovery"
	"mediakernel/internal/sync"
)

// Config assembles the per-stage policies used by an Engine run.
type Config struct {
	Scenario string
	// FramesPerPacket is the audio frame count carried by one RTP packet.
	FramesPerPacket int
	// Channels is the PCM channel count.
	Channels int
	// WindowMaxEvents hard-caps jitter-window memory.
	WindowMaxEvents int
	// ReplayWindow is how far behind the watermark a late packet still
	// triggers deterministic recomputation.
	ReplayWindow clock.Ticks
	// Recovery chooses the silence/interpolation policy.
	Recovery recovery.Policy
	// Align controls audio/video drift constraints.
	Align sync.AlignConfig
	// Bandwidth configures the adaptive buffer target.
	Bandwidth bandwidth.EstimatorConfig
	// FaultLogLimit bounds the classified fault log.
	FaultLogLimit int
}

// DefaultConfig returns simulation-scale, bounded defaults.
func DefaultConfig(scenario string) Config {
	return Config{
		Scenario:        scenario,
		FramesPerPacket: 960,
		Channels:        2,
		WindowMaxEvents: 2048,
		ReplayWindow:    clock.Milliseconds(500),
		Recovery:        recovery.DefaultPolicy(),
		Align:           sync.DefaultAlignConfig(),
		Bandwidth:       bandwidth.DefaultEstimatorConfig(),
		FaultLogLimit:   4096,
	}
}

// BandwidthPoint is one external available-bandwidth observation on the
// unified timeline.
type BandwidthPoint struct {
	At          clock.Ticks
	BytesPerSec float64
}

// Input is everything one Engine run consumes.
type Input struct {
	// AudioEvents are network-order (arrival-order) audio events.
	AudioEvents []*model.Event
	// VideoFrames are presentation-order interactive frames, possibly with
	// degraded timestamps.
	VideoFrames []model.VideoFrame
	// Bandwidth describes the available-bandwidth curve.
	Bandwidth []BandwidthPoint
}

// Output is the deterministic product of one run.
type Output struct {
	Metrics model.Metrics
	// Frames is the final aligned presentation sequence.
	Frames []model.AlignedFrame
	// Audio is the fully recovered audio timeline.
	Audio *model.AudioBlock
	// Faults exposes the classified, timeline-anchored fault log.
	Faults []model.MediaError
}
