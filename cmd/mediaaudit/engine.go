package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	prtp "github.com/pion/rtp"

	"mediaaudit/internal/clock"
	"mediaaudit/internal/fixture"
	"mediaaudit/internal/index"
	"mediaaudit/internal/model"
	"mediaaudit/internal/mp4"
	"mediaaudit/internal/pipeline"
	"mediaaudit/internal/rtp"
	"mediaaudit/internal/wav"
)

// analysis holds the parsed subsystem results shared across chunk callbacks.
type analysis struct {
	manifest    *fixture.Manifest
	wavReader   *wav.Reader
	inspect     *wav.InspectReport
	drift       rtp.DriftResult
	mp4Result   *mp4.ScanResult
	chunkFrames int64
	tileFrames  int64
}

// loadAnalysis opens and analyzes every artifact in the package directory.
func loadAnalysis(pkgDir string, chunkFrames, tileFrames int64) (*analysis, error) {
	manPath := filepath.Join(pkgDir, fixture.ManifestFile)
	raw, err := os.ReadFile(manPath)
	if err != nil {
		return nil, err
	}
	var man fixture.Manifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return nil, err
	}

	wr, err := wav.NewReader(filepath.Join(pkgDir, fixture.AudioFile))
	if err != nil {
		return nil, err
	}
	ins, err := wr.Inspect()
	if err != nil {
		wr.Close()
		// Truncation is a located fault, not a fatal error; continue if report exists.
		if ins == nil {
			return nil, err
		}
	}
	// Detect a frame-count shortfall against the manifest even when the WAV is
	// itself frame-aligned (physical EOF). The missing tail is isolated to the
	// final residual block rather than condemning the whole track.
	actualFrames := wr.Header().FrameCount
	if tf := wr.TailFault(); tf != nil {
		ins.Faults.Add(*tf)
	}
	if actualFrames < man.Frames {
		ins.Faults.Add(model.Fault{
			Kind: model.KindTruncated,
			Range: model.FrameRange{
				First: model.Frame(actualFrames),
				Last:  model.Frame(man.Frames),
			},
			ByteOffset: wr.Header().DataChunk.Offset +
				actualFrames*wr.Header().FrameBytes,
			Message: "audio shorter than manifest: tail frames missing",
		})
	}

	drift, err := analyzeRTP(filepath.Join(pkgDir, fixture.RTPFile))
	if err != nil {
		wr.Close()
		return nil, err
	}
	mres, err := mp4.Scan(filepath.Join(pkgDir, fixture.MP4File))
	if err != nil {
		wr.Close()
		return nil, err
	}
	return &analysis{
		manifest: &man, wavReader: wr, inspect: ins, drift: drift,
		mp4Result: mres, chunkFrames: chunkFrames, tileFrames: tileFrames,
	}, nil
}

func analyzeRTP(path string) (rtp.DriftResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return rtp.DriftResult{}, err
	}
	defer f.Close()
	s := rtp.NewStream("rtp", 90000)
	err = rtp.ReadPackets(f, func(p *prtp.Packet, wallNanos int64) error {
		s.Push(p.SequenceNumber, p.Timestamp, wallNanos)
		return nil
	})
	if err != nil {
		return rtp.DriftResult{}, err
	}
	return s.EstimateDrift()
}

// chunkDamage returns the number of located faults intersecting a chunk. A
// short-tail truncation beyond the last full frame is attributed to the final
// chunk so the damage is not silently dropped.
func (a *analysis) chunkDamage(seq int64, r frameRangeType) (int, error) {
	first := int64(r.First)
	last := int64(r.Last)
	count := 0
	totalChunks := (a.manifest.Frames + a.chunkFrames - 1) / a.chunkFrames
	isLastChunk := seq == totalChunks-1
	for _, fault := range a.inspect.Faults.All() {
		intersects := int64(fault.Range.Last) > first && int64(fault.Range.First) < last
		if !intersects && isLastChunk &&
			(fault.Kind == model.KindTruncated || fault.Kind == model.KindAlignment) {
			// Tail fault beyond the clamped frame count: attach to last chunk.
			intersects = int64(fault.Range.First) >= first
		}
		if intersects {
			count++
		}
	}
	return count, nil
}

