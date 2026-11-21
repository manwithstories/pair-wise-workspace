package main

import (
	"errors"
	"fmt"

	"github.com/archive/rtprestore/internal/checkpoint"
	"github.com/archive/rtprestore/internal/pipeline"
	"github.com/archive/rtprestore/internal/synth"
	"github.com/archive/rtprestore/internal/verify"
)

// typedError carries the CLI exit code alongside the underlying failure.
type typedError struct {
	code int
	err  error
}

func (e *typedError) Error() string { return e.err.Error() }
func (e *typedError) Unwrap() error { return e.err }

func mapError(err error) int {
	var te *typedError
	if errors.As(err, &te) {
		return te.code
	}
	switch {
	case errors.Is(err, verify.ErrUnreadable), errors.Is(err, checkpoint.ErrUnreadable):
		return exitBadArgs
	case errors.Is(err, checkpoint.ErrParamsChanged), errors.Is(err, checkpoint.ErrCheckpointCorrupt):
		return exitUnsafeCheckpoint
	default:
		return exitBadArgs
	}
}

// runDemo generates the degraded capture, restores it and prints the
// acceptance metrics, then independently verifies the on-disk shards.
func runDemo(args []string) error {
	d, err := parseDemo(args)
	if err != nil {
		return &typedError{exitBadArgs, err}
	}
	cfg := synth.Config{
		Seed: d.seed, VideoPackets: d.videoPackets, AudioPackets: d.audioPackets,
		LossPercent: d.loss, ReorderPercent: d.reorder, DuplicatePct: d.duplicate,
		WrapAt: d.wrapAt, DriftPPM: d.driftPPM, VFR: d.vfr,
	}
	recs, truth := synth.NewBuilder(cfg).Build()
	params := checkpoint.Params{
		Seed: d.seed, LossPercent: d.loss, ReorderPercent: d.reorder,
		WrapAt: d.wrapAt, DriftPPM: d.driftPPM, VFR: d.vfr,
		CheckpointEvery: d.checkpointEvery,
	}
	runCfg := pipeline.Config{
		Params: params, WrapAt: d.wrapAt, OutDir: d.out,
		CheckpointEvery: d.checkpointEvery, BlockSize: d.checkpointEvery,
	}
	m, _, err := pipeline.Run(recs, truth, runCfg, d.resume)
	if err != nil {
		return &typedError{exitUnsafeCheckpoint, err}
	}
	if err := checkpoint.SaveMetrics(d.out, checkpoint.MetricsSnapshot{
		RoundTripRate: m.RoundTripRate, KeyframeHitRate: m.KeyframeHitRate,
	}); err != nil {
		return &typedError{exitUnsafeCheckpoint, err}
	}
	// Independent audit of the freshly written artifacts.
	rep, err := verify.Run(d.out, params, verify.Inputs{
		RoundTripRate: m.RoundTripRate, KeyframeHitRate: m.KeyframeHitRate,
		BlockSize: d.checkpointEvery,
	})
	if err != nil {
		return &typedError{exitUnsafeCheckpoint, err}
	}
	fmt.Printf("roundtrip_rate=%.2fx\n", m.RoundTripRate)
	fmt.Printf("keyframe_hit_rate=%.3f\n", m.KeyframeHitRate)
	fmt.Printf("segments=%d\n", rep.Segments)
	fmt.Printf("segment_sha256_count=%d\n", rep.SegmentSHA256Count)
	fmt.Printf("resume_reused=%d\n", rep.ResumeReused)
	fmt.Printf("corrupt_rejected=%d\n", rep.CorruptRejected)
	fmt.Printf("silence_frames=%d clip_frames=%d preserved_boundary=%d\n",
		m.SilenceFrames, m.ClipFrames, m.PreservedBoundary)
	fmt.Printf("uncertain_intervals=%d gaps=%d\n", len(m.UncertainIntervals), len(m.Gaps))
	fmt.Printf("exit_code=%d\n", exitOK)
	return nil
}

// runVerify audits an existing output directory, optionally injecting a torn
// tail or comparing against a second run's per-segment hashes.
func runVerify(args []string) error {
	v, err := parseVerify(args)
	if err != nil {
		return &typedError{exitBadArgs, err}
	}
	params, err := checkpoint.LoadParams(v.out)
	if err != nil {
		return &typedError{mapError(err), err}
	}
	if v.inject == "torn-tail" {
		if err := verify.InjectTornTail(v.out); err != nil {
			return &typedError{exitDataCorruption, err}
		}
	}
	rep, err := verify.Run(v.out, params, verify.Inputs{
		BlockSize: params.CheckpointEvery,
	})
	if err != nil {
		return &typedError{mapError(err), err}
	}
	// Pull the run's rates from its metrics document when available; the
	// integrity verdict itself derives solely from disk evidence.
	rep.RoundTripRate, rep.KeyframeHitRate = loadRates(v.out)
	fmt.Printf("roundtrip_rate=%.2fx\n", rep.RoundTripRate)
	fmt.Printf("keyframe_hit_rate=%.3f\n", rep.KeyframeHitRate)
	fmt.Printf("segments=%d\n", rep.Segments)
	fmt.Printf("segment_sha256_count=%d\n", rep.SegmentSHA256Count)
	fmt.Printf("resume_reused=%d\n", rep.ResumeReused)
	fmt.Printf("corrupt_rejected=%d\n", rep.CorruptRejected)
	fmt.Printf("bad_segments=%d\n", rep.BadSegments)
	fmt.Printf("first_error_tick=%d\n", rep.FirstErrorTick)
	if v.compare != "" {
		if err := compareRuns(v.out, v.compare); err != nil {
			return &typedError{exitDataCorruption, err}
		}
		fmt.Println("cross_run_sha256=identical")
	}
	code := exitOK
	if rep.BadSegments > 0 {
		code = exitDataCorruption
	}
	fmt.Printf("exit_code=%d\n", code)
	if code != exitOK {
		return &typedError{code, fmt.Errorf("%d corrupt segment(s) located", rep.BadSegments)}
	}
	return nil
}
