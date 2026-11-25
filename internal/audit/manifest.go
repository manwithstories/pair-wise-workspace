package audit

import "mediqc/internal/media"

// FileDigest identifies a product file in a manifest.
type FileDigest struct {
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// TraceStage links one trace ID through a subsystem boundary.
type TraceStage struct {
	Component string     `json:"component"`
	Code      media.Code `json:"code"`
	TraceID   string     `json:"trace_id"`
	Offset    int64      `json:"offset,omitempty"`
	Note      string     `json:"note,omitempty"`
}

// ChunkDigest records one checkpoint-backed fragment.
type ChunkDigest struct {
	ID       string `json:"id"`
	Index    int    `json:"index"`
	Bytes    int    `json:"bytes"`
	SHA256   string `json:"sha256"`
	Replayed bool   `json:"-"`
}

// CaseManifest is the independently verifiable record for one fixture case.
type CaseManifest struct {
	SchemaVersion int    `json:"schema_version"`
	Seed          int64  `json:"seed"`
	CaseID        string `json:"case_id"`
	Class         string `json:"class"`
	InputMode     string `json:"input_mode"`
	TraceID       string `json:"trace_id"`

	Input      FileDigest     `json:"input"`
	RTPInput   *FileDigest    `json:"rtp_input,omitempty"`
	OutputTS   *FileDigest    `json:"output_ts,omitempty"`
	OutputFMP4 *FileDigest    `json:"output_fmp4,omitempty"`
	Checkpoint FileDigest     `json:"checkpoint"`
	Chunks     []ChunkDigest  `json:"chunks,omitempty"`
	TraceChain []TraceStage   `json:"trace_chain,omitempty"`
	Errors     []*media.Error `json:"errors,omitempty"`

	Samples      []media.SampleRef `json:"samples,omitempty"`
	SampleCount  int               `json:"sample_count"`
	HashMatched  int               `json:"hash_matched"`
	HashTotal    int               `json:"hash_total"`
	HashMatchPct float64           `json:"hash_match_pct"`

	DurationMS        int64 `json:"duration_ms"`
	AuditedDurationMS int64 `json:"audited_duration_ms,omitempty"`
	SkewMS            int64 `json:"manifest_skew_ms"`
	TargetBitrateBPS  int64 `json:"bitrate_target_bps"`
	ActualBitrateBPS  int64 `json:"bitrate_actual_bps"`
	BitrateDeltaPPM   int64 `json:"bitrate_delta_ppm"`

	CCGaps           int    `json:"cc_gaps"`
	PSISwitches      int    `json:"psi_switches"`
	PCRWraps         int    `json:"pcr_wraps"`
	LateReplays      int    `json:"late_replays"`
	RTPDuplicates    int    `json:"rtp_duplicates,omitempty"`
	RTPOutsideWindow int    `json:"rtp_outside_window,omitempty"`
	UnfixableGaps    int    `json:"unfixable_gaps"`
	TimelineHash     string `json:"timeline_hash"`
	ReplayHash       string `json:"replay_hash"`
	ReplayHashEqual  bool   `json:"replay_sha256_equal"`
}

// SuiteManifest is the top-level deterministic batch manifest.
type SuiteManifest struct {
	SchemaVersion int   `json:"schema_version"`
	Seed          int64 `json:"seed"`

	Cases    int `json:"cases"`
	Clean    int `json:"clean"`
	Repaired int `json:"repaired"`
	Rejected int `json:"rejected"`

	CCGaps      int `json:"cc_gaps"`
	PSISwitches int `json:"psi_switches"`
	PCRWraps    int `json:"pcr_wraps"`
	LateReplays int `json:"late_replays"`

	RTPDuplicates    int `json:"rtp_duplicates,omitempty"`
	RTPOutsideWindow int `json:"rtp_outside_window,omitempty"`

	BitrateTargetBPS int64 `json:"bitrate_target_bps"`
	BitrateActualBPS int64 `json:"bitrate_actual_bps"`
	BitrateDeltaPPM  int64 `json:"bitrate_delta_ppm"`

	SampleHashMatch   string `json:"sample_hash_match"`
	SampleHashMatched int    `json:"sample_hash_matched"`
	SampleHashTotal   int    `json:"sample_hash_total"`
	ManifestSkewMS    int64  `json:"manifest_skew_ms"`
	ReplayHashEqual   bool   `json:"replay_sha256_equal"`

	CasesManifests []CaseManifest `json:"case_manifests"`
}

// VerifiedSuite pairs a parsed manifest with the hash of its exact JSON bytes.
type VerifiedSuite struct {
	Manifest     *SuiteManifest
	ManifestHash string
	ManifestPath string
}
