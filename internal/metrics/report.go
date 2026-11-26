// Package metrics derives the acceptance metrics from pipeline results and a
// reference video timeline: A/V sync error, packet loss recovery rate,
// end-to-end P95 latency and bounded queue peak occupancy.
package metrics

import (
	"math"
	"sort"

	"mediamix/internal/clock"
)

// Thresholds from the delivery acceptance criteria.
const (
	MaxAVSyncMS          = 5.0
	MinRecoveryPct       = 95.0
	MaxP95LatencyMS      = 250.0
	MaxQueuePeakBytes    = 8 << 20 // 8 MiB
)

// Sample is one measured stage traversal.
type Sample struct {
	Seq       uint16
	ArrivalMS float64 // wall-clock ms when the packet entered ingest
	EmittedMS float64 // wall-clock ms when its audio was committed
	MediaTick clock.Tick
	VideoTick clock.Tick // corresponding reference video timeline tick
}

// Report is the serialisable acceptance report for one case.
type Report struct {
	Case            string  `json:"case"`
	AVSyncMS        float64 `json:"av_sync_ms"`
	LossRecoveryPct float64 `json:"loss_recovery_pct"`
	P95LatencyMS    float64 `json:"p95_latency_ms"`
	QueueBytesPeak  int     `json:"queue_bytes_peak"`
	OutputSHA256    string  `json:"output_sha256"`
}

// Latencies returns the per-sample end-to-end latency in milliseconds.
func latencyMS(samples []Sample) []float64 {
	out := make([]float64, 0, len(samples))
	for _, s := range samples {
		if s.EmittedMS >= s.ArrivalMS {
			out = append(out, s.EmittedMS-s.ArrivalMS)
		}
	}
	return out
}

// Percentile returns the p-th percentile (0..100) using nearest-rank.
func Percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	cp := append([]float64(nil), values...)
	sort.Float64s(cp)
	rank := int(math.Ceil(p/100*float64(len(cp)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(cp) {
		rank = len(cp) - 1
	}
	return cp[rank]
}

// AVSync computes the worst absolute A/V timing error across samples, in
// milliseconds, by comparing each audio media tick to its video reference.
func AVSync(samples []Sample) float64 {
	var worst float64
	for _, s := range samples {
		d := math.Abs((s.MediaTick - s.VideoTick).Millis())
		if d > worst {
			worst = d
		}
	}
	return worst
}

// RecoveryRate converts lost and concealed packet counts into a percentage.
// Lost is the number of detected gaps; concealed is how many were filled by
// PLC. A truncated tail that cannot be concealed counts against recovery.
func RecoveryRate(lost, concealed int64) float64 {
	if lost <= 0 {
		return 100
	}
	if concealed > lost {
		concealed = lost
	}
	return float64(concealed) * 100 / float64(lost)
}

// Build assembles a Report from raw measurements and pipeline counters.
// lostPackets is the number of sequence numbers the ingest declared lost;
// recoveredGaps is the number of gaps PLC successfully filled. A truncated
// tail that cannot be concealed naturally lowers the recovery percentage.
func Build(caseName string, samples []Sample, lostPackets, recoveredGaps int64,
	queuePeak int, sha string) Report {

	lat := latencyMS(samples)
	return Report{
		Case:            caseName,
		AVSyncMS:        round2(AVSync(samples)),
		LossRecoveryPct: round2(RecoveryRate(lostPackets, recoveredGaps)),
		P95LatencyMS:    round2(Percentile(lat, 95)),
		QueueBytesPeak:  queuePeak,
		OutputSHA256:    sha,
	}
}

// Verdict explains which acceptance thresholds, if any, a report violates.
type Verdict struct {
	OK       bool
	Reasons  []string
}

// Evaluate checks a report against every acceptance threshold.
func Evaluate(r Report) Verdict {
	v := Verdict{OK: true}
	if r.AVSyncMS > MaxAVSyncMS {
		v.OK = false
		v.Reasons = append(v.Reasons, "av_sync_ms exceeds 5 ms")
	}
	if r.LossRecoveryPct < MinRecoveryPct {
		v.OK = false
		v.Reasons = append(v.Reasons, "loss_recovery_pct below 95%")
	}
	if r.P95LatencyMS > MaxP95LatencyMS {
		v.OK = false
		v.Reasons = append(v.Reasons, "p95_latency_ms exceeds 250 ms")
	}
	if r.QueueBytesPeak > MaxQueuePeakBytes {
		v.OK = false
		v.Reasons = append(v.Reasons, "queue_bytes_peak exceeds 8 MiB")
	}
	return v
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
