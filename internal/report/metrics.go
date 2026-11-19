// Package report aggregates per-asset and batch metrics (AV sync error,
// packet recovery, latency percentiles) and renders them as JSON.
package report

import (
	"encoding/json"
	"io"
	"sort"
)

// Percentiles is a latency/error distribution summary.
type Percentiles struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	Max float64 `json:"max"`
}

// Percentile computes the p-th percentile (0..100) of values using linear
// interpolation on the sorted samples. Empty input yields 0.
func Percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	s := make([]float64, len(values))
	copy(s, values)
	sort.Float64s(s)
	if p <= 0 {
		return s[0]
	}
	if p >= 100 {
		return s[len(s)-1]
	}
	rank := p / 100 * float64(len(s)-1)
	lo := int(rank)
	frac := rank - float64(lo)
	return s[lo] + frac*(s[lo+1]-s[lo])
}

// Summarize builds Percentiles for values.
func Summarize(values []float64) Percentiles {
	return Percentiles{
		P50: Percentile(values, 50),
		P95: Percentile(values, 95),
		Max: Percentile(values, 100),
	}
}

// AssetReport is the per-asset processing record.
type AssetReport struct {
	Name         string  `json:"name"`
	Kind         string  `json:"kind"` // "hls", "dash", "rtp"
	Status       string  `json:"status"`
	Error        string  `json:"error,omitempty"`
	Segments     int     `json:"segments,omitempty"`
	Duplicates   int     `json:"duplicates,omitempty"`
	Conflicts    int     `json:"conflicts,omitempty"`
	Gaps         []int   `json:"gaps,omitempty"`
	Tracks       int     `json:"tracks,omitempty"`
	Samples      int     `json:"samples,omitempty"`
	AVSyncP95Ms  float64 `json:"av_sync_p95_ms,omitempty"`
	ProcessingMs float64 `json:"processing_ms"`
}

// Summary is the batch-level JSON report.
type Summary struct {
	Assets                   []AssetReport `json:"assets"`
	AVSyncErrorMs            Percentiles   `json:"av_sync_error_ms"`
	PacketRecoveryRate       float64       `json:"packet_recovery_rate"`
	PacketRecovery           *PacketStats  `json:"packet_recovery_detail,omitempty"`
	ProcessingP95Ms          float64       `json:"processing_p95_ms"`
	CheckpointResumeVerified bool          `json:"checkpoint_resume_verified"`
	CorruptCasesDetected     int           `json:"corrupt_cases_detected"`
	CorruptCasesTotal        int           `json:"corrupt_cases_total"`
	AssetsFailed             int           `json:"assets_failed"`
}

// PacketStats details the RTP recovery run.
type PacketStats struct {
	Received   int      `json:"received"`
	Duplicates int      `json:"duplicates"`
	Emitted    int      `json:"emitted"`
	Reordered  int      `json:"reordered"`
	Late       int      `json:"late,omitempty"`
	Lost       int      `json:"lost"`
	Gaps       []GapRef `json:"retryable_gaps,omitempty"`
	Origin     int64    `json:"origin"`
}

// GapRef is one retryable burst-loss gap, relative to the stream origin.
type GapRef struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
	Len   int   `json:"len"`
}

// WriteJSON renders the summary.
func WriteJSON(w io.Writer, s *Summary) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}
