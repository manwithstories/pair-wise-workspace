// Package mediad is the reusable library for inspecting multi-camera
// CMAF/fMP4 captures. It discovers fragments, strictly parses MP4 boxes,
// repairs timelines, aligns representations at common keyframes, enforces
// metadata compatibility, merges deterministically and writes/verifies a
// deliverable manifest with resumable checkpoints.
//
// The cmd/mediad binary is only a thin demo/verification front-end over this
// library. The library never transcodes and enforces the 128 MiB per-fragment
// ceiling at discovery time.
package mediad

import (
	"context"

	"mediad/internal/core"
	"mediad/internal/manifest"
	"mediad/internal/pipeline"
)

// Config controls the bounded worker pool.
type Config = pipeline.Config

// Report is the outcome of an inspection or resume.
type Report = pipeline.Report

// Manifest is the deliverable checklist type.
type Manifest = manifest.Manifest

// VerifyReport contains artifact hash/roundtrip verification results.
type VerifyReport = manifest.VerifyReport

// Checkpoint is the resumable state.
type Checkpoint = pipeline.Checkpoint

// Process exit codes mandated by the terminal contract.
const (
	ExitComplete    = core.ExitComplete
	ExitFatal       = core.ExitFatal
	ExitCanceled    = core.ExitCanceled
	ExitQuarantined = core.ExitQuarantined
)

// DefaultConfig returns 4 workers and an 8-slot queue.
func DefaultConfig() Config { return pipeline.DefaultConfig() }

// Inspect runs the full pipeline over mediaRoot. When manifestPath is
// non-empty the manifest is atomically written there and verified against
// artifact paths relative to mediaRoot.
func Inspect(ctx context.Context, mediaRoot, manifestPath string, cfg Config) (*Report, error) {
	return pipeline.Execute(ctx, pipeline.ExecuteParams{
		MediaRoot:    mediaRoot,
		ManifestPath: manifestPath,
		Config:       cfg,
	})
}

// InspectWithCheckpoint is Inspect with a durable checkpoint and an optional
// deterministic cancel gate (cancelAfterCommits > 0 is used by demos/tests).
func InspectWithCheckpoint(ctx context.Context, mediaRoot, checkpointPath, manifestPath string,
	cfg Config, expectedHash string, cancelAfterCommits int) (*Report, error) {
	return pipeline.Execute(ctx, pipeline.ExecuteParams{
		MediaRoot: mediaRoot, CheckpointPath: checkpointPath, ManifestPath: manifestPath,
		Config: cfg, ExpectedHash: expectedHash, CancelAfterCommits: cancelAfterCommits,
	})
}

// Resume continues a run whose checkpoint file lives in checkpointDir.
func Resume(ctx context.Context, checkpointDir string) (*Report, error) {
	return pipeline.ResumeDir(ctx, checkpointDir)
}

// DryRun computes the deterministic result hash without writing outputs.
func DryRun(ctx context.Context, mediaRoot string, cfg Config) (*Report, error) {
	return pipeline.DryRun(ctx, mediaRoot, "", "", cfg)
}

// ExitCode maps an error to the contracted process exit code.
func ExitCode(err error) int { return core.ExitCode(err) }
