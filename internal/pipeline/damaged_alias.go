package pipeline

import (
	"encoding/json"
	"path/filepath"

	"fieldcut/internal/clock"
	"fieldcut/internal/cut"
	"fieldcut/internal/events"
	"fieldcut/internal/ingest"
	"fieldcut/internal/manifest"
	"fieldcut/internal/recover"
)

type ingestTrack = ingest.MP4Track

func runDamaged(req Request, j *recover.Journal, b *Bundle, partial *ingestTrack, trunc *clock.Error, hashes map[string]string, reused []string) (*Result, error) {
	aligned, alignErrs := events.Align(b.RawEvents)
	crossErrs := events.CrossTrackSkew(aligned, 5*clock.NsPerMs)
	allErrs := append(append(append([]*clock.Error{}, alignErrs...), crossErrs...), trunc)

	primaryPCM, met, silence, _, err := processAudio(b)
	if err != nil {
		return nil, err
	}
	cutPlan, err := cut.PlanCuts(cut.Request{
		TrackID: 1, Duration: Duration,
		Candidates:      cutCandidates(aligned, silence, partial),
		AudioSampleRate: OutputRate,
		VideoFPS:        VideoFPS,
		Errors:          allErrs,
	})
	if err != nil {
		return nil, err
	}
	assignments, _ := events.Assign(aligned, cutPlan.Segments)

	m := &manifest.Manifest{
		Case: req.Case, Seed: req.Seed, Events: aligned, Assignments: assignments,
		Errors: allErrs, CutPointErrorNs: cutPlan.ErrorAnchorNs,
	}
	if partial != nil {
		first := clock.Interval{Start: 0, End: clock.NsPerSecond}
		ss := samplesInSegment(partial, first)
		if len(ss) > 0 {
			blob, err := cut.PackageFragment(partial.TrackID, partial.TimeScale, partial.Color, ss)
			if err != nil {
				return nil, err
			}
			audioBlob, err := slicePCM16(primaryPCM, first)
			if err != nil {
				return nil, err
			}
			videoPath := filepath.Join("segments", "seg-000.mp4")
			audioPath := filepath.Join("segments", "seg-000.pcm")
			if err := recover.AtomicWriteFile(filepath.Join(req.OutDir, videoPath), blob); err != nil {
				return nil, err
			}
			if err := recover.AtomicWriteFile(filepath.Join(req.OutDir, audioPath), audioBlob); err != nil {
				return nil, err
			}
			_ = j.Commit("package", hashes["package"], map[string]string{
				videoPath: HashBytes(blob), audioPath: HashBytes(audioBlob),
			})
			m.Segments = []manifest.Segment{{
				Index: 0, File: videoPath, AudioFile: audioPath, Interval: first,
				SHA256: HashBytes(blob), AudioSHA256: HashBytes(audioBlob), SizeBytes: int64(len(blob)),
				RandomAccess: true, EventIDs: eventIDsForSegment(assignments, 0),
			}}
			m.Metrics.ActualBPS = float64(len(blob)) * 8
			m.Metrics.TargetBPS = TargetBPS
			m.Metrics.BitrateDeltaPct = (m.Metrics.ActualBPS - float64(TargetBPS)) / float64(TargetBPS) * 100
		}
	}

	firstError := firstReportableError(allErrs)
	m.FirstError = firstError
	m.Metrics.RMSDBFS = met.RMSDBFS
	m.Metrics.PeakDBFS = met.PeakDBFS
	m.Metrics.ClippedSamples = met.Clipped
	m.Metrics.SegmentCount = len(m.Segments)
	m.Metrics.ManifestMaxSkewNs = cutPlan.MaxSkewNs
	m.Metrics.ReusedNodes = len(reused)
	if firstError != nil {
		m.Metrics.ErrorCode = string(firstError.Code)
		m.Metrics.TrackID = firstError.TrackID
		if firstError.EndNs > firstError.StartNs {
			m.Metrics.IntervalNs = firstError.EndNs - firstError.StartNs
		}
		report, _ := json.MarshalIndent(firstError, "", "  ")
		_ = recover.AtomicWriteFile(filepath.Join(req.OutDir, "damage.json"), append(report, '\n'))
	}
	if _, err := manifest.Write(req.OutDir, m); err != nil {
		return nil, err
	}
	if err := WriteMetrics(req.OutDir, m); err != nil {
		return nil, err
	}
	_ = j.Commit("manifest", hashes["manifest"], nil)
	return &Result{Code: 2, Manifest: m, FirstError: firstError, ReusedNodes: reused}, nil
}

func firstReportableError(errs []*clock.Error) *clock.Error {
	for _, e := range errs {
		if e != nil && e.StartNs >= 0 && e.EndNs > e.StartNs {
			return e
		}
	}
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
