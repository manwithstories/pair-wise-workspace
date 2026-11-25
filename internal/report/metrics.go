// Package report aggregates run metrics, enforces thresholds, and
// writes summaries atomically so a failed run never leaves output that
// could be mistaken for a complete one.
package report

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Summary is the acceptance output for one run.
type Summary struct {
	AVSyncMaxMS       float64 `json:"av_sync_max_ms"`
	PacketRecoveryPct float64 `json:"packet_recovery_pct"`
	SeekP95MS         float64 `json:"seek_p95_ms"`
	RepairedSamples   int     `json:"repaired_samples"`
	InvalidSamples    int     `json:"invalid_samples"`
	IndexSHA256       string  `json:"index_sha256"`
}

// Thresholds are the quantitative acceptance limits.
type Thresholds struct {
	AVSyncMaxMS       float64
	PacketRecoveryPct float64
	SeekP95MS         float64
}

// DefaultThresholds are the acceptance limits from the specification.
var DefaultThresholds = Thresholds{AVSyncMaxMS: 20, PacketRecoveryPct: 95, SeekP95MS: 25}

// Exceeds returns descriptions of every threshold violation.
func (s Summary) Exceeds(t Thresholds) []string {
	var out []string
	if s.AVSyncMaxMS > t.AVSyncMaxMS {
		out = append(out, fmt.Sprintf("av_sync_max_ms %.3f > %.3f", s.AVSyncMaxMS, t.AVSyncMaxMS))
	}
	if s.PacketRecoveryPct < t.PacketRecoveryPct {
		out = append(out, fmt.Sprintf("packet_recovery_pct %.2f < %.2f", s.PacketRecoveryPct, t.PacketRecoveryPct))
	}
	if s.SeekP95MS > t.SeekP95MS {
		out = append(out, fmt.Sprintf("seek_p95_ms %.3f > %.3f", s.SeekP95MS, t.SeekP95MS))
	}
	return out
}

// SHA256Hex digests the concatenated parts (e.g. all index files).
func SHA256Hex(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Percentile returns the p-th percentile (0..100) of vals using the
// nearest-rank method. Empty input yields 0.
func Percentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	rank := int(p / 100 * float64(len(sorted)))
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

// AtomicWrite writes data to path via a temp file in the same directory
// followed by rename. On any failure the temp file is removed and path
// is left untouched.
func AtomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}
