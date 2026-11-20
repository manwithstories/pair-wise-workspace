// Package pipeline wires every stage together: ingest, timeline repair, graph
// ordering, audio/color normalization, MP4 indexing and the final manifest.
// Tracks run with bounded concurrency but results are assembled in a fixed
// sorted order so output is deterministic. Progress is checkpointed per stage;
// an interrupted run resumes by skipping stages whose input digest matches.
package pipeline

import (
	"errors"
	"os"
	"time"

	"github.com/mediamend/mediamend/internal/checkpoint"
	"github.com/mediamend/mediamend/internal/fixture"
	"github.com/mediamend/mediamend/internal/ingest"
)

// ErrInterrupted marks a controlled stop requested through InterruptAfter.
var ErrInterrupted = errors.New("pipeline interrupted after requested stage")

// Config controls one run.
type Config struct {
	Root            string
	Project         *fixture.Project
	Inputs          []ingest.TrackInput
	KeyEvery        uint32
	MaxWorkers      int
	InterruptAfter  string // stage name; empty runs to completion
	RandomSeeks     int
	SeekSeed        int64
	ExpectedCorrupt []uint64 // ground-truth corrupted seqs (for precision)
}

// Run executes (or resumes) the pipeline and returns the aggregated result.
func Run(cfg Config) (*Result, error) {
	if cfg.MaxWorkers <= 0 {
		cfg.MaxWorkers = 4
	}
	sem := make(chan struct{}, cfg.MaxWorkers)
	store := checkpoint.NewStore(cfg.Root)
	res := newResult()

	digest, err := ingest.DigestInputs(cfg.Inputs)
	if err != nil {
		return res, err
	}
	res.InputDigest = digest

	cp, cpFaults, err := store.Resume(digest)
	if err != nil {
		res.Faults = append(res.Faults, cpFaults...)
		return res, err // exit code 4 handled by the CLI via fault class
	}
	if cp == nil {
		cp = checkpoint.Fresh(digest)
	} else {
		res.Resumed = true
		res.SkippedStages = cp.SkippedStages()
	}

	// Stage 1: ingest (streaming, bounded concurrency).
	start := time.Now()
	if !cp.Skip(checkpoint.StageIngest) {
		if err := runIngest(cfg, sem, res); err != nil {
			return res, err
		}
		if err := store.Mark(cp, checkpoint.StageIngest, checkpoint.Position{}); err != nil {
			return res, err
		}
	} else {
		if err := loadFramesForResume(cfg, res); err != nil {
			return res, err
		}
	}
	if cfg.InterruptAfter == checkpoint.StageIngest {
		return res, ErrInterrupted
	}

	// Stage 2: timeline repair per track.
	if !cp.Skip(checkpoint.StageTimeline) {
		runTimeline(cfg, sem, res)
		if err := store.Mark(cp, checkpoint.StageTimeline, checkpoint.Position{}); err != nil {
			return res, err
		}
	}
	if cfg.InterruptAfter == checkpoint.StageTimeline {
		return res, ErrInterrupted
	}

	// Stage 3: graph build and cycle detection.
	if !cp.Skip(checkpoint.StageGraph) {
		if err := runGraph(cfg, res); err != nil {
			return res, err // graph cycle: fatal, no checkpoint mark
		}
		_ = store.Mark(cp, checkpoint.StageGraph, checkpoint.Position{})
	}
	if cfg.InterruptAfter == checkpoint.StageGraph {
		return res, ErrInterrupted
	}

	// Stages 4-5: audio then video/color, driven by graph layers.
	if !cp.Skip(checkpoint.StageAudio) {
		runAudio(cfg, sem, res)
		_ = store.Mark(cp, checkpoint.StageAudio, checkpoint.Position{})
	}
	if !cp.Skip(checkpoint.StageColor) {
		runColor(cfg, sem, res)
		_ = store.Mark(cp, checkpoint.StageColor, checkpoint.Position{})
	}
	if cfg.InterruptAfter == checkpoint.StageAudio || cfg.InterruptAfter == checkpoint.StageColor {
		return res, ErrInterrupted
	}

	// Stage 6: index build/verify + random seeks.
	if !cp.Skip(checkpoint.StageIndex) {
		if err := runIndex(cfg, sem, res); err != nil {
			return res, err
		}
		_ = store.Mark(cp, checkpoint.StageIndex, checkpoint.Position{FrameSeq: uint64(res.FramesTotal)})
	}
	if cfg.InterruptAfter == checkpoint.StageIndex {
		return res, ErrInterrupted
	}

	// Stage 7: manifest + stable output checksum.
	if !cp.Skip(checkpoint.StageManifest) {
		if err := runManifest(cfg, res); err != nil {
			return res, err
		}
		_ = store.Mark(cp, checkpoint.StageManifest, checkpoint.Position{})
	}
	res.ResumeMS = time.Since(start).Milliseconds()
	res.SkippedStages = cp.SkippedStages()
	finalizeMetrics(cfg, res)
	return res, nil
}

// sortedTrackIDs is defined on Result in result.go.

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
