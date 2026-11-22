package model

import (
	"sort"
)

// Metrics aggregates the pipeline output required by the demonstration
// scenarios. All numeric fields are computed deterministically from the
// processed event stream.
type Metrics struct {
	Scenario string `json:"scenario"`
	// AVSyncMSP95 is the 95th percentile of audio/video drift in ms.
	AVSyncMSP95 int64 `json:"av_sync_ms_p95"`
	// LossRecoveryRate is recovered/(recovered+unrecoverable) in 0..1.
	LossRecoveryRate float64 `json:"loss_recovery_rate"`
	// LatencyMSP95 is the 95th percentile of arrival-to-output delay in ms.
	LatencyMSP95    int64 `json:"latency_ms_p95"`
	DuplicateCount  int   `json:"duplicate_count"`
	ReorderedCount  int   `json:"reordered_count"`
	LateReplayCount int   `json:"late_replay_count"`
	// UnrecoverableSamples counts individual interleaved sample values that
	// no recovery policy could reconstruct.
	UnrecoverableSamples int `json:"unrecoverable_samples"`
	// RealSamples / RecoveredSamples break down audio sample provenance.
	RealSamples      int `json:"real_samples"`
	RecoveredSamples int `json:"recovered_samples"`
	// ConflictCount counts same-sequence/different-payload packets.
	ConflictCount int `json:"conflict_count"`
	// DroppedExpired counts events removed to bound buffer memory.
	DroppedExpired int `json:"dropped_expired"`
	// ResultHash pins full processing determinism.
	ResultHash string `json:"result_hash"`
}

// Percentile returns the p-th percentile (0..100) of a copied-and-sorted
// sample slice using nearest-rank on the sorted values.
func Percentile(values []int64, p float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	rank := int(float64(len(sorted))*p/100.0 + 0.999999)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}
