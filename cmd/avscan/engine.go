package main

import (
	"path/filepath"

	"avscan/internal/batch"
	"avscan/internal/fixture"
	"avscan/internal/model"
	"avscan/internal/parse"
	"avscan/internal/scan"
	"avscan/internal/timeline"
)

func filepathDir(p string) string { return filepath.Dir(p) }

// analysisRun holds the aggregated cross-subsystem results.
type analysisRun struct {
	timeline   *model.Timeline
	globals    []model.Fault
	keyReports []scan.KeyFrameReport
	damage     scan.DamageResult
	colour     timeline.MetadataReport
	audio      timeline.MetadataReport
	alignment  timeline.AlignmentReport
	blocks     []batch.BlockResult
}

// demoClips builds clip/transition points, including one mid-grid point that
// cannot land exactly on a sample block boundary.
func demoClips(tl *model.Timeline) []model.ClipPoint {
	clips := []model.ClipPoint{
		{Label: "opening", AtNS: 0},
		{Label: "mid-review", AtNS: 123_456_789}, // intentionally non-integral
	}
	// A timeline-wide transition near the one-second mark.
	clips = append(clips, model.ClipPoint{Label: "transition-1s", AtNS: 1_000_000_000})
	return clips
}

// analyze parses every demo input, runs scan/timeline analysis and drives the
// resumable block runner.
func analyze(set fixture.DemoSet, indexPath string, blockSize int, resume bool) (demoOutput, error) {
	run := &analysisRun{}
	totalSamples := 0
	trackCount := 0
	var sampleCounts []int
	var fileSize int64

	mp4Files := []string{set.HealthyPath, set.DamagedPath, set.BoundaryPath}
	for _, path := range mp4Files {
		res, err := parse.ParseMP4(path)
		if err != nil {
			return demoOutput{}, err
		}
		fileSize += res.FileSize
		for _, tr := range res.Timeline.Tracks {
			trackCount++
			totalSamples += len(tr.Samples)
			sampleCounts = append(sampleCounts, len(tr.Samples))
		}
		run.timeline = mergeTimeline(run.timeline, res.Timeline)
		run.globals = append(run.globals, res.GlobalFaults...)
	}
	// RTP capture with its deliberate sequence gap.
	rtpRecs, err := parse.ReadRTP1File(set.RTPPath, "voice", 8000)
	if err != nil {
		return demoOutput{}, err
	}
	rtpRes, err := parse.ParseRTPStream(rtpRecs)
	if err != nil {
		return demoOutput{}, err
	}
	run.globals = append(run.globals, rtpRes.Faults...)

	run.keyReports = scan.AnalyzeAllKeyFrames(run.timeline)
	run.damage = scan.LocateDamage(run.timeline, run.globals)
	run.colour = timeline.NormalizeColour(run.timeline)
	run.audio, _ = timeline.NormalizeAudio(run.timeline)

	// Map operator clip/transition points onto the unified nanosecond axis.
	// The mid-grid point deliberately lands between samples to prove rounding
	// is reported rather than silently drifted.
	clips := demoClips(run.timeline)
	run.alignment = timeline.AlignClips(run.timeline, clips)
	run.globals = append(run.globals, run.alignment.Faults...)

	summary := batch.InputSummary{
		Source:       indexPath,
		FileSize:     fileSize,
		TrackCount:   trackCount,
		SampleCounts: sampleCounts,
		BlockSize:    blockSize,
	}
	runner, err := batch.NewRunner(batch.RunOptions{
		IndexPath: indexPath, BlockSize: blockSize,
		Resume: resume, Summary: summary,
	})
	if err != nil {
		return demoOutput{}, err
	}
	processor := func(trackID uint32, start, end int) (batch.BlockResult, error) {
		br := batch.BlockResult{TrackID: trackID, SampleStart: start, SampleEnd: end}
		for _, tr := range run.timeline.Tracks {
			if tr.ID != trackID {
				continue
			}
			for i := start; i < end && i < len(tr.Samples); i++ {
				if tr.Samples[i].IsSync {
					br.KeyFrames++
				}
			}
			for _, f := range tr.ParseFaults {
				if f.SampleIndex >= int64(start) && f.SampleIndex < int64(end) {
					br.FaultCount++
				}
			}
		}
		return br, nil
	}
	for _, tr := range run.timeline.Tracks {
		if err := runner.Process(tr.ID, len(tr.Samples), processor); err != nil {
			return demoOutput{}, err
		}
	}
	run.blocks = runner.Blocks()

	index := batch.IndexRecord{
		InputDigest:    runnerDigest(summary),
		BlockSize:      blockSize,
		Blocks:         run.blocks,
		DamageSegments: run.damage.Segments,
		Faults:         run.globals,
	}
	sum, err := batch.IndexSHA256(index)
	if err != nil {
		return demoOutput{}, err
	}
	if err := batch.AtomicWriteJSON(indexPath, index); err != nil {
		return demoOutput{}, err
	}
	out := demoOutput{
		Tracks:           trackCount,
		Samples:          totalSamples,
		DamageSegments:   len(run.damage.Segments),
		FirstBadOffset:   run.damage.FirstBadOffset,
		CheckpointBlocks: runner.Completed(),
		IndexSHA256:      sum,
		KeyFrames:        run.keyReports,
		Colour:           run.colour.Colour,
		Audio:            run.audio.Audio,
		RoundingReports:  len(run.alignment.NonIntegralRounding()),
	}
	return out, nil
}

func runnerDigest(s batch.InputSummary) string {
	d, _ := s.Digest()
	return d
}

// mergeTimeline appends src's tracks into dst (which may be nil) and remaps
// each track ID to a process-wide unique value, since every fixture file
// numbers its own tracks from 1. Sample events and faults are remapped too.
func mergeTimeline(dst, src *model.Timeline) *model.Timeline {
	if dst == nil {
		dst = &model.Timeline{MovieTimescale: src.MovieTimescale}
	}
	for _, tr := range src.Tracks {
		nextID := uint32(len(dst.Tracks) + 1)
		oldID := tr.ID
		tr.ID = nextID
		for i := range tr.Samples {
			tr.Samples[i].TrackID = nextID
		}
		for i := range tr.ParseFaults {
			if tr.ParseFaults[i].TrackID == oldID {
				tr.ParseFaults[i].TrackID = nextID
			}
		}
		dst.Tracks = append(dst.Tracks, tr)
	}
	return dst
}

// validate enforces the acceptance field contract.
func validate(out demoOutput) error {
	switch {
	case out.Tracks != 9: // 3 MP4 files x 3 tracks
		return model.ErrDamagedInput
	case out.Samples < 200:
		return model.ErrDamagedInput
	case out.DamageSegments < 2:
		return model.ErrDamagedInput
	case out.FirstBadOffset < 0:
		return model.ErrDamagedInput
	case out.CheckpointBlocks < 2:
		return model.ErrDamagedInput
	case len(out.IndexSHA256) != 64:
		return model.ErrDamagedInput
	}
	return nil
}
