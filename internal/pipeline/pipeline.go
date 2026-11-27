// Package pipeline wires every subsystem into one resumable offline job:
// probe -> shared cut plan -> per-rendition encode -> hash-verified journal
// commit -> captions -> manifests -> audit. It streams segment by segment so
// resident memory stays bounded regardless of asset length, and it injects
// interruptions on demand to prove the recovery contract.
package pipeline

import (
	"context"
	"fmt"
	"strings"

	"avcore/internal/adapter"
	"avcore/internal/audit"
	"avcore/internal/caption"
	"avcore/internal/clock"
	"avcore/internal/manifest"
	"avcore/internal/segment"
	"avcore/internal/state"
)

// RenditionConfig is one target bitrate ladder rung.
type RenditionConfig struct {
	Name      string
	TargetBPS int
	Width     int
	Height    int
}

// Config fully describes a job.
type Config struct {
	Source       string
	JobDir       string
	SegmentTicks int64
	Renditions   []RenditionConfig
	FrameRate    clock.Rate
	// InterruptAfterCuts stops the run with exit semantics after this many
	// cuts have been fully committed (0 disables injection).
	InterruptAfterCuts int
}

// Engine holds the adapters and builders shared by a run.
type Engine struct {
	Probe   adapter.ProbeAdapter
	Encode  adapter.EncodeAdapter
	FFmpeg  *adapter.FFmpegV1
	Builder *manifest.Builder
}

// NewEngine constructs an engine from located local binaries.
func NewEngine() (*Engine, error) {
	probe, err := adapter.NewFFProbeV1("")
	if err != nil {
		return nil, err
	}
	enc, err := adapter.NewFFmpegV1("")
	if err != nil {
		return nil, err
	}
	return &Engine{
		Probe:   probe,
		Encode:  enc,
		FFmpeg:  enc,
		Builder: manifest.NewBuilder(),
	}, nil
}

// Result is what a completed (or recovered) run hands to the CLI.
type Result struct {
	Summary     audit.Summary
	Journal     *state.Journal
	ManifestSHA string
	// Interrupted is true when an injected stop ended the run early.
	Interrupted bool
}

// fingerprint hashes source content plus the normalized config and adapter.
func (e *Engine) fingerprint(cfg Config) (state.Fingerprint, error) {
	inputHash, err := state.FileSHA256(cfg.Source)
	if err != nil {
		return state.Fingerprint{}, adapter.NewError(adapter.CodePathRejected,
			"cannot hash source: "+err.Error(), adapter.Source{File: cfg.Source})
	}
	var b strings.Builder
	for _, r := range cfg.Renditions {
		fmt.Fprintf(&b, "%s|%d|%dx%d;", r.Name, r.TargetBPS, r.Width, r.Height)
	}
	fmt.Fprintf(&b, "seg=%d;rate=%d/%d", cfg.SegmentTicks, cfg.FrameRate.Num, cfg.FrameRate.Den)
	return state.Fingerprint{
		InputHash:      inputHash,
		ConfigHash:     state.HashBytes([]byte(b.String())),
		AdapterVersion: e.Probe.AdapterVersion(),
	}, nil
}

