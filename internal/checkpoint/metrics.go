package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const metricsName = "metrics.json"

// MetricsSnapshot is the small rate document written next to the checkpoint
// so an independent verifier can report rates without rerunning the pipeline.
type MetricsSnapshot struct {
	RoundTripRate   float64 `json:"roundtrip_rate"`
	KeyframeHitRate float64 `json:"keyframe_hit_rate"`
}

// SaveMetrics writes the run rates once.
func SaveMetrics(dir string, m MetricsSnapshot) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCheckpointCorrupt, err)
	}
	return os.WriteFile(filepath.Join(dir, metricsName), b, 0o644)
}

// LoadMetrics reads the rate document, returning zero values when absent so
// verification still proceeds on older run directories.
func LoadMetrics(dir string) MetricsSnapshot {
	var m MetricsSnapshot
	if b, err := os.ReadFile(filepath.Join(dir, metricsName)); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// ShardHashes returns the recorded per-segment hashes in shard order.
func ShardHashes(dir string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, stateName))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCheckpointCorrupt, err)
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCheckpointCorrupt, err)
	}
	out := make([]string, len(st.Shards))
	for i, s := range st.Shards {
		out[i] = s.SHA256
	}
	return out, nil
}

// DigestBytes is the shared SHA-256 helper used by cross-run comparisons.
func DigestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
