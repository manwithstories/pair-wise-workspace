// Package report aggregates the numbers the acceptance run prints: end-to-end
// synchronization error, packet recovery rate, recovered ranges and processing
// latency percentiles. All durations are integer microseconds; percentiles use
// the nearest-rank method so repeated runs with the same input agree exactly.
package report

import (
	"sort"

	"archivearchivist/internal/model"
)

// Metrics is the machine-readable summary written to --json and printed in the
// terminal digest.
type Metrics struct {
	Tracks              int            `json:"tracks"`
	CorruptRanges       int            `json:"corrupt_ranges"`
	RecoveredRanges     int            `json:"recovered_ranges"`
	UnrecoverableRanges int            `json:"unrecoverable_ranges"`
	Wraps               int            `json:"wraps"`
	ConstantFPS         bool           `json:"constant_fps"`
	VFRTracks           int            `json:"variable_frame_rate_tracks"`
	SyncSamples         int            `json:"sync_samples"`
	SyncErrorP95US      int64          `json:"sync_error_p95_us"`
	SyncErrorMaxUS      int64          `json:"sync_error_max_us"`
	SnapErrorP95US      int64          `json:"keyframe_snap_p95_us"`
	SnapErrorMaxUS      int64          `json:"keyframe_snap_max_us"`
	PacketRecoveryRate  model.Rational `json:"packet_recovery_rate"`
	RecoveredPackets    int            `json:"recovered_packets"`
	TotalGap            int            `json:"total_gap"`
	LatePackets         int            `json:"late_packets"`
	Iterations          int            `json:"iterations"`
	LatencyP95US        int64          `json:"latency_p95_us"`
	LatencyMeanUS       int64          `json:"latency_mean_us"`
	BlockedBoundaries   int            `json:"blocked_boundaries"`
	RecoveredBoundaries int            `json:"recovered_boundaries"`
	CrossfadeSkewP95US  int64          `json:"crossfade_skew_p95_us"`
	DriftReviewTracks   []string       `json:"drift_review_tracks,omitempty"`
	Errors              []string       `json:"errors,omitempty"`
}

// Quantile returns the nearest-rank percentile of an unsorted sample set. p is
// clamped to (0,100]; an empty input yields zero.
func Quantile(xs []int64, p int) int64 {
	if len(xs) == 0 {
		return 0
	}
	if p <= 0 {
		p = 1
	}
	if p > 100 {
		p = 100
	}
	sorted := append([]int64(nil), xs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := (p*len(sorted) + 99) / 100 // ceil(p/100 * n)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// Mean returns the rounded arithmetic mean.
func Mean(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	var sum int64
	for _, x := range xs {
		sum += x
	}
	return sum / int64(len(xs))
}

// Abs returns the absolute value of a duration.
func Abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// FormatMillis renders microseconds as fixed 3-decimal milliseconds. It is a
// display helper only; no measurement depends on it.
func FormatMillis(us int64) string {
	neg := us < 0
	if neg {
		us = -us
	}
	whole := us / 1000
	frac := us % 1000
	out := itoa(whole) + "." + pad3(frac)
	if neg {
		return "-" + out
	}
	return out
}

// FormatRate renders a rational with three decimals.
func FormatRate(r model.Rational) string { return FormatMillis(r.Scale(1000)) }

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func pad3(v int64) string {
	var buf [3]byte
	for i := 2; i >= 0; i-- {
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[:])
}
