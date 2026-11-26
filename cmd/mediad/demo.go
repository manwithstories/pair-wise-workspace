package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"mediad/internal/fixture"
	"mediad/internal/model"
	"mediad/internal/pipeline"
)

type demoConfig struct {
	Work           string
	Seed           int64
	Profile        string
	InterruptAfter int
}

func runDemo(cfg demoConfig) error {
	if cfg.Work == "" {
		return model.New(model.CodeInvalidArgument, model.StageCLI, "--work is required")
	}
	if cfg.Profile != "variable" && cfg.Profile != "stable" {
		return model.New(model.CodeInvalidArgument, model.StageCLI, "--profile must be variable or stable")
	}
	if cfg.InterruptAfter < 0 {
		return model.New(model.CodeInvalidArgument, model.StageCLI, "--interrupt-after must be non-negative")
	}
	if err := os.MkdirAll(cfg.Work, 0o755); err != nil {
		return model.Wrap(model.CodeInvalidArgument, model.StageCLI, "create work directory", err)
	}

	media, err := fixture.GenerateMedia(cfg.Seed, nil)
	if err != nil {
		return err
	}
	cleanPath, err := safeWriteWork(cfg.Work, "clean.mp4", media.Data)
	if err != nil {
		return err
	}
	if _, err := safeWriteWork(cfg.Work, "truncated-offset.mp4", media.Data[:media.TruncateAt]); err != nil {
		return err
	}
	truncPath, err := pipeline.SafeJoin(cfg.Work, "truncated-offset.mp4")
	if err != nil {
		return err
	}

	netPlan := fixture.GenerateNetworkPlan(media.Data, cfg.Seed, cfg.Profile)
	stats, assembled, err := fixture.RunNetworkPlan(netPlan)
	if err != nil {
		return err
	}
	if err := fixture.VerifyAssembled(media.Data, assembled); err != nil {
		return err
	}
	if _, err := safeWriteWork(cfg.Work, "weak-assembled.mp4", assembled); err != nil {
		return err
	}

	expectedOffset, detectedOffset, delta, err := probeCorruptBox(truncPath, media.ExpectedBadBox)
	if err != nil {
		return err
	}

	cachePath, err := pipeline.SafeJoin(cfg.Work, "index-cache.json")
	if err != nil {
		return err
	}
	c1, err := pipeline.LoadOrBuildIndex(cleanPath, cachePath)
	if err != nil {
		return err
	}
	c2, err := pipeline.LoadOrBuildIndex(cleanPath, cachePath)
	if err != nil {
		return err
	}
	if err := pipeline.InvalidateIndexForTest(cachePath); err != nil {
		return model.Wrap(model.CodeInternal, model.StageCLI, "invalidate cache", err)
	}
	c3, err := pipeline.LoadOrBuildIndex(cleanPath, cachePath)
	if err != nil {
		return err
	}
	indexState := c1.State + "," + c2.State + "," + c3.State

	req := demoMapping()
	freshOut, err := pipeline.SafeJoin(cfg.Work, "fresh.mp4")
	if err != nil {
		return err
	}
	freshCkpt, err := pipeline.SafeJoin(cfg.Work, "fresh.checkpoint.json")
	if err != nil {
		return err
	}
	runCache, err := pipeline.SafeJoin(cfg.Work, "run-index.json")
	if err != nil {
		return err
	}
	fresh, err := pipeline.Run(pipeline.Options{
		InputPath: cleanPath, OutputPath: freshOut, CheckpointPath: freshCkpt,
		CachePath: runCache, Requests: req,
	})
	if err != nil {
		return err
	}

	deliveredOut, err := pipeline.SafeJoin(cfg.Work, "delivered.mp4")
	if err != nil {
		return err
	}
	deliveredCkpt, err := pipeline.SafeJoin(cfg.Work, "delivered.checkpoint.json")
	if err != nil {
		return err
	}
	_, err = pipeline.Run(pipeline.Options{
		InputPath: cleanPath, OutputPath: deliveredOut, CheckpointPath: deliveredCkpt,
		CachePath: runCache, Requests: req, InterruptAfter: cfg.InterruptAfter,
	})
	if cfg.InterruptAfter > 0 && !errors.Is(err, pipeline.ErrInterrupted) {
		return model.Newf(model.CodeInternal, model.StageCLI, "expected interruption, got %v", err)
	}
	if err := appendUncommittedTail(deliveredOut); err != nil {
		return err
	}
	start := time.Now()
	resumed, err := pipeline.Run(pipeline.Options{
		InputPath: cleanPath, OutputPath: deliveredOut, CheckpointPath: deliveredCkpt,
		CachePath: runCache, Requests: req,
	})
	resumeMS := time.Since(start).Milliseconds()
	if err != nil {
		return err
	}
	if resumeMS >= 5000 {
		return model.Newf(model.CodeInternal, model.StagePipeline, "1 MiB resume took %dms >= 5000ms", resumeMS)
	}
	repeat, err := pipeline.Run(pipeline.Options{
		InputPath: cleanPath, OutputPath: deliveredOut, CheckpointPath: deliveredCkpt,
		CachePath: runCache, Requests: req,
	})
	if err != nil {
		return err
	}

	sampleCount := 0
	for _, tr := range media.Tracks {
		sampleCount += tr.Samples
	}
	equal := resumed.OutputSHA256 == fresh.OutputSHA256 && repeat.OutputSHA256 == fresh.OutputSHA256
	if !equal {
		return model.New(model.CodeIntegrity, model.StagePipeline, "demo output checksums differ")
	}

	fmt.Printf("tracks=%d\n", len(media.Tracks))
	fmt.Printf("samples=%d\n", sampleCount)
	fmt.Printf("duplicate=%d\n", stats.Duplicate)
	fmt.Printf("reordered=%d\n", stats.Reordered)
	fmt.Printf("burst_loss=%d\n", stats.BurstLoss)
	fmt.Printf("retransmit=%d\n", stats.Retransmit)
	fmt.Printf("index_state=%s\n", indexState)
	fmt.Printf("expected_offset=%d\n", expectedOffset)
	fmt.Printf("detected_offset=%d\n", detectedOffset)
	fmt.Printf("delta_bytes=%d\n", delta)
	fmt.Printf("resume_ms=%d\n", resumeMS)
	fmt.Printf("output_sha256=%s\n", resumed.OutputSHA256)
	fmt.Printf("fresh_sha256=%s\n", fresh.OutputSHA256)
	fmt.Printf("equal=%t\n", equal)
	return nil
}