// Run executes the full job, resuming from any verified checkpoint.
func (e *Engine) Run(ctx context.Context, cfg Config, caps caption.Result, logger *audit.Logger) (*Result, error) {
	if len(cfg.Renditions) < 2 {
		return nil, adapter.NewError(adapter.CodeBadArgument,
			"at least two bitrate renditions are required", adapter.Source{})
	}
	info, err := e.Probe.Inspect(ctx, cfg.Source)
	if err != nil {
		return nil, err
	}
	kfs, err := e.Probe.Keyframes(ctx, cfg.Source)
	if err != nil {
		return nil, err
	}
	plan, err := segment.Build(kfs, info.DurationTicks, segment.Options{
		TargetTicks: cfg.SegmentTicks,
	})
	if err != nil {
		return nil, err
	}

	fp, err := e.fingerprint(cfg)
	if err != nil {
		return nil, err
	}
	journal, err := state.Open(cfg.JobDir, fp)
	if err != nil {
		return nil, err
	}
	reused, faults := journal.Verify()
	for _, f := range faults {
		_ = logger.Emit(audit.Record{"event": "checkpoint_fault", "code": string(f.Code), "file": f.Source.File})
	}

	if err := e.encodeRenditions(ctx, cfg, plan, journal, logger); err != nil {
		return nil, err
	}

	// Interruption injection: stop after the configured number of committed
	// cuts, before any manifest is written.
	if cfg.InterruptAfterCuts > 0 && countCommittedCuts(journal) >= cfg.InterruptAfterCuts {
		_ = logger.Emit(audit.Record{"event": "interrupt_injected", "after_cuts": cfg.InterruptAfterCuts})
		return &Result{Journal: journal, Interrupted: true}, adapter.NewError(
			adapter.CodeInterrupted, "injected interruption", adapter.Source{})
	}

	if err := e.writeCaptions(cfg, plan, caps, journal, logger); err != nil {
		return nil, err
	}
	sha, err := e.writeManifests(cfg, plan, journal)
	if err != nil {
		return nil, err
	}
	sum := e.buildSummary(cfg, plan, journal, caps, sha, reused)
	return &Result{Summary: sum, Journal: journal, ManifestSHA: sha}, nil
}

// encodeRenditions processes one cut at a time across every rendition so that
// a long asset never holds more than one segment's data per rung in flight.
func (e *Engine) encodeRenditions(ctx context.Context, cfg Config, plan *segment.Plan,
	journal *state.Journal, logger *audit.Logger) error {
	for vi, rc := range cfg.Renditions {
		for _, cut := range plan.Cuts {
			if _, ok := journal.Lookup(vi, cut.Index); ok {
				_ = logger.Emit(audit.Record{"event": "segment_reused", "variant": vi, "cut": cut.Index})
				continue
			}
			name := fmt.Sprintf("v%d_%03d.m4s", vi, cut.Index)
			req := adapter.EncodeRequest{
				Input: cfg.Source, StartTicks: cut.StartTicks,
				DurationTicks: cut.EndTicks - cut.StartTicks, TargetBPS: rc.TargetBPS,
				Width: rc.Width, Height: rc.Height, FrameRate: cfg.FrameRate,
				Output: journal.FinalPath(name),
			}
			// Encode into the journal staging path.
			req.Output = journal.TempPath(name)
			res, err := e.Encode.Encode(ctx, req)
			if err != nil {
				return err
			}
			m, err := e.FFmpeg.Measure(ctx, cfg.Source, adapter.EncodeRequest{
				Input: cfg.Source, StartTicks: cut.StartTicks,
				DurationTicks: cut.EndTicks - cut.StartTicks,
				Width: rc.Width, Height: rc.Height, Output: journal.TempPath(name),
			})
			if err != nil {
				return err
			}
			rec := state.SegmentRecord{
				ID: name, Variant: vi, CutIndex: cut.Index, Path: name,
				StartTicks: cut.StartTicks, EndTicks: cut.EndTicks,
				TargetBPS: rc.TargetBPS, ActualBPS: res.ActualBPS,
				PSNRMeanDB: m.PSNRMeanDB, SSIMMean: m.SSIMMean,
			}
			if _, err := journal.Commit(rec, ""); err != nil {
				return err
			}
			_ = logger.Emit(audit.Record{
				"event": "segment_committed", "variant": vi, "cut": cut.Index,
				"target_bps": rc.TargetBPS, "actual_bps": res.ActualBPS,
				"psnr": m.PSNRMeanDB, "ssim": m.SSIMMean,
			})
		}
	}
	return nil
}

func countCommittedCuts(j *state.Journal) int {
	seen := map[int]bool{}
	for _, s := range j.Segments {
		seen[s.CutIndex] = true
	}
	return len(seen)
}
