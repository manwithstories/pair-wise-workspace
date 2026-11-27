// Package pipeline orchestrates parse, recover, repair and index stages
// in track-graph order, persisting checkpoints after each stage.
package pipeline

import (
	"fmt"

	"avrepair/internal/index"
	"avrepair/internal/journal"
	"avrepair/internal/manifest"
	"avrepair/internal/media"
)

// Stage names, ordered.
const (
	StageParse    = "parse"
	StageRecover  = "recover"
	StageRepair   = "repair"
	StageIndex    = "index"
	StageFinalize = "finalize"
)

// Options configures one repair run.
type Options struct {
	Input       string
	StateDir    string
	OutputDir   string
	InterruptAt string
	Resume      bool
}

// Report is the terminal output of a run.
type Report struct {
	Input            string   `json:"input"`
	StateDir         string   `json:"state_dir"`
	OutputDir        string   `json:"output_dir"`
	Manifest         string   `json:"manifest"`
	InputDigest      string   `json:"input_digest"`
	Tracks           int      `json:"tracks"`
	Segments         int      `json:"segments"`
	SegmentsValid    int      `json:"segments_valid"`
	Quarantined      int      `json:"quarantined_segments"`
	IndexEntries     int      `json:"seek_index_entries"`
	AVSyncMaxMS      float64  `json:"av_sync_max_ms"`
	LossRecoveryPct  float64  `json:"loss_recovery_pct"`
	P95SegmentMS     float64  `json:"p95_segment_ms"`
	ReusedStages     int      `json:"reused_stages"`
	ReusedNames      []string `json:"reused_stage_names,omitempty"`
	CompletedStages  []string `json:"completed_stages"`
	ResultSHA256     string   `json:"result_sha256"`
	IndexPath        string   `json:"index_path"`
	Interrupted      bool     `json:"interrupted"`
	Resumed          bool     `json:"resumed"`
	Warnings         []string `json:"warnings,omitempty"`
	Corrections      int      `json:"corrections"`
	RecoveredPackets int      `json:"recovered_packets"`
	Unrecovered      int      `json:"unrecovered_groups"`
}

// ErrInterrupted reports a requested clean stop after a stage.
var ErrInterrupted = fmt.Errorf("pipeline: interrupted after stage")

type runContext struct {
	opt       Options
	store     *journal.Store
	digest    string
	report    *Report
	man       *manifest.Manifest
	timeline  *media.Timeline
	index     *index.Index
	captures  map[string]*capture
	hash      string
	segTimes  []float64
	recordSeq int
}
