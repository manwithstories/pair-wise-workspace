// Package run wires the pairflow stages into one reusable library entry
// point: generate/inspect inputs, parse and reorder, segment, index, verify,
// and summarise.
//
// It exists so the command line tools stay thin drivers. Every observable
// behaviour (progress events, structured faults, metrics) is available here
// first, and the acceptance output the CLI prints is a direct projection of
// RunReport.
package run

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mediamend/mediamend/internal/clock"
	"github.com/mediamend/mediamend/internal/fault"
	"github.com/mediamend/mediamend/internal/frame"
	"github.com/mediamend/mediamend/internal/index"
	"github.com/mediamend/mediamend/internal/pairflow"
	"github.com/mediamend/mediamend/internal/pipeline"
	"github.com/mediamend/mediamend/internal/segment"
	"github.com/mediamend/mediamend/internal/verify"
)

// Config controls one end-to-end run.
type Config struct {
	// InputDir is the fixture directory holding fixture.json and input/.
	InputDir string
	// OutDir receives segments, manifests and the keyframe index.
	OutDir string
	// QueueCap is the per-stage queue capacity.
	QueueCap int
	// SlowConsumer injects latency downstream to exercise backpressure.
	SlowConsumer int64 // nanoseconds
	// Ladder is the representation list.
	Ladder []segment.Representation
	// ReorderWindow overrides the per-camera reorder depth.
	ReorderWindow int
	// TargetDuration is the segment target length in master ticks.
	TargetDuration uint64
	// RandomSeeks is the number of index seeks the verifier exercises.
	RandomSeeks int
	// Progress receives runner progress events.
	Progress func(pipeline.Event)
}

// Report is the acceptance-facing summary.
type Report struct {
	RoundTripRate      float64        `json:"roundtrip_rate"`
	KeyframeHitRate    float64        `json:"keyframe_hit_rate"`
	SegmentSHAChecked  int            `json:"segment_sha256_checked"`
	AlignmentError     uint64         `json:"alignment_error_ticks"`
	PeakInflight       int            `json:"peak_inflight_frames"`
	CorruptRecords     int            `json:"corrupt_records"`
	FirstErrorOffset   int64          `json:"first_error_offset"`
	BackpressureWaitNS int64          `json:"backpressure_wait_ns"`
	BackpressureHits   int            `json:"backpressure_hits"`
	FramesParsed       int            `json:"frames_parsed"`
	FramesVerified     int            `json:"frames_verified"`
	FramesCommitted    int            `json:"frames_committed"`
	Quarantined        int            `json:"quarantined_frames"`
	LateFrames         int            `json:"late_frames"`
	Segments           int            `json:"segments"`
	SegmentsPerCamera  map[string]int `json:"segments_per_camera"`
	Manifests          []string       `json:"manifests"`
	IndexFiles         []string       `json:"index_files"`
	Canceled           bool           `json:"canceled"`
	ElapsedNS          int64          `json:"elapsed_ns"`
	Faults             fault.List     `json:"faults"`
	MissingKeyframes   int            `json:"missing_keyframes"`
	DuplicateFrames    int            `json:"duplicate_frames"`
	TimestampWraps     int            `json:"timestamp_wraps"`
}

// Fatal reports whether the run must exit non-zero.
func (r *Report) Fatal() bool { return r.Faults.Fatal() }

