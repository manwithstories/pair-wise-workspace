package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/relayteam/relay/internal/align"
	"github.com/relayteam/relay/internal/checkpoint"
	"github.com/relayteam/relay/internal/fixture"
	"github.com/relayteam/relay/internal/media"
	"github.com/relayteam/relay/internal/segment"
	"github.com/relayteam/relay/internal/stream"
	"github.com/relayteam/relay/internal/timeline"
	"github.com/relayteam/relay/internal/verify"
)

// RunConfig controls one resumable relay processing pass.
type RunConfig struct {
	Root                 string
	InputPath            string
	ChunkRecords         int
	MaxInflightBytes     int64
	SlowEvery            int
	SlowDelay            time.Duration
	InterruptAfterChunks int
}

// RunResult contains terminal metrics and the repeatable output digest.
type RunResult struct {
	Records               int    `json:"records"`
	FirstBadRecord        int    `json:"first_bad_record"`
	PrecisionTicks        int64  `json:"precision_ticks"`
	BackpressurePeakBytes int64  `json:"backpressure_peak_bytes"`
	RestoreMS             int64  `json:"restore_ms"`
	SHA256                string `json:"sha256"`
	Chunks                int    `json:"chunks"`
}

// Run executes read/validate/cut/align -> bounded stream -> checkpoint/manifest.
func Run(ctx context.Context, cfg RunConfig) (*RunResult, error) {
	if cfg.ChunkRecords <= 0 || cfg.MaxInflightBytes <= 0 || cfg.Root == "" || cfg.InputPath == "" {
		return nil, media.E(media.ErrArgument, "app.run", media.Meta{},
			errors.New("root, input, positive chunk-records and max-inflight-bytes are required"))
	}
	if cfg.SlowDelay == 0 {
		cfg.SlowDelay = 2 * time.Millisecond
	}
	inputFP, err := verify.SHA256File(cfg.InputPath)
	if err != nil {
		return nil, media.E(media.ErrCheckpoint, "app.run", media.Meta{}, err)
	}
	cpCfg := checkpoint.Config{
		InputName: filepath.Base(cfg.InputPath), ChunkRecords: cfg.ChunkRecords,
		MaxInflightBytes: cfg.MaxInflightBytes, SegmentTicks90k: fixture.SegmentTicks,
		SlowEvery: cfg.SlowEvery, PayloadBytes: 4096,
		RenditionCount: len(fixture.Renditions), TrackCount: len(fixture.TrackNames),
	}
	mgr, restoreMS, err := checkpoint.Restore(cfg.Root, inputFP, cpCfg)
	if err != nil {
		return &RunResult{RestoreMS: restoreMS}, err
	}

	cutter := segment.New(fixture.SegmentTicks)
	aligner := align.NewAligner(fixture.SegmentTicks)
	validator := timeline.NewValidator()
	if err := replayCommitted(mgr, cutter, aligner, validator); err != nil {
		return &RunResult{RestoreMS: restoreMS}, err
	}
	initialChunks := len(mgr.Index.Chunks)

	q, err := stream.NewQueue[timeline.Record](1024, cfg.MaxInflightBytes)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	var producerErr error
	recordsSeen := 0
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer q.Close()
		recordsSeen, producerErr = produce(runCtx, cfg, mgr, q, validator, cutter, aligner)
		if producerErr != nil {
			cancel()
		}
	}()

	consumerErr, processed := consume(runCtx, cfg, mgr, q, initialChunks)
	cancel()
	wg.Wait()
	stats := q.Stats()
	res := &RunResult{
		Records: recordsSeen, BackpressurePeakBytes: stats.PeakBytes,
		RestoreMS: restoreMS, Chunks: len(mgr.Index.Chunks),
	}
	if consumerErr != nil && !errors.Is(consumerErr, stream.ErrClosed) {
		return res, consumerErr
	}
	if producerErr != nil && !errors.Is(producerErr, context.Canceled) {
		return res, producerErr
	}
	_ = processed

	report, err := aligner.Report()
	if err != nil {
		return res, err
	}
	res.PrecisionTicks = report.PrecisionTicks
	digest, err := BuildOutputs(cfg.Root, mgr, report)
	if err != nil {
		return res, err
	}
	res.SHA256 = digest
	return res, nil
}

