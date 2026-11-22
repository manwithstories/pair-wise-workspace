// Package fixture builds fully synthetic broadcast delivery packages:
// a self-consistent MP4-like box stream, RTP packet sequences and
// timed metadata events. Normal, damaged and boundary cases are all
// produced in code without any external media.
package fixture

import (
	"fmt"

	"github.com/pion/rtp"

	"mediacheck/internal/metadata"
)

// Package is one generated delivery package.
type Package struct {
	MP4        []byte
	RTPPackets []*rtp.Packet
	Samples    map[string][]Sample
	Events     []metadata.Event
	Windows    []metadata.TrackWindow
	// RTPGap marks the dropped burst span on the ms timeline.
	RTPGap Span
	// RTPReorderable is the number of burst sequences delivered late.
	RTPReorderable int
	// GroundTruth lists the defects the scenario intends to produce.
	GroundTruth []GroundTruth
}

// Generate builds the package described by cfg.
func Generate(cfg Config) (*Package, error) {
	if cfg.DurationMs <= 0 {
		return nil, fmt.Errorf("fixture: duration must be positive, got %d", cfg.DurationMs)
	}
	if cfg.VideoTS == 0 || cfg.AudioTS == 0 {
		return nil, fmt.Errorf("fixture: timescales must be non-zero")
	}
	pkg := &Package{Samples: map[string][]Sample{}}

	video := buildSamples(videoTrack, cfg.DurationMs, frameMs, true, cfg.Seed)
	audio := buildSamples(audioTrack, cfg.DurationMs, 100, true, cfg.Seed+1)
	if cfg.Profile == ProfileNoKeyframe {
		for i := range video {
			video[i].IsSync = false
		}
	}
	if cfg.Profile == ProfileCorrupt {
		injectCorruption(video, cfg.DurationMs)
	}
	if cfg.DriftMs != 0 || cfg.Profile == ProfileDrift {
		applyDrift(audio, cfg.DriftMs)
	}
	pkg.Samples[videoTrack] = video
	pkg.Samples[audioTrack] = audio

	pkg.MP4 = buildMP4(cfg, video, audio)
	pkg.RTPPackets, pkg.RTPGap, pkg.RTPReorderable = buildRTP(cfg)
	pkg.Events = buildEvents(cfg)
	pkg.Windows = []metadata.TrackWindow{
		{Track: videoTrack, StartMs: 0, EndMs: cfg.DurationMs},
		{Track: audioTrack, StartMs: 0, EndMs: cfg.DurationMs},
	}
	pkg.GroundTruth = groundTruth(cfg)
	return pkg, nil
}

// buildSamples creates one sample every frameMs, marking sync samples
// at each GOP boundary. Generation is purely arithmetic and stable.
func buildSamples(track string, duration, frameMs int64, sync bool, seed int64) []Sample {
	out := []Sample{}
	var t int64
	idx := 0
	for t < duration {
		size := 64 + int((seed+int64(idx)*7)%128)
		out = append(out, Sample{
			Track: track, StartMs: t, DurMs: frameMs,
			IsSync: sync && (t%gopMs == 0), Size: size,
		})
		t += frameMs
		idx++
	}
	return out
}

func injectCorruption(video []Sample, duration int64) {
	// Damage a bounded middle span; surrounding samples stay valid.
	start := duration / 3
	end := duration/3 + 300
	for i := range video {
		if video[i].StartMs >= start && video[i].StartMs < end {
			video[i].Corrupt = true
		}
	}
}

func applyDrift(audio []Sample, drift int64) {
	if drift == 0 {
		drift = 120
	}
	for i := range audio {
		audio[i].StartMs += drift
	}
}

func groundTruth(cfg Config) []GroundTruth {
	gt := []GroundTruth{}
	switch cfg.Profile {
	case ProfileTruncated:
		gt = append(gt, GroundTruth{Kind: "truncated_box", Track: "container", StartMs: 0})
	case ProfileCorrupt:
		gt = append(gt, GroundTruth{Kind: "corrupt_sample", Track: videoTrack,
			StartMs: cfg.DurationMs / 3, EndMs: cfg.DurationMs/3 + 300})
	case ProfileNoKeyframe:
		gt = append(gt, GroundTruth{Kind: "missing_keyframe", Track: videoTrack, EndMs: cfg.DurationMs})
	case ProfileDrift:
		gt = append(gt, GroundTruth{Kind: "clock_drift", Track: audioTrack})
	}
	if cfg.LossBurst > 0 {
		gt = append(gt, GroundTruth{Kind: "rtp_gap", Track: videoTrack})
	}
	return gt
}