// Run executes the full pipeline for one fixture directory.
func Run(ctx context.Context, cfg Config) (*Report, error) {
	if cfg.InputDir == "" || cfg.OutDir == "" {
		return nil, fmt.Errorf("run: input and output directories are required")
	}
	if cfg.QueueCap <= 0 {
		cfg.QueueCap = 8
	}
	if cfg.RandomSeeks <= 0 {
		cfg.RandomSeeks = 100
	}
	if cfg.TargetDuration == 0 {
		cfg.TargetDuration = 90000
	}
	if len(cfg.Ladder) == 0 {
		cfg.Ladder = segment.DefaultLadder
	}
	fx, err := LoadFixture(cfg.InputDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return nil, err
	}

	artDir := filepath.Join(cfg.OutDir, "output")
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		return nil, err
	}
	_ = os.RemoveAll(filepath.Join(artDir, "output")) // stale nested output from older builds
	report := &Report{SegmentsPerCamera: map[string]int{}, FirstErrorOffset: -1}

	// Build one pipeline source per camera, loading its metadata sidecar.
	var sources []pipeline.Source
	expectedFrames := map[string]int{}
	for _, cam := range fx.Cameras {
		file := fx.Files[cam.ID]
		metaPath := fx.Files[cam.ID+".meta"]
		if file == "" || metaPath == "" {
			return nil, fmt.Errorf("run: fixture is missing files for camera %s", cam.ID)
		}
		md, err := frame.LoadMetadata(metaPath)
		if err != nil {
			return nil, fmt.Errorf("run: load metadata for %s: %w", cam.ID, err)
		}
		anchorTS := camAnchor(md)
		sources = append(sources, pipeline.Source{
			Camera: cam.ID,
			File:   file,
			Meta:   md,
			Window: cfg.ReorderWindow,
			Clock: clock.Config{
				Camera: cam.ID, Rate: cam.Rate, FrameStep: cam.FrameStep,
				AnchorSeq: 0, AnchorDeviceTS: anchorTS, AnchorMasterTS: 0,
			},
		})
		expectedFrames[cam.ID] = int(cam.Frames)
	}

	// Flow: parse -> reorder -> transform -> verify.
	runner := pipeline.New(pipeline.FlowConfig{
		QueueCap:      cfg.QueueCap,
		MaxInflight:   24,
		SlowConsumer:  nsToDuration(cfg.SlowConsumer),
		Progress:      cfg.Progress,
		ReorderWindow: cfg.ReorderWindow,
	})
	res, flowErr := runner.Flow(ctx, sources)
	report.Canceled = res.Canceled
	report.Faults = append(report.Faults, res.Faults...)
	report.PeakInflight = res.Metrics.PeakInflight
	report.BackpressureWaitNS = res.Metrics.WaitNanos
	report.BackpressureHits = res.Metrics.BackpressureHits
	report.FramesParsed = res.Metrics.Parsed
	report.FramesVerified = res.Metrics.Verified
	report.FramesCommitted = res.Metrics.Committed
	report.Quarantined = res.Metrics.Quarantined
	report.LateFrames = res.Metrics.LateFrames
	report.ElapsedNS = res.Metrics.ElapsedNanos

	// Segment everything that was verified. Cancellation keeps the
	// segments that were already committed by earlier runs.
	segResults, segFaults := segment.SegmentAll(res.Items, segment.Options{Config: segment.Config{
		Root:           cfg.OutDir, // segment writer appends "output"
		Ladder:         cfg.Ladder,
		TargetDuration: cfg.TargetDuration,
	}})
	report.Faults = append(report.Faults, segFaults...)
	for _, sr := range segResults {
		report.Segments += len(sr.Segments)
		report.SegmentsPerCamera[sr.Camera] = sr.SegmentCount(cfg.Ladder[0].Name)
		report.MissingKeyframes += sr.MissingKeys
		report.Manifests = append(report.Manifests, sr.Manifests...)
	}

	// Keyframe index per camera.
	for _, sr := range segResults {
		refs := make([]index.SegmentRef, 0, len(sr.Segments))
		for _, s := range sr.Segments {
			refs = append(refs, index.SegmentRef{
				File: s.File, Representation: s.Representation, StartTS: s.StartTS,
				FirstSeq: s.FirstSeq, FrameCount: s.FrameCount,
			})
		}
		idx, fs := index.BuildKeyframe(sr.Camera, refs, artDir)
		report.Faults = append(report.Faults, fs...)
		if idx != nil {
			if err := idx.Save(artDir); err != nil {
				report.Faults = append(report.Faults, fault.Fault{
					Kind: fault.KindWriteFailure, Stage: fault.StageIndex,
					Camera: sr.Camera, Message: err.Error(), Fatal: true,
				})
			} else {
				report.IndexFiles = append(report.IndexFiles, filepath.Join(artDir, index.FileName))
			}
		}
	}

	// Verify the delivery against itself and the ingest counts.
	vopts := verify.Options{
		OutDir:       artDir,
		RandomSeeks:  cfg.RandomSeeks,
		SeekSeed:     1,
		Ladder:       cfg.Ladder,
		SourceFrames: map[string]int{},
	}
	for cam, n := range expectedFrames {
		vopts.SourceFrames[cam] = n
	}
	vr := verify.Run(vopts)
	report.RoundTripRate = vr.RoundTripRate
	report.KeyframeHitRate = vr.KeyframeHitRate
	report.SegmentSHAChecked = vr.SegmentsChecked
	report.AlignmentError = vr.AlignmentError
	report.Faults = append(report.Faults, vr.Faults...)

	// Count defect classes for the terminal summary.
	report.CorruptRecords = report.Faults.Count(fault.KindChecksumMismatch)
	report.DuplicateFrames = report.Faults.Count(fault.KindDuplicateFrame)
	report.TimestampWraps = report.Faults.Count(fault.KindSequenceWrap)
	if off, ok := report.Faults.MinOffset(); ok {
		report.FirstErrorOffset = off
	}
	report.Faults = report.Faults.Sorted()

	// Persist the report next to the artefacts for later inspection.
	if err := writeReport(cfg.OutDir, report); err != nil {
		return report, err
	}
	if flowErr != nil && flowErr != pipeline.ErrFlowCanceled {
		return report, flowErr
	}
	if res.Canceled {
		return report, pipeline.ErrFlowCanceled
	}
	return report, nil
}

// camAnchor picks the first frame's device timestamp as the clock anchor.
func camAnchor(md *frame.Metadata) uint64 {
	es := md.Entries()
	if len(es) == 0 {
		return 0
	}
	sort.SliceStable(es, func(i, j int) bool { return es[i].Seq < es[j].Seq })
	return es[0].DeviceTS
}

func writeReport(outDir string, r *Report) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(outDir, ".report-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, filepath.Join(outDir, "report.json"))
}

// LoadFixture reads the fixture manifest written by pairflow.Generate.
func LoadFixture(dir string) (*pairflow.Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "fixture.json"))
	if err != nil {
		return nil, fmt.Errorf("run: read fixture.json: %w", err)
	}
	var m pairflow.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("run: parse fixture.json: %w", err)
	}
	if len(m.Cameras) == 0 {
		return nil, fmt.Errorf("run: fixture.json declares no cameras")
	}
	return &m, nil
}

// nsToDuration converts a nanosecond count to a time.Duration.
func nsToDuration(ns int64) time.Duration { return time.Duration(ns) }

// FormatLadder renders a ladder for terminal output.
func FormatLadder(l []segment.Representation) string {
	names := make([]string, len(l))
	for i, r := range l {
		names[i] = r.Name
	}
	return strings.Join(names, ",")
}