func replayCommitted(mgr *checkpoint.Manager, cutter *segment.Cutter, aligner *align.Aligner, validator *timeline.Validator) error {
	return mgr.StreamCommitted(func(rec timeline.Record) error {
		if err := validator.Validate(&rec); err != nil {
			return err
		}
		frag, err := cutter.Assign(rec)
		if err != nil {
			return err
		}
		return aligner.Add(frag, len(rec.Payload))
	})
}

func produce(ctx context.Context, cfg RunConfig, mgr *checkpoint.Manager, q *stream.Queue[timeline.Record],
	validator *timeline.Validator, cutter *segment.Cutter, aligner *align.Aligner) (int, error) {
	f, err := os.Open(cfg.InputPath)
	if err != nil {
		return 0, media.E(media.ErrCheckpoint, "app.read", media.Meta{}, err)
	}
	defer f.Close()
	r := timeline.NewReader(f)
	seen := 0
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			return seen, nil
		}
		if err != nil {
			return seen, err
		}
		seen++
		if rec.RecordNo < mgr.Index.NextRecordNo {
			continue
		}
		if err := validator.Validate(rec); err != nil {
			return seen, err
		}
		frag, err := cutter.Assign(*rec)
		if err != nil {
			return seen, err
		}
		if err := aligner.Add(frag, len(rec.Payload)); err != nil {
			return seen, err
		}
		weight := int64(len(rec.Payload) + 512)
		if err := q.Push(ctx, *rec, weight); err != nil {
			return seen, err
		}
	}
}

func consume(ctx context.Context, cfg RunConfig, mgr *checkpoint.Manager, q *stream.Queue[timeline.Record], initialChunks int) (error, int64) {
	var processed int64
	armed := cfg.InterruptAfterChunks > 0 && initialChunks < cfg.InterruptAfterChunks
	var chunk *checkpoint.ChunkWriter

	for {
		rec, weight, err := q.Pop(ctx)
		if errors.Is(err, stream.ErrClosed) {
			break
		}
		if err != nil {
			return err, processed
		}
		processed++
		if err := stream.SlowHook(ctx, processed, cfg.SlowEvery, cfg.SlowDelay); err != nil {
			q.Release(weight)
			return err, processed
		}
		if chunk == nil {
			chunk, err = mgr.BeginChunk()
			if err != nil {
				q.Release(weight)
				return err, processed
			}
		}
		if err := chunk.Write(rec); err != nil {
			q.Release(weight)
			return err, processed
		}
		// The record has been streamed into the chunk hasher/file; its weighted
		// queue reservation can now be released before the index commit.
		q.Release(weight)

		if chunk.Count() < cfg.ChunkRecords {
			continue
		}
		if armed && len(mgr.Index.Chunks) == cfg.InterruptAfterChunks {
			if err := chunk.Persist(); err != nil {
				return err, processed
			}
			meta := rec.Meta(rec.RecordNo, rec.LineNo)
			chunk = nil // Deliberately leave an uncommitted orphan for recovery.
			return media.E(media.ErrInterrupted, "app.interrupt", meta,
				fmt.Errorf("simulated interruption after %d committed chunks", cfg.InterruptAfterChunks)), processed
		}
		if _, err := chunk.Commit(); err != nil {
			return err, processed
		}
		chunk = nil
	}
	if chunk != nil && chunk.Count() > 0 {
		if _, err := chunk.Commit(); err != nil {
			return err, processed
		}
	}
	return nil, processed
}
