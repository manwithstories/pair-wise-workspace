// Package metrics aggregates the counters produced by every pipeline stage
// into the single acceptance report printed by the CLI.
package metrics

import (
	"sort"
	"strconv"
	"strings"

	"avcheck/internal/flow"
	"avcheck/internal/recovery"
)

// Thresholds encodes the acceptance gates from the specification.
type Thresholds struct {
	MaxSyncErrorSamples int64
	MinRecoveryRate     float64
	MaxP95LatencyMS     float64
	MaxQueueBytes       int
}

// DefaultThresholds are the acceptance values for a healthy run.
func DefaultThresholds(queueBytes int) Thresholds {
	return Thresholds{
		MaxSyncErrorSamples: 1,
		MinRecoveryRate:     0.90,
		MaxP95LatencyMS:     200,
		MaxQueueBytes:       queueBytes,
	}
}

// Report is the final, serializable summary of one pipeline run.
type Report struct {
	SyncErrorSamples   int64
	LossRecoveryRate   float64
	P95LatencyMS       float64
	PeakQueueBytes     int
	FrameCount         int
	FrameHash          uint64
	BackpressureEvents int64
	AcceptedBlocks     int64
	LostBlocks         int64
	// TrackSamples maps track ID to emitted sample frames.
	TrackSamples map[uint32]int
}

// Latency is a collector for per-block virtual latency observations (ms).
type Latency struct {
	samples []float64
}

// Add records one latency observation.
func (l *Latency) Add(ms float64) {
	l.samples = append(l.samples, ms)
}

// P95 returns the 95th percentile using nearest-rank on sorted observations.
func (l *Latency) P95() float64 {
	if len(l.samples) == 0 {
		return 0
	}
	sorted := append([]float64(nil), l.samples...)
	sort.Float64s(sorted)
	rank := int(0.95*float64(len(sorted)) + 0.999999) // ceil(0.95*N)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// Build assembles a Report from stage-level counters.
func Build(syncErr int64, rec recovery.Stats, q flow.Snapshot, lat *Latency,
	frameCount int, frameHash uint64, trackSamples map[uint32]int,
	lateBlocks int64) Report {
	totalMissing := rec.Interpolated + rec.SilenceFilled
	var rate float64
	if totalMissing > 0 {
		rate = float64(rec.Interpolated) / float64(totalMissing)
	} else {
		rate = 1.0 // nothing was lost: perfect recovery by definition
	}
	return Report{
		SyncErrorSamples:   syncErr,
		LossRecoveryRate:   rate,
		P95LatencyMS:       lat.P95(),
		PeakQueueBytes:     q.PeakBytes,
		FrameCount:         frameCount,
		FrameHash:          frameHash,
		BackpressureEvents: q.BackpressureEv,
		AcceptedBlocks:     q.Enqueued,
		LostBlocks:         lateBlocks,
		TrackSamples:       trackSamples,
	}
}

// Pass evaluates the report against acceptance thresholds.
func (r Report) Pass(t Thresholds) bool {
	return r.SyncErrorSamples <= t.MaxSyncErrorSamples &&
		r.LossRecoveryRate >= t.MinRecoveryRate &&
		r.P95LatencyMS <= t.MaxP95LatencyMS &&
		r.PeakQueueBytes <= t.MaxQueueBytes &&
		r.LostBlocks == 0
}

// Line renders the canonical key=value output consumed by acceptance.
func (r Report) Line() string {
	var b strings.Builder
	b.WriteString("sync_error_samples=")
	b.WriteString(itoa(r.SyncErrorSamples))
	b.WriteString(" loss_recovery_rate=")
	b.WriteString(formatRate(r.LossRecoveryRate))
	b.WriteString(" p95_latency_ms=")
	b.WriteString(formatFloat(r.P95LatencyMS))
	b.WriteString(" peak_queue_bytes=")
	b.WriteString(itoa(int64(r.PeakQueueBytes)))
	b.WriteString(" frame_count=")
	b.WriteString(itoa(int64(r.FrameCount)))
	b.WriteString(" frame_hash=0x")
	b.WriteString(hex64(r.FrameHash))
	return b.String()
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func formatRate(v float64) string {
	return strconv.FormatFloat(v, 'f', 3, 64)
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', 3, 64)
}

func hex64(n uint64) string {
	const digits = "0123456789abcdef"
	var buf [16]byte
	for i := 15; i >= 0; i-- {
		buf[i] = digits[n&0xF]
		n >>= 4
	}
	return string(buf[:])
}
