package main

import (
	"errors"
	"fmt"

	"mediaaudit/internal/index"
	"mediaaudit/internal/model"
	"mediaaudit/internal/pipeline"
)

func runScan(args []string, timed bool) error {
	f, err := parseFlags(args)
	if err != nil {
		return err
	}
	pkgDir, err := f.required("package")
	if err != nil {
		return err
	}
	indexPath, err := f.required("index")
	if err != nil {
		return err
	}
	chunkFrames, err := f.int64("chunk-frames", 4096)
	if err != nil {
		return err
	}
	cpEvery, err := f.int64("checkpoint-every", 8)
	if err != nil {
		return err
	}
	interrupt, err := f.int64("interrupt-after-checkpoint", 0)
	if err != nil {
		return err
	}
	p := pipeline.Params{
		ChunkFrames:               chunkFrames,
		CheckpointEvery:           cpEvery,
		InterruptAfterCheckpoint:  interrupt,
		Workers:                   4,
	}
	rep, _, err := runEngine(pkgDir, indexPath, f.string("report", ""), p, false)
	if err != nil {
		if pipeline.IsInterrupted(err) {
			fmt.Printf("scan interrupted after checkpoint %d\n", interrupt)
		}
		return err
	}
	printSummary(rep)
	return nil
}

func runResume(args []string) error {
	f, err := parseFlags(args)
	if err != nil {
		return err
	}
	pkgDir, err := f.required("package")
	if err != nil {
		return err
	}
	indexPath, err := f.required("index")
	if err != nil {
		return err
	}
	reportPath, err := f.required("report")
	if err != nil {
		return err
	}
	// Resume reuses the chunk/checkpoint sizing recorded inside the index.
	cfg, totalFrames, err := loadConfig(pkgDir, 0, 0)
	if err != nil {
		return err
	}
	existing, err := index.Load(indexPath)
	if err != nil {
		return err
	}
	cfg.ChunkFrames = existing.Payload.Config.ChunkFrames
	cfg.CheckpointEvery = existing.Payload.Config.CheckpointEvery
	p := pipeline.Params{
		ChunkFrames:     cfg.ChunkFrames,
		CheckpointEvery: cfg.CheckpointEvery,
		Workers:         4,
	}
	rep, resumeMS, err := runEngine(pkgDir, indexPath, reportPath, p, true)
	if err != nil {
		return err
	}
	_ = totalFrames
	fmt.Printf("resume complete in %d ms\n", resumeMS)
	printSummary(rep)
	return nil
}

func runVerify(args []string) error {
	f, err := parseFlags(args)
	if err != nil {
		return err
	}
	pkgDir, err := f.required("package")
	if err != nil {
		return err
	}
	indexPath, err := f.required("index")
	if err != nil {
		return err
	}
	reportPath, err := f.required("report")
	if err != nil {
		return err
	}
	idx, err := index.Load(indexPath)
	if err != nil {
		return err
	}
	cfg := idx.Payload.Config
	if err := idx.ValidateConfig(cfg); err != nil {
		return err
	}
	an, err := loadAnalysis(pkgDir, cfg.ChunkFrames, cfg.TileFrames)
	if err != nil {
		return err
	}
	defer an.wavReader.Close()
	fn := func(seq int64, r frameRangeType) (int, error) {
		return an.chunkDamage(seq, r)
	}
	if err := pipeline.Recompute(idx, idx.Payload.TotalFrames, cfg.ChunkFrames, fn); err != nil {
		return err
	}
	// Re-render the report and confirm it matches the stored checksum.
	rep, err := an.buildReport(idx, pkgDir, 0)
	if err != nil {
		return err
	}
	stored, err := readReportSHA(reportPath)
	if err != nil {
		return err
	}
	if stored != rep.ReportSHA256 {
		return model.NewFault(model.KindIndex, "", model.FrameRange{}, -1,
			"report checksum differs from recomputation")
	}
	fmt.Println("verify OK: index and report match recomputation")
	printSummary(rep)
	_ = errors.Is
	return nil
}

func printSummary(r *pipeline.Report) {
	if r == nil {
		return
	}
	fmt.Printf("damage_count=%d sample_frames=%d..%d byte_offset=%d\n",
		r.DamageCount, r.SampleFrames[0], r.SampleFrames[1], r.ByteOffset)
	if len(r.KeyframeGapMs) > 0 {
		fmt.Printf("keyframe_gap_ms=%v\n", r.KeyframeGapMs)
	}
	fmt.Printf("clock_drift_ppm=%.1f resume_ms=%d\n", r.ClockDriftPPM, r.ResumeMs)
	fmt.Printf("index_sha256=%s\nreport_sha256=%s\n", r.IndexSHA256, r.ReportSHA256)
}
