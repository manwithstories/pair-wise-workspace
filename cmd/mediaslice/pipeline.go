package main

import (
	"time"

	"mediaslice/align"
	"mediaslice/media"
	"mediaslice/scan"
	"mediaslice/stream"
)

// pipelineInputs is one representation's resolved on-disk material.
type pipelineInputs struct {
	id      string
	mp4Path string
	rtpPath string
}

type pipelineResult struct {
	indexes      map[string]*scan.FileIndex
	bounds       []*align.TrackBoundary
	plan         *align.LadderPlan
	align        *align.AlignResult
	runnerResult *stream.RunResult
	scanErrors   media.ErrorList
	resumeMS     int64
}

// buildPlan scans (or reuses verified indexes for) every representation and
// computes the aligned ladder. Corrupt representations are isolated: their
// diagnostics are returned while healthy ones still produce a plan.
func buildPlan(workDir string, inputs []pipelineInputs, segmentTicks, waitTicks media.Ticks) (*pipelineResult, error) {
	res := &pipelineResult{indexes: map[string]*scan.FileIndex{}}
	cfg := align.BoundaryConfig{SegmentTicks: segmentTicks, KeyframeWaitMax: waitTicks}
	var healthy []*align.TrackBoundary
	var healthyIDs []string
	for _, in := range inputs {
		idx, _, err := scan.ScanOrLoad(workDir, in.mp4Path, in.rtpPath, scan.DefaultConfig())
		if err != nil {
			return nil, err
		}
		res.indexes[in.id] = idx
		res.scanErrors = append(res.scanErrors, idx.Errors...)
		if len(idx.Errors) > 0 || idx.VideoTrack() == nil {
			continue // isolate corrupt representation
		}
		b := align.ComputeTrackBoundary(in.id, idx, cfg)
		if len(b.TrackErrors) > 0 {
			res.scanErrors = append(res.scanErrors, b.TrackErrors...)
			continue
		}
		res.bounds = append(res.bounds, b)
		healthy = append(healthy, b)
		healthyIDs = append(healthyIDs, in.id)
	}
	if len(healthy) == 0 {
		return res, nil
	}
	res.align = align.AlignLadder(healthy, cfg)
	plan, err := align.BuildLadderPlan(res.align, healthy)
	if err != nil {
		return nil, err
	}
	res.plan = plan
	return res, nil
}

// executePlan runs the streamer, optionally injecting a crash and resuming.
func executePlan(workDir string, res *pipelineResult, cfg stream.Config,
	resume bool, checkpointEverySeg int) error {
	if !resume {
		cfg.CheckpointEvery = checkpointEverySeg
		r, err := stream.NewRunner(workDir, res.plan, res.indexes, res.bounds, cfg)
		if err != nil {
			return err
		}
		rr, err := r.Run()
		if err != nil {
			if _, ok := err.(*stream.ErrCrashInjected); ok {
				// Crash injection without resume: caller treats as fatal.
				return err
			}
			return err
		}
		res.runnerResult = rr
		return nil
	}

	// Phase 1: crash at the midpoint segment.
	crashSeq := len(res.plan.Segments) / 2
	crashCfg := cfg
	crashCfg.CrashAfterSequence = crashSeq
	crashCfg.CheckpointEvery = checkpointEverySeg
	r1, err := stream.NewRunner(workDir, res.plan, res.indexes, res.bounds, crashCfg)
	if err != nil {
		return err
	}
	if _, err := r1.Run(); err != nil {
		if _, ok := err.(*stream.ErrCrashInjected); !ok {
			return err
		}
	}

	// Phase 2: resume, timing the validated restart.
	start := time.Now()
	resumeCfg := stream.DefaultConfig()
	resumeCfg.CheckpointEvery = checkpointEverySeg
	r2, err := stream.NewRunner(workDir, res.plan, res.indexes, res.bounds, resumeCfg)
	if err != nil {
		return err
	}
	rr, err := r2.Run()
	if err != nil {
		return err
	}
	res.resumeMS = time.Since(start).Milliseconds()
	res.runnerResult = rr
	return nil
}
