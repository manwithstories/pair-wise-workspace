package stream

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"time"

	"mediaflow/internal/clock"
	"mediaflow/internal/remux"
	"mediaflow/internal/timeline"
)

// RunConfig controls a streaming run.
type RunConfig struct {
	OutputName string
	// MaxFramesPerSegment bounds work between checkpoints.
	MaxFramesPerSegment int
}

// DefaultRunConfig returns bounded demo settings.
func DefaultRunConfig() RunConfig {
	return RunConfig{OutputName: "stream.bin", MaxFramesPerSegment: 64}
}

// RunStats captures throughput and latency metrics for one run.
type RunStats struct {
	Segments        int
	FramesWritten   int
	ResumedFrom     int    // segment index resumed from (0 = fresh)
	ResumeUsed      bool   // whether a valid checkpoint was consumed
	PerSegmentUS    []int64
	P95US           int64
	OutputSHA       string
	Bytes           int64
	SegmentChecksum []string
}

// Runner incrementally writes normalized frames through the remux writer,
// checkpointing after every complete segment.
type Runner struct {
	writer       *remux.Writer
	checkpointer *Checkpointer
	cfg          RunConfig
}

// NewRunner wires a remux writer and checkpoint store together.
func NewRunner(w *remux.Writer, cp *Checkpointer, cfg RunConfig) *Runner {
	if cfg.MaxFramesPerSegment <= 0 {
		cfg = DefaultRunConfig()
	}
	return &Runner{writer: w, checkpointer: cp, cfg: cfg}
}

// groupSegments splits frames into bounded segments at frame boundaries so
// every checkpoint corresponds to a complete, independently checksummed unit.
func groupSegments(frames []clock.Normalized, maxPer int) []Segment {
	var segs []Segment
	for start := 0; start < len(frames); start += maxPer {
		end := start + maxPer
		if end > len(frames) {
			end = len(frames)
		}
		segs = append(segs, Segment{Index: len(segs), Frames: frames[start:end]})
	}
	return segs
}

// Run executes (or resumes) the stream. On ctx cancellation it returns an
// ErrCanceled structured error after persisting the latest checkpoint.
func (r *Runner) Run(ctx context.Context, frames []clock.Normalized, inputSHA string) (RunStats, error) {
	segments := groupSegments(frames, r.cfg.MaxFramesPerSegment)
	encodedSeg := make([][]byte, len(segments))
	actualSHA := make([]string, len(segments))
	for i, seg := range segments {
		b, _ := remux.EncodeFrames(seg.Frames)
		encodedSeg[i] = b
		actualSHA[i] = hashSegment(b)
	}

	cp, err := r.checkpointer.Load()
	if err != nil {
		return RunStats{}, err
	}
	startSeg := 0
	resumed := false
	if cp.InputSHA != "" {
		if err := VerifyResume(cp, inputSHA, actualSHA); err != nil {
			// Stale checkpoint: restart from scratch.
			cp = Checkpoint{InputSHA: inputSHA, OutputName: r.cfg.OutputName}
		} else {
			startSeg = cp.NextSeg
			resumed = true
		}
	} else {
		cp = Checkpoint{InputSHA: inputSHA, OutputName: r.cfg.OutputName}
	}

	stats := RunStats{
		Segments:        len(segments),
		ResumedFrom:     startSeg,
		ResumeUsed:      resumed,
		SegmentChecksum: actualSHA,
	}
	// Concatenate encoded segments into one deterministic output body.
	var body []byte
	for i := 0; i < startSeg; i++ {
		body = append(body, encodedSeg[i]...)
		stats.FramesWritten += len(segments[i].Frames)
	}

	for i := startSeg; i < len(segments); i++ {
		if err := ctx.Err(); err != nil {
			_ = r.checkpointer.Save(cp)
			return stats, timeline.GlobalError("stream.run", true,
				fmt.Errorf("%w: %v", timeline.ErrCanceled, err))
		}
		t0 := time.Now()
		body = append(body, encodedSeg[i]...)
		stats.FramesWritten += len(segments[i].Frames)
		elapsed := time.Since(t0).Microseconds()
		stats.PerSegmentUS = append(stats.PerSegmentUS, elapsed)

		// Advance checkpoint only after the segment bytes are fully appended.
		cp.NextSeg = i + 1
		cp.SegSHA = append(cp.SegSHA, actualSHA[i])
		cp.WrittenUS = segments[i].Frames[len(segments[i].Frames)-1].StartUS
		if err := r.checkpointer.Save(cp); err != nil {
			return stats, err
		}
	}

	res, err := r.writer.WriteAtomically(r.cfg.OutputName, bytes.NewReader(body))
	if err != nil {
		return stats, err
	}
	stats.OutputSHA = res.SHA256
	stats.Bytes = res.Bytes
	stats.P95US = percentile(stats.PerSegmentUS, 95)
	if err := r.checkpointer.Remove(); err != nil {
		return stats, err
	}
	return stats, nil
}

// percentile returns the p-th percentile (0-100) using nearest-rank on a copy.
func percentile(values []int64, p int) int64 {
	if len(values) == 0 {
		return 0
	}
	cp := append([]int64(nil), values...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	rank := (p * len(cp) + 99) / 100 // nearest-rank, ceil
	if rank < 1 {
		rank = 1
	}
	if rank > len(cp) {
		rank = len(cp)
	}
	return cp[rank-1]
}
