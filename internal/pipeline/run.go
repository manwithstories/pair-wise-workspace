// Package pipeline wires probe, segment, index and rtp into one batch
// scheduler with cancellation, crash-safe checkpoint resume and idempotent
// re-runs. Already committed chunks are never counted twice.
package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"

	"mediacheck/internal/index"
	"mediacheck/internal/model"
	"mediacheck/internal/rtp"
	"mediacheck/internal/segment"
)

// Input describes one representation file to process.
type Input struct {
	ID             string
	Representation string
	Reader         io.ReaderAt
	Size           int64
	// Damage labels select which verification paths the demo exercises.
	Truncated bool
}

// Config controls a batch run.
type Config struct {
	SegmentSeconds  int
	WindowSeconds   int
	MaxDriftMillis  int64
	CheckpointEvery int
	// CancelAfterSamples, when > 0, cancels the context mid-batch to prove
	// resume; production runs leave it zero.
	CancelAfterSamples int
}

// Metrics holds the acceptance-facing measurements.
type Metrics struct {
	AVSyncMaxMillis       int64
	PacketRecoveryPct     float64
	P95SegmentLatencyMs   int64
	Segments              int
	AlignedRepresentations int
	CheckpointResumes     int
	QuarantinedRanges     int
	ManifestSHA256        string
}

// Runner executes one batch over a local index store.
type Runner struct {
	store   *index.Store
	planner *segment.Planner
	cfg     Config
}

// NewRunner validates configuration and opens the work directory.
func NewRunner(workDir string, cfg Config) (*Runner, error) {
	if cfg.SegmentSeconds <= 0 {
		return nil, fmt.Errorf("pipeline: segment duration must be positive")
	}
	if cfg.CheckpointEvery <= 0 {
		cfg.CheckpointEvery = index.DefaultCheckpointEvery
	}
	if cfg.WindowSeconds <= 0 {
		cfg.WindowSeconds = cfg.SegmentSeconds / 2
		if cfg.WindowSeconds <= 0 {
			cfg.WindowSeconds = 1
		}
	}
	store, err := index.NewStore(workDir)
	if err != nil {
		return nil, err
	}
	pcfg := segment.PlanConfig{
		TargetTicks:  int64(cfg.SegmentSeconds) * 90000,
		WindowTicks:  int64(cfg.WindowSeconds) * 90000,
		MaxDriftTicks: cfg.MaxDriftMillis * 90,
	}
	planner, err := segment.NewPlanner(pcfg)
	if err != nil {
		return nil, err
	}
	return &Runner{store: store, planner: planner, cfg: cfg}, nil
}

// inputDigest fingerprints the batch inputs so a checkpoint cannot be
// resumed against different media. Readers are consumed via bounded reads.
func (r *Runner) inputDigest(inputs []Input) (string, error) {
	h := sha256.New()
	ids := make([]string, 0, len(inputs))
	for _, in := range inputs {
		ids = append(ids, in.ID)
	}
	sort.Strings(ids)
	for _, id := range ids {
		h.Write([]byte(id))
	}
	for _, in := range inputs {
		fmt.Fprintf(h, ":%d", in.Size)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Run executes the full batch and returns acceptance metrics. It is safe to
// call again after interruption: the checkpoint cursor skips committed work.
func (r *Runner) Run(ctx context.Context, inputs []Input, rtpStats *rtp.Stats) (*Metrics, *segment.Manifest, error) {
	if len(inputs) == 0 {
		return nil, nil, fmt.Errorf("pipeline: no inputs")
	}
	digest, err := r.inputDigest(inputs)
	if err != nil {
		return nil, nil, err
	}

	chunker, resumes, err := r.openChunker(ctx, digest)
	if err != nil {
		return nil, nil, err
	}

	results, err := r.probeAll(ctx, inputs)
	if err != nil {
		return nil, nil, err
	}

	aligned, alignErrs, avSync, err := r.planAll(ctx, results)
	if err != nil {
		return nil, nil, err
	}

	quarantined, segLatency, err := r.processSegments(ctx, results, aligned, chunker)
	if err != nil {
		return nil, nil, err
	}

	manifest, err := r.writeManifest(digest, aligned, alignErrs, quarantined, rtpStats)
	if err != nil {
		return nil, nil, model.NewError(model.CodeIndexWriteFailed, "index", 0, 0, err.Error())
	}

	m := &Metrics{
		AVSyncMaxMillis:        avSync,
		Segments:               len(manifest.Segments),
		AlignedRepresentations: len(aligned.Representations),
		CheckpointResumes:      resumes,
		QuarantinedRanges:      len(manifest.QuarantinedRanges),
		P95SegmentLatencyMs:    percentile95(segLatency),
		ManifestSHA256:         manifestDigest(manifest),
	}
	if rtpStats != nil {
		m.PacketRecoveryPct = rtpStats.RecoveryPercent()
	}
	return m, manifest, nil
}

// openChunker resumes from a validated checkpoint or starts fresh.
func (r *Runner) openChunker(ctx context.Context, digest string) (*index.Chunker, int, error) {
	cp, err := index.LoadCheckpoint(r.store, digest)
	if err == nil {
		return index.Resume(*cp), 1, nil
	}
	if err != index.ErrNotFound {
		// Checksum/digest mismatch: refuse resume (exit code 4 upstream).
		return nil, 0, fmt.Errorf("%w: %v", model.NewError(model.CodeCheckpointMismatch, "checkpoint", 0, 0, err.Error()), err)
	}
	return index.NewChunker(digest, r.cfg.CheckpointEvery), 0, nil
}
