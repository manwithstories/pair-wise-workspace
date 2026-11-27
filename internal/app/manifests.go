package app

import (
	"encoding/json"
	"os"
	"path/filepath"

	"bcast/internal/fault"
	"bcast/internal/model"
	"bcast/internal/timeline"
	"bcast/internal/verify"
)

// writeJSON 以缩进格式确定性落盘任意可序列化结构。
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// TrackSummary 是时间线清单中单轨的有界统计（不逐条导出 30 万样本）。
type TrackSummary struct {
	TrackID    uint8  `json:"track_id"`
	Kind       string `json:"kind"`
	Samples    int    `json:"samples"`
	FirstPTSUS int64  `json:"first_pts_us"`
	LastEndUS  int64  `json:"last_end_us"`
	Wraps      int    `json:"wraps"`
	DriftPPM   int64  `json:"drift_ppm"`
	Timebase   uint64 `json:"timebase"`
}

// TimelineManifest 是可复核的时间线清单：逐轨摘要 + 全部缺口 + 故障计数。
type TimelineManifest struct {
	TotalSamples int            `json:"total_samples"`
	TotalGaps    int            `json:"total_gaps"`
	Tracks       []TrackSummary `json:"tracks"`
	Gaps         []model.Gap    `json:"gaps"`
	FaultCounts  map[string]int `json:"fault_counts"`
}

// writeManifests 落盘时间线清单与帧序列清单（确定性 JSON）。
func writeManifests(workDir string, tl *timeline.Timeline,
	refs []model.FrameRef, fc *fault.Collector) error {
	drifts := map[uint8]int64{}
	for _, d := range timeline.EstimateAll(tl) {
		drifts[d.TrackID] = d.PPM
	}
	sums := make([]TrackSummary, 0, len(tl.Plan.Tracks))
	for _, tp := range tl.Plan.Tracks {
		samples := tl.ByTrack[tp.ID]
		sum := TrackSummary{
			TrackID: tp.ID, Kind: tp.Kind.String(), Samples: len(samples),
			Timebase: tp.Timebase, DriftPPM: drifts[tp.ID],
		}
		if len(samples) > 0 {
			last := samples[len(samples)-1]
			sum.FirstPTSUS = samples[0].PTS
			sum.LastEndUS = last.PTS + last.Duration
			sum.Wraps = last.Wrap
		}
		sums = append(sums, sum)
	}
	counts := map[string]int{}
	for _, kind := range []string{
		fault.KindDrop, fault.KindDupe, fault.KindCorrupt,
		fault.KindChecksum, fault.KindTruncated, fault.KindWrap, fault.KindBoundary,
	} {
		if n := fc.CountByKind(kind); n > 0 {
			counts[kind] = n
		}
	}
	tm := TimelineManifest{
		TotalSamples: len(tl.Samples), TotalGaps: len(tl.Gaps),
		Tracks: sums, Gaps: tl.Gaps, FaultCounts: counts,
	}
	if err := writeJSON(filepath.Join(workDir, "timeline_manifest.json"), tm); err != nil {
		return err
	}
	frames := make([]verify.FrameEntry, 0, len(refs))
	for _, r := range refs {
		frames = append(frames, verify.FrameEntry{
			Index: r.Index, TimeUS: r.TimeUS, Path: r.Path, Sha256: r.Sha256,
		})
	}
	return writeJSON(filepath.Join(workDir, "frames_manifest.json"), frames)
}
