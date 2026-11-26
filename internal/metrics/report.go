// Package metrics aggregates run metrics and renders the terminal
// verification report.
package metrics

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Report aggregates everything the verifier prints.
type Report struct {
	AVSyncMaxMs     int64 // worst audio/video skew across cameras
	LossRecovered   int64 // packets healed from the retry cache
	LossTotal       int64 // recovered + lost packets
	PeakBufferBytes int64
	AlignedSegments int
	CleanHash       string
	ReplayHash      string
	SkippedRecords  int
	Resumed         bool

	lat []int64 // per-event pipeline latency samples (ms)
}

// Observe records one pipeline latency sample in milliseconds.
func (r *Report) Observe(ms int64) { r.lat = append(r.lat, ms) }

// P95 returns the 95th-percentile latency in milliseconds.
func (r *Report) P95() int64 {
	if len(r.lat) == 0 {
		return 0
	}
	s := append([]int64(nil), r.lat...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[(len(s)*95-1)/100]
}

// LossRecoveryPct is the share of gaps healed from the retry cache.
func (r *Report) LossRecoveryPct() float64 {
	if r.LossTotal == 0 {
		return 100
	}
	return 100 * float64(r.LossRecovered) / float64(r.LossTotal)
}

// Hash returns the sha256 hex digest of a manifest.
func Hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// String renders the report as key: value lines for the terminal.
func (r *Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "av_sync_max_ms: %d\n", r.AVSyncMaxMs)
	fmt.Fprintf(&b, "loss_recovery_pct: %.2f\n", r.LossRecoveryPct())
	fmt.Fprintf(&b, "p95_ms: %d\n", r.P95())
	fmt.Fprintf(&b, "peak_buffer_bytes: %d\n", r.PeakBufferBytes)
	fmt.Fprintf(&b, "aligned_segments: %d\n", r.AlignedSegments)
	fmt.Fprintf(&b, "clean_hash: %s\n", r.CleanHash)
	fmt.Fprintf(&b, "replay_hash: %s\n", r.ReplayHash)
	fmt.Fprintf(&b, "hash_match: %t\n", r.CleanHash == r.ReplayHash)
	fmt.Fprintf(&b, "skipped_records: %d\n", r.SkippedRecords)
	fmt.Fprintf(&b, "resumed: %t\n", r.Resumed)
	return b.String()
}
