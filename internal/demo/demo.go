// Package demo orchestrates the self-contained end-to-end demonstration:
// it generates the fixture asset and container, runs the pipeline with a
// simulated interrupt and resume, then validates and prints every metric.
package demo

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"

	"avpipe/internal/fixture"
	"avpipe/internal/ingest"
	"avpipe/internal/media"
	"avpipe/internal/pipeline"
	"avpipe/internal/transport"
)

// Options mirrors the demo CLI flags.
type Options struct {
	OutDir         string
	Frames         uint32
	FPS            uint32
	Segment        string
	Targets        []int
	Seed           int64
	Faults         string
	InterruptAfter int
	Resume         bool
}

// Run performs the full demonstration and returns the joined final report.
func Run(opt Options) (*pipeline.Report, error) {
	if opt.FPS != media.VideoFPS {
		return nil, media.NewError(media.ClassConfig, "demo", "demo requires 30 fps", 0, false)
	}
	if opt.Segment != "1s" {
		return nil, media.NewError(media.ClassConfig, "demo", "demo requires 1s segments", 0, false)
	}
	if opt.Faults != string(fixture.FaultMixed) && opt.Faults != string(fixture.FaultNone) {
		return nil, media.NewError(media.ClassConfig, "demo", "unknown fault mode: "+opt.Faults, 0, false)
	}
	if err := checkLimits(opt); err != nil {
		return nil, err
	}

	container, frameHash, err := generateContainer(opt)
	if err != nil {
		return nil, err
	}

	base := pipeline.Config{
		OutDir: opt.OutDir, InputPath: container, FrameCount: opt.Frames, FPS: opt.FPS,
		SegmentMS: 1000, Targets: opt.Targets, Seed: opt.Seed,
		InterruptAfter: opt.InterruptAfter,
	}

	// Phase 1: run until the simulated interrupt.
	first, err := pipeline.Run(base)
	interrupted := errors.Is(err, pipeline.ErrInterrupted)
	if err != nil && !interrupted {
		return first, err
	}
	var second *pipeline.Report
	if interrupted {
		if !opt.Resume {
			return first, err
		}
		resumeCfg := base
		resumeCfg.InterruptAfter = 0
		resumeCfg.Resume = true
		second, err = pipeline.Run(resumeCfg)
		if err != nil {
			return second, err
		}
	}
	final := second
	if final == nil {
		final = first
	}
	final.FrameHash = frameHash
	final.Interrupted = interrupted
	return final, nil
}

func checkLimits(opt Options) error {
	if opt.Frames == 0 || opt.Frames > ingest.MaxFrames {
		return media.NewError(media.ClassConfig, "demo", "frame count out of range", 0, false)
	}
	if opt.Frames%opt.FPS != 0 {
		return media.NewError(media.ClassConfig, "demo", "frames must cover whole seconds", 0, false)
	}
	if len(opt.Targets) == 0 || len(opt.Targets) > 3 {
		return media.NewError(media.ClassConfig, "demo", "targets must be 1..3 representations", 0, false)
	}
	allowed := map[int]bool{180: true, 450: true, 900: true}
	for _, t := range opt.Targets {
		if !allowed[t] {
			return media.NewError(media.ClassConfig, "demo", fmt.Sprintf("unsupported target %dk", t), 0, false)
		}
	}
	return nil
}

// generateContainer builds deterministic media, injects the fault profile
// and writes the binary container next to the output directory.
func generateContainer(opt Options) (string, [32]byte, error) {
	var frameHash [32]byte
	frames := fixture.GenerateFrames(opt.Frames, opt.Seed)
	audioPackets := (opt.Frames / opt.FPS) * media.AudioPacketsPerSecond
	audio := fixture.GenerateAudio(audioPackets, opt.Seed)

	canonical := fixture.BuildPackets(frames, audio)
	frameHash = sha256.Sum256(framesPacked(frames))

	delivered := canonical
	if opt.Faults == string(fixture.FaultMixed) {
		delivered, _ = fixture.InjectFaults(canonical, opt.Frames, opt.Seed)
	}
	srcDir := filepath.Join(opt.OutDir, "source")
	if err := mkdirAll(srcDir); err != nil {
		return "", frameHash, err
	}
	container := filepath.Join(srcDir, "asset.avp")
	hdr := ingest.Header{
		Frames: opt.Frames, FPS: opt.FPS, Width: fixture.Width, Height: fixture.Height,
		AudioRate: media.AudioRate, Packets: uint32(len(delivered)),
	}
	if err := ingest.WriteContainer(container, hdr, delivered); err != nil {
		return "", frameHash, err
	}
	return container, frameHash, nil
}

func framesPacked(frames []fixture.FrameSet) []byte {
	var buf []byte
	for i := range frames {
		buf = append(buf, fixture.FrameContent(&frames[i].Frame)...)
	}
	return buf
}

// Validate runs the acceptance assertions over the final report.
func Validate(rep *pipeline.Report, opt Options) []error {
	var failures []error
	if rep.MeanPSNR() < 28 {
		failures = append(failures, fmt.Errorf("psnr_y_db %.2f below 28 dB", rep.MeanPSNR()))
	}
	if rep.MaxAlignMS() > 0.04 {
		failures = append(failures, fmt.Errorf("align_max_ms %.3f exceeds 0.04 ms", rep.MaxAlignMS()))
	}
	if rep.MaxDriftMS() > 20 {
		failures = append(failures, fmt.Errorf("av_drift_ms %.2f exceeds 20 ms", rep.MaxDriftMS()))
	}
	for _, s := range rep.Segments {
		if !s.BitrateOK {
			failures = append(failures, fmt.Errorf("segment g%d v%dk delta %.1f%% outside ±20%%",
				s.Group, s.TargetKbps, s.DeltaPct))
		}
	}
	if opt.Resume && rep.ResumeSegments < opt.InterruptAfter {
		failures = append(failures, fmt.Errorf("resume_segments %d below interrupt-after %d",
			rep.ResumeSegments, opt.InterruptAfter))
	}
	// FEC must have recovered at least the single-loss and corrupt packets.
	if opt.Faults == string(fixture.FaultMixed) && rep.RecoveredPackets < 2 {
		failures = append(failures, fmt.Errorf("recovered_packets %d below expected 2", rep.RecoveredPackets))
	}
	_ = transport.BlockSize
	return failures
}