// buildReport assembles the deterministic report from the finalized index and
// the subsystem analyses. resumeMS is measured by the caller.
func (a *analysis) buildReport(idx *index.File, pkgDir string, resumeMS int64) (*pipeline.Report, error) {
	fileSHA, err := idx.FileSHA256()
	if err != nil {
		return nil, err
	}
	rep := &pipeline.Report{
		PackagePath:   pkgDir,
		SampleRate:    a.manifest.SampleRate,
		Channels:      a.manifest.Channels,
		DamageCount:   idx.DamageCount(),
		ClockDriftPPM: a.drift.PPM,
		Chunks:        len(idx.Payload.Chunks),
		ResumeMs:      resumeMS,
		IndexSHA256:   fileSHA,
	}
	// Keyframe gaps from the first video track.
	if len(a.mp4Result.Tracks) > 0 {
		rep.KeyframeGapMs = append([]float64(nil), a.mp4Result.Tracks[0].KeyframeGapsMs...)
	}
	// First located damage range with its source byte offset.
	for _, fault := range a.inspect.Faults.All() {
		if fault.Kind.String() != "crc" && fault.Kind.String() != "truncated" &&
			fault.Kind.String() != "alignment" {
			continue
		}
		rep.SampleFrames = [2]int64{int64(fault.Range.First), int64(fault.Range.Last)}
		rep.ByteOffset = fault.ByteOffset
		rep.Faults = append(rep.Faults, fault)
		break
	}
	// Cross-track projection onto the proxy timeline for traceability.
	if len(a.mp4Result.Tracks) > 0 {
		a.attachCrossTrack(rep)
	}
	for _, f := range a.inspect.Faults.All() {
		rep.Faults = appendFaultOnce(rep.Faults, f)
	}
	if err := pipeline.FinalizeReport(rep); err != nil {
		return nil, err
	}
	return rep, nil
}

func (a *analysis) attachCrossTrack(rep *pipeline.Report) {
	if len(rep.Faults) == 0 {
		return
	}
	audioRate := a.wavReader.Header().Rate()
	videoRate := clock.TrackRate{Track: "mp4", Rate: videoRateOf(a.mp4Result)}
	src := clock.Event{
		Track: "wav", Frame: frameOf(rep.SampleFrames[0]),
		Kind: rep.Faults[0].Kind, ByteOffset: rep.ByteOffset,
	}
	dm := clock.ProjectDamage(src, audioRate, []clock.TrackRate{
		{Track: "wav", Rate: audioRate}, videoRate,
	})
	rep.Faults = append(rep.Faults, projectedFault(dm))
}

// runEngine drives a full scan or resume and returns the finished report.
func runEngine(pkgDir, indexPath, reportPath string, p pipeline.Params, timed bool) (*pipeline.Report, int64, error) {
	cfg, totalFrames, err := loadConfig(pkgDir, p.ChunkFrames, p.CheckpointEvery)
	if err != nil {
		return nil, 0, err
	}
	idx, fresh, err := openOrCreateIndex(indexPath, cfg, totalFrames)
	if err != nil {
		return nil, 0, err
	}
	if !fresh {
		if err := idx.ValidateConfig(cfg); err != nil {
			return nil, 0, err
		}
	}
	an, err := loadAnalysis(pkgDir, p.ChunkFrames, cfg.TileFrames)
	if err != nil {
		return nil, 0, err
	}
	defer an.wavReader.Close()

	rn, err := pipeline.NewRunner(idx, p)
	if err != nil {
		return nil, 0, err
	}
	start := time.Now()
	fn := func(seq int64, r frameRangeType) (int, error) {
		return an.chunkDamage(seq, r)
	}
	runErr := rn.Run(context.Background(), totalFrames, fn)
	resumeMS := time.Since(start).Milliseconds()
	if !timed {
		resumeMS = 0
	}
	if runErr != nil && !pipeline.IsInterrupted(runErr) {
		return nil, resumeMS, runErr
	}
	if pipeline.IsInterrupted(runErr) {
		// Deliberate stop: persist nothing more and surface exit 75.
		return nil, resumeMS, runErr
	}
	rep, err := an.buildReport(idx, pkgDir, resumeMS)
	if err != nil {
		return nil, resumeMS, err
	}
	if reportPath != "" {
		out, err := pipeline.Render(rep)
		if err != nil {
			return nil, resumeMS, err
		}
		if err := os.WriteFile(reportPath, append(out, '\n'), 0o644); err != nil {
			return nil, resumeMS, err
		}
	}
	return rep, resumeMS, nil
}
