// Package pipeline orchestrates demux, recovery, transport simulation and
// rendering, then reports synchronisation, recovery rate and latency.
package pipeline

import (
	"encoding/json"
	"io"
	"sort"

	"mediasim/internal/clock"
)

// Exit codes are part of the contract.
const (
	ExitOK      = 0
	ExitError   = 1
	ExitPartial = 2
)

// Gates are the acceptance thresholds printed by the demo.
type Gates struct {
	AVSyncP95Ms    float64 `json:"av_sync_p95_ms"`
	PacketRecovery float64 `json:"packet_recovery_rate"`
	LatencyP95Ms   float64 `json:"latency_p95_ms"`
	ExpectedFrames int     `json:"expected_frames"`
}

// DefaultGates matches the acceptance criteria.
func DefaultGates() Gates {
	return Gates{AVSyncP95Ms: 20, PacketRecovery: 0.90, LatencyP95Ms: 150}
}

// Report is the reproducible output of one run.
type Report struct {
	Fixture          string         `json:"fixture"`
	Seed             int64          `json:"seed"`
	Frames           int            `json:"frames"`
	FrameHashes      int            `json:"frame_sha256_count"`
	RecoveredPrefix  int            `json:"recovered_prefix_frames"`
	RecoveredIndexed int            `json:"recovered_indexed_frames"`
	CorruptRanges    int            `json:"corrupt_ranges"`
	AVSyncP95Ms      float64        `json:"av_sync_p95_ms"`
	AVSyncMaxMs      float64        `json:"av_sync_max_ms"`
	PacketRecovery   float64        `json:"packet_recovery_rate"`
	LatencyP95Ms     float64        `json:"latency_p95_ms"`
	RTP              RTPReport      `json:"rtp"`
	Recovery         RecoveryReport `json:"recovery"`
	AudioEdits       []AudioEdit    `json:"audio_edits,omitempty"`
	Gates            GateResults    `json:"gates"`
	Errors           []ErrorEntry   `json:"errors"`
	ExitCode         int            `json:"exit_code"`
}

// RTPReport summarises the transport simulation.
type RTPReport struct {
	Sent          int `json:"sent"`
	Received      int `json:"received"`
	Duplicate     int `json:"duplicate"`
	Reordered     int `json:"reordered"`
	Late          int `json:"late"`
	Lost          int `json:"lost"`
	Delivered     int `json:"delivered"`
	MaxBuffered   int `json:"max_buffered"`
	KeyframeWaits int `json:"keyframe_waits"`
	Expired       int `json:"expired"`
	Repaired      int `json:"repaired_from_origin"`
	Unrepaired    int `json:"unrepaired"`
}

// RecoveryReport summarises parse and scan recovery.
type RecoveryReport struct {
	Indexed        bool `json:"indexed"`
	ScannedSamples int  `json:"scanned_samples"`
	CommittedScan  int  `json:"committed_scan"`
	Phantom        int  `json:"phantom"`
	SkippedRanges  int  `json:"skipped_ranges"`
	Resynced       bool `json:"resynced"`
}

// GateResults records pass/fail per acceptance threshold.
type GateResults struct {
	AVSyncOK   bool `json:"av_sync_ok"`
	RecoveryOK bool `json:"recovery_ok"`
	LatencyOK  bool `json:"latency_ok"`
	FramesOK   bool `json:"frames_ok"`
	HashesOK   bool `json:"hashes_ok"`
}

// ErrorEntry is the flattened, reviewable form of a classified error.
type ErrorEntry struct {
	Category string `json:"category"`
	Span     string `json:"span"`
	Recovery string `json:"recovery"`
	Message  string `json:"message"`
}

// ErrorList converts the classified list into report entries.
func errorEntries(l *clock.List) []ErrorEntry {
	out := make([]ErrorEntry, 0, l.Len())
	for _, e := range l.Items() {
		out = append(out, ErrorEntry{
			Category: string(e.Category), Span: e.Span.String(),
			Recovery: string(e.Recovery), Message: e.Message,
		})
	}
	return out
}

// Percentile returns the p-th percentile (0<p<=1) using nearest-rank on a
// copy, so the input order is irrelevant.
func Percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	if p <= 0 {
		return minFloat(v)
	}
	if p >= 1 {
		return maxFloat(v)
	}
	cp := append([]float64(nil), v...)
	sort.Float64s(cp)
	rank := int(p*float64(len(cp)) + 0.999999)
	if rank < 1 {
		rank = 1
	}
	if rank > len(cp) {
		rank = len(cp)
	}
	return cp[rank-1]
}

func minFloat(v []float64) float64 {
	m := v[0]
	for _, x := range v[1:] {
		if x < m {
			m = x
		}
	}
	return m
}

func maxFloat(v []float64) float64 {
	m := v[0]
	for _, x := range v[1:] {
		if x > m {
			m = x
		}
	}
	return m
}

// WriteJSON emits the report as indented JSON with a trailing newline.
func (r *Report) WriteJSON(w io.Writer) error {
	if r.Errors == nil {
		r.Errors = []ErrorEntry{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
