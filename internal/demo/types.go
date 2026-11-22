// Package demo wires the pipeline nodes used by the mediacore demo
// command: parse, timeline, media index, cut, silence, fingerprint,
// bitrate, RTP overflow check and report.
package demo

import (
	"mediacore/internal/audio"
	"mediacore/internal/clock"
	"mediacore/internal/fingerprint"
	"mediacore/internal/manifest"
	"mediacore/internal/media"
	"mediacore/internal/timeline"
)

// Version is the algorithm version stamped into idempotency keys and
// provenance records.
const Version = "1.0.0"

// Params are the digest-relevant run parameters.
type Params struct {
	Seed     int64
	Dir      string // fixture directory
	CutStart float64
	CutEnd   float64
}

// DefaultParams returns the standard demo configuration.
func DefaultParams(seed int64, dir string) Params {
	return Params{Seed: seed, Dir: dir, CutStart: 1.5, CutEnd: 5.5}
}

// Summary is the stable demo report. Re-running the same input and
// params must reproduce it exactly.
type Summary struct {
	TargetKbps             float64 `json:"target_bitrate_kbps"`
	ActualKbps             float64 `json:"actual_bitrate_kbps"`
	DeviationPct           float64 `json:"bitrate_deviation_pct"`
	SilenceSegments        int     `json:"silence_segments"`
	DuplicateSegments      int     `json:"duplicate_segments"`
	QualitySamples         int     `json:"quality_samples"`
	ManifestAlignmentMs    float64 `json:"manifest_alignment_ms"`
	RecoveredNodes         int     `json:"recovered_nodes"`
	FirstMissingDependency string  `json:"first_missing_dependency"`
	DuplicateCommits       int     `json:"duplicate_commits"`
}

// ---- node payloads (JSON-checkpointed) ----

type HlsResult struct {
	Tracks []manifest.Track `json:"tracks"`
	Digest string           `json:"digest"`
}

type DashResult struct {
	Tracks []manifest.Track `json:"tracks"`
	Digest string           `json:"digest"`
}

type TimelineResult struct {
	AlignmentMs float64                  `json:"alignment_ms"`
	Tracks      []timeline.TrackTimeline `json:"tracks"`
}

type SegmentIndex struct {
	Gap        bool           `json:"gap"`
	Truncated  bool           `json:"truncated"`
	DecodeTime uint64         `json:"decode_time"`
	Samples    []media.Sample `json:"samples,omitempty"`
}

type MediaIndexResult struct {
	Tracks map[string][]SegmentIndex `json:"tracks"`
	Issues []string                  `json:"issues"`
}

type CutResult struct {
	TrackID          string     `json:"track_id"`
	StartRequested   clock.Tick `json:"start_requested"`
	StartActual      clock.Tick `json:"start_actual"`
	EndRequested     clock.Tick `json:"end_requested"`
	EndActual        clock.Tick `json:"end_actual"`
	DeviationStartMs float64    `json:"deviation_start_ms"`
	DeviationEndMs   float64    `json:"deviation_end_ms"`
	SegmentRange     [2]int     `json:"segment_range"`
}

type SilenceResult struct {
	Segments []audio.Segment `json:"segments"`
	Partial  bool            `json:"partial"`
}

type FingerprintResult struct {
	Count      int                      `json:"count"`
	Duplicates []fingerprint.Duplicate  `json:"duplicates"`
}

type BitrateResult struct {
	TargetKbps   float64 `json:"target_kbps"`
	ActualKbps   float64 `json:"actual_kbps"`
	DeviationPct float64 `json:"deviation_pct"`
	Samples      int     `json:"samples"`
}

type RtpResult struct {
	Packets   int  `json:"packets"`
	Monotonic bool `json:"monotonic"`
}

type ReportResult struct {
	Summary Summary `json:"summary"`
}
