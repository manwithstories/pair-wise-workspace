package pipeline

import (
	"fmt"
	"path/filepath"
	"strings"

	"mediarun/internal/ingest"
	"mediarun/internal/mapping"
	"mediarun/internal/mediaerr"
	"mediarun/internal/metadata"
	"mediarun/internal/timebase"
)

// validateOptions enforces work-directory containment and required names.
func validateOptions(opts Options) error {
	if opts.WorkDir == "" || opts.InputPath == "" || opts.OutputName == "" {
		return mediaerr.New(mediaerr.CodeInvalidArgs,
			"work dir, input path and output name are required")
	}
	if err := insideWorkDir(opts.WorkDir, opts.InputPath); err != nil {
		return err
	}
	if opts.QueueDepth < 0 {
		return mediaerr.New(mediaerr.CodeInvalidArgs, "queue depth must be >= 0")
	}
	return nil
}

// insideWorkDir rejects path traversal: every resolved path must stay within
// the sandboxed work directory.
func insideWorkDir(workDir, target string) error {
	absWork, err := filepath.Abs(workDir)
	if err != nil {
		return mediaerr.New(mediaerr.CodePathTraversal, err.Error())
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return mediaerr.New(mediaerr.CodePathTraversal, err.Error())
	}
	rel, err := filepath.Rel(absWork, absTarget)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return mediaerr.New(mediaerr.CodePathTraversal,
			"path escapes work directory: "+target).OnPath(target)
	}
	return nil
}

// mappingRules serializes the validated plan into trace-friendly rule maps.
func mappingRules(plan *mapping.Plan) []map[string]string {
	out := make([]map[string]string, 0, len(plan.Targets))
	for _, t := range plan.Targets {
		out = append(out, map[string]string{
			"target": string(t.Role), "source": t.Source.ID, "codec": t.Source.Codec,
		})
	}
	return out
}

// resolveAll runs metadata arbitration for every track that has metadata input.
func resolveAll(opts Options, info ingest.ContainerInfo) (map[string][]metadata.Decision, error) {
	out := map[string][]metadata.Decision{}
	for _, tr := range info.Tracks {
		in, ok := opts.Metadata[tr.ID]
		if !ok {
			in = metadata.Input{TrackID: tr.ID, CodedWidth: int(tr.Width), CodedHeight: int(tr.Height)}
		}
		view, err := metadata.Resolve(in)
		if err != nil {
			return nil, err
		}
		out[tr.ID] = view.Decisions
	}
	return out, nil
}

// prepareResume loads and verifies any prior checkpoint, returning the sample
// boundary to continue from. Without --resume an existing checkpoint is
// ignored for a fresh run (the work dir is expected to be clean per demo).
func prepareResume(opts Options, inputHash, mappingHash string) (int64, string, error) {
	cp, err := LoadCheckpoint(opts.WorkDir)
	if err != nil {
		return 0, "", err
	}
	if cp == nil {
		return 0, "", nil
	}
	if !opts.Resume {
		// Stale checkpoint present on a fresh run: treat as mismatch so the
		// operator cannot accidentally merge two jobs in one work dir.
		return 0, "", mediaerr.New(mediaerr.CodeCheckpointMismatch,
			"checkpoint exists but --resume was not requested")
	}
	if err := cp.Verify(inputHash, mappingHash); err != nil {
		return 0, "", err
	}
	return cp.LastCompleteSample, cp.OutputSHA256, nil
}

// isInterrupt recognizes the clean interrupt sentinel by its marker text.
func isInterrupt(err error) bool {
	return err != nil && strings.Contains(err.Error(), "INTERRUPT_BOUNDARY")
}

// checkpointInterrupt persists the partial boundary so a later --resume can
// continue, then returns the stats with committed=false (exit 0 for the
// interrupt leg of the demo, which is an expected, recoverable stop).
func checkpointInterrupt(opts Options, stats *Stats, outputPath string, trace *Trace) (*Stats, error) {
	cp := Checkpoint{
		InputSHA256:        stats.InputSHA256,
		MappingSHA256:      stats.MappingSHA256,
		LastCompleteSample: stats.SamplesOut,
		OutputSHA256:       stats.OutputSHA256,
	}
	if err := SaveCheckpoint(opts.WorkDir, cp); err != nil {
		return stats, err
	}
	if _, err := trace.Finalize(opts.WorkDir, opts.ManifestName, "partial"); err != nil {
		return stats, err
	}
	_ = outputPath
	return stats, nil
}

