// Package pipeline wires the streaming EBML parser, shared clock,
// watermark timeline, bounded queue, manifest aligner and checkpoint
// store into one resumable run with structured metrics and exit codes.
package pipeline

import (
	"context"
	"time"

	"mediapack/internal/clock"
	"mediapack/internal/manifest"
	"mediapack/internal/stream"
	"mediapack/internal/timeline"
)

// Config configures a full pipeline run.
type Config struct {
	QueueBytes    int64
	WatermarkMS   int64
	GridMS        int64
	ToleranceMS   int64
	MaxElement    int64
	MaxBlock      int64
	WindowMS      int64
	// InterruptAfterSegments simulates a crash after N committed
	// segments (0 means run to completion).
	InterruptAfterSegments int
}

// DefaultConfig returns sane demo defaults.
func DefaultConfig() Config {
	return Config{
		QueueBytes: 1 << 20, WatermarkMS: 500, GridMS: 500,
		ToleranceMS: 25, MaxElement: 64 << 20, MaxBlock: 4 << 20,
		WindowMS: 10_000,
	}
}

// Metrics are the acceptance-relevant numbers printed by the CLI.
type Metrics struct {
	Segments                int    `json:"segments"`
	Representations         int    `json:"representations"`
	PeakBufferBytes         int64  `json:"peak_buffer_bytes"`
	QueueLimitBytes         int64  `json:"queue_limit_bytes"`
	CorruptOffsetErrorBytes int64  `json:"corrupt_offset_error_bytes"`
	ResumeMS                int64  `json:"resume_ms"`
	ManifestSHA256          string `json:"manifest_sha256"`
	Blocks                  int    `json:"blocks"`
	Clusters                int    `json:"clusters"`
	LateEvents              int    `json:"duplicates,omitempty"`
	Duplicates              int    `json:"-"`
	ManifestErrors          int    `json:"manifest_errors"`
	Resumed                 bool   `json:"resumed"`
}

// Run executes the pipeline over data, writing outputs through out and
// recording checkpoints via cp. When resumeFrom is non-nil the run
// validates it against the current source before reusing its offsets.
func Run(ctx context.Context, data []byte, cfg Config, out *Outputs,
	cp Checkpointer, resumeFrom *ResumeState) (*Metrics, error) {
	start := time.Now()
	if err := ctx.Err(); err != nil {
		return nil, clock.NewError(clock.CatCancelled, "%v", err)
	}
	fp := manifest.FingerprintBytes(data)

	// Resume validation: refuse to continue on any source/output drift.
	if resumeFrom != nil {
		if err := cp.Verify(resumeFrom.toCheckpoint(), fp, resumeFrom.ManifestSHA); err != nil {
			return nil, err
		}
	}

	tb := &clock.Timebase{ScaleNS: 1_000_000, ToleranceNS: cfg.ToleranceMS * 1_000_000}
	engine := timeline.NewEngine(timeline.Config{
		Timebase: tb,
		LeadNS:   cfg.WatermarkMS * 1_000_000,
		WindowNS: cfg.WindowMS * 1_000_000,
	})
	queue := stream.NewBoundedQueue(cfg.QueueBytes)
	defer queue.Close()

	runner := &runner{
		cfg: cfg, tb: tb, engine: engine, queue: queue,
		out: out, cp: cp, fp: fp,
		wrap: newWrapTracker(),
	}
	if resumeFrom != nil {
		runner.resumed = true
		runner.resumeOffset = resumeFrom.ParseOffset
	}

	summary, err := runner.feed(ctx, data)
	runner.metrics.Clusters = summary.Clusters
	runner.metrics.Blocks = summary.Blocks
	if err != nil {
		// Preserve the precise corrupt offset for the metrics payload.
		if me, ok := clock.AsMediaError(err); ok && me.Category == clock.CatTruncated {
			runner.metrics.CorruptOffsetErrorBytes = corruptGap(me.Offset, int64(len(data)))
		}
		// A simulated/real interruption still checkpoints the boundary.
		_ = runner.checkpoint(summary.EndOffset)
		return runner.finalize(start, err)
	}

	// Flush any in-flight events, then build and persist the manifest.
	engine.Flush()
	if err := runner.buildManifest(); err != nil {
		return runner.finalize(start, err)
	}
	if err := runner.checkpoint(summary.EndOffset); err != nil {
		return runner.finalize(start, err)
	}
	return runner.finalize(start, nil)
}

// corruptGap is the absolute distance between the parser's truncation
// anchor and the physical end of the input (the actual cut point).
func corruptGap(anchor, physicalEOF int64) int64 {
	d := anchor - physicalEOF
	if d < 0 {
		d = -d
	}
	return d
}
