// Package pipeline orchestrates the incremental processing stages
// (scan -> clock -> compose -> render), writing stage summaries into
// the content-addressed cache and atomic checkpoints so an
// interrupted run can resume by recomputing only unfinished or
// invalid stages.
package pipeline

import (
	"encoding/json"

	"mediatimeline/internal/timeline"
)

// Stage names in execution order.
const (
	StageScan    = "scan"
	StageClock   = "clock"
	StageCompose = "compose"
	StageRender  = "render"
)

// StageOrder is the pipeline execution order.
var StageOrder = []string{StageScan, StageClock, StageCompose, StageRender}

// RunParams are the processing parameters recorded in run.json.
type RunParams struct {
	Seed        int64 `json:"seed"`
	Tracks      int   `json:"tracks"`
	Frames      int   `json:"frames"`
	MaxDriftPPM int64 `json:"max_drift_ppm"`
}

// Canonical returns the deterministic parameter string used in keys.
func (p RunParams) Canonical() string {
	b, _ := json.Marshal(p)
	return string(b)
}

// Options controls one pipeline run.
type Options struct {
	Dir            string
	InterruptAfter int // stop after this many completed stages; <=0: run all
}

// StageInfo reports how one stage was served.
type StageInfo struct {
	Name           string `json:"name"`
	CacheHit       bool   `json:"cache_hit"`
	Recomputed     bool   `json:"recomputed"`
	DurationMicros int64  `json:"duration_micros"`
}

// CorruptInfo reports one isolated corrupt track.
type CorruptInfo struct {
	Track  string `json:"track"`
	Offset int64  `json:"offset"`
	Reason string `json:"reason"`
}

// Report is the outcome of a pipeline run.
type Report struct {
	Params           RunParams
	Stages           []StageInfo
	Interrupted      bool
	Corrupt          []CorruptInfo
	TrackReports     []timeline.TrackReport
	CacheHits        int
	RecomputedStages int
	SummarySHA256    string
	ManifestDigest   string
	CompletedMicros  int64 // original durations of checkpoint-reused stages
	RunMicros        int64 // wall time of this run
	Notes            []string
}

// TrackSpec describes one generated input track.
type TrackSpec struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"` // "es" or "rtp"
	Codec    string `json:"codec"`
	Stream   string `json:"stream"`
	Index    string `json:"index,omitempty"`
	Anchors  string `json:"anchors,omitempty"`
	TBNum    int64  `json:"tb_num"`
	TBDen    int64  `json:"tb_den"`
	TsBits   uint   `json:"ts_bits"`
	DriftPPM int64  `json:"drift_ppm"`
}

// FixtureMeta is the fixture manifest stored as inputs/tracks.json.
type FixtureMeta struct {
	Params        RunParams   `json:"params"`
	Tracks        []TrackSpec `json:"tracks"`
	CorruptTrack  string      `json:"corrupt_track,omitempty"`
	CorruptOffset int64       `json:"corrupt_offset"`
}

// --- stage payloads (JSON-serialisable, cached between runs) ---

type scannedSample struct {
	Seq      int    `json:"seq"`
	Offset   int64  `json:"offset"`
	Length   int64  `json:"length"`
	Keyframe bool   `json:"keyframe"`
	RawTS    uint64 `json:"raw_ts"`
	Digest   string `json:"digest"`
}

type scannedTrack struct {
	ID           string          `json:"id"`
	Codec        string          `json:"codec"`
	TBNum        int64           `json:"tb_num"`
	TBDen        int64           `json:"tb_den"`
	TsBits       uint            `json:"ts_bits"`
	StreamDigest string          `json:"stream_digest"`
	Samples      []scannedSample `json:"samples"`
}

type scanResult struct {
	Tracks  []scannedTrack `json:"tracks"`
	Corrupt []CorruptInfo  `json:"corrupt"`
}

type alignedTrack struct {
	ID       string               `json:"id"`
	Priority int                  `json:"priority"`
	Aligned  bool                 `json:"aligned"`
	Samples  []timeline.SampleRef `json:"samples"`
}

type clockResult struct {
	Tracks []alignedTrack `json:"tracks"`
}

type composeResult struct {
	Frames  []timeline.Frame       `json:"frames"`
	Reports []timeline.TrackReport `json:"reports"`
}

type renderResult struct {
	Summary        string `json:"summary"`
	ManifestDigest string `json:"manifest_digest"`
	FrameCount     int    `json:"frame_count"`
}