// finalizeTraceAndCheckpoint writes the provenance manifest and the terminal
// checkpoint atomically alongside the committed output.
func finalizeTraceAndCheckpoint(opts Options, inputHash, mappingHash, outputHash string,
	stats *Stats, trace *Trace) error {
	if _, err := trace.Finalize(opts.WorkDir, opts.ManifestName, outputHash); err != nil {
		return err
	}
	cp := Checkpoint{
		InputSHA256: inputHash, MappingSHA256: mappingHash,
		LastCompleteSample: stats.SamplesOut, OutputSHA256: outputHash,
	}
	return SaveCheckpoint(opts.WorkDir, cp)
}

// fillBitrateAndAlignment computes reported bitrate from total encoded sample
// bytes over exact media duration, and the cross-track alignment error in ms.
func fillBitrateAndAlignment(stats *Stats, info ingest.ContainerInfo) {
	var encodedBytes int64
	durations := map[string]timebase.Rational{}
	for _, tr := range info.Tracks {
		tb := timebase.NewTimebase(tr.Timescale)
		durations[tr.ID] = tb.ToSeconds(stats.ticks[tr.ID])
		encodedBytes += stats.bytes[tr.ID]
	}
	// Media duration = longest track timeline (exact rational).
	var mediaDur timebase.Rational
	for _, d := range durations {
		if d.Cmp(mediaDur) > 0 {
			mediaDur = d
		}
	}
	stats.DurationSeconds = mediaDur.Float64()
	if mediaDur.IsPositive() && encodedBytes > 0 {
		// bits/sec = bytes*8 / seconds, computed exactly then rounded.
		bits := timebase.New(encodedBytes*8, 1)
		bps := bits.Div(mediaDur)
		stats.ActualBitrateBPS = nearestInt(bps.Float64())
	}
	if stats.TargetBitrateBPS > 0 && stats.ActualBitrateBPS > 0 {
		dev := float64(stats.ActualBitrateBPS-stats.TargetBitrateBPS) /
			float64(stats.TargetBitrateBPS) * 100
		stats.BitrateDeviationPct = round1(dev)
	}
	// Cross-track alignment: end-time difference between video and audio,
	// located by kind rather than a hard-coded track index.
	var video, audio timebase.Rational
	var hasV, hasA bool
	for _, tr := range info.Tracks {
		switch tr.Kind {
		case ingest.KindVideo:
			video, hasV = durations[tr.ID], true
		case ingest.KindAudio:
			audio, hasA = durations[tr.ID], true
		}
	}
	if hasV && hasA {
		stats.ManifestAlignMillis = alignmentError(video, audio)
	}
}

// enforceAlignment computes the statistics and rejects a manifest whose
// cross-track drift exceeds the configured threshold, so a misaligned
// deliverable can never reach the atomic commit.
func enforceAlignment(opts Options, stats *Stats, info ingest.ContainerInfo) error {
	fillBitrateAndAlignment(stats, info)
	limit := opts.MaxAlignMillis
	if limit <= 0 {
		limit = 1 // specification default: |alignment| <= 1 ms
	}
	if stats.ManifestAlignMillis > limit {
		return mediaerr.New(mediaerr.CodeManifestMisalign, fmt.Sprintf(
			"cross-track drift %d ms exceeds limit %d ms",
			stats.ManifestAlignMillis, limit))
	}
	return nil
}

func nearestInt(f float64) int64 {
	if f >= 0 {
		return int64(f + 0.5)
	}
	return int64(f - 0.5)
}

func round1(f float64) float64 {
	return float64(int64(f*10+sign(f)*0.5)) / 10
}

func sign(f float64) float64 {
	if f < 0 {
		return -1
	}
	return 1
}
