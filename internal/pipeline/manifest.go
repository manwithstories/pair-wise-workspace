package pipeline

import (
	"encoding/json"

	"mediad/internal/checkpoint"
	"mediad/internal/dedup"
)

// ManifestSegment is one complete fMP4 segment entry.
type ManifestSegment struct {
	ID             int    `json:"id"`
	File           string `json:"file"`
	SHA256         string `json:"sha256"`
	Size           int64  `json:"size"`
	StartNS        int64  `json:"start_ns"`
	EndNS          int64  `json:"end_ns"`
	DurationNS     int64  `json:"duration_ns"`
	AUStart        int    `json:"au_start"`
	AUEndExclusive int    `json:"au_end_exclusive"`
	AlignErrorNS   int64  `json:"align_error_ns"`
}

// Manifest is the deterministic reusable-segment manifest.
type Manifest struct {
	Version          int               `json:"version"`
	Codec            string            `json:"codec"`
	Seed             int64             `json:"seed"`
	TargetBPS        int               `json:"target_bps"`
	ActualBPS        int64             `json:"actual_bps"`
	DeltaPct         float64           `json:"delta_pct"`
	DriftPPM         int               `json:"drift_ppm"`
	TimescaleNS      uint32            `json:"timescale_ns"`
	InitFile         string            `json:"init_file"`
	Segments         []ManifestSegment `json:"segments"`
	TotalAUs         int               `json:"total_aus"`
	CompleteAUs      int               `json:"complete_aus"`
	BadAUs           int               `json:"bad_aus"`
	CompleteAUPct    float64           `json:"complete_au_pct"`
	KeyframeCoverage float64           `json:"keyframe_coverage_pct"`
	SilenceNS        int64             `json:"silence_ns"`
	UnknownNS        int64             `json:"unknown_ns"`
	DuplicateAUs     int64             `json:"duplicate_aus"`
	DupSavedBytes    int64             `json:"dup_saved_bytes"`
	AlignMaxAbsNS    int64             `json:"align_max_abs_ns"`
	AlignMaxMS       float64           `json:"align_max_ms"`
	RTPDuplicates    int64             `json:"rtp_duplicates"`
	PausedReads      int64             `json:"-"`
	FirstErrorByte   int64             `json:"first_error_byte"`
}

// Save writes the manifest atomically and returns its file hash.
func (m *Manifest) Save(path string) (string, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	b = append(b, '\n')
	if err := checkpoint.AtomicWriteFile(path, b, 0o644); err != nil {
		return "", err
	}
	return dedup.ManifestHash(b), nil
}
