// Package metrics aggregates deterministic broadcast QC metrics.
package metrics

import (
	"math"
	"sort"

	"bcast/internal/boxes"
	"bcast/internal/clock"
	"bcast/internal/media"
)

// Report contains the terminal and JSON metrics emitted by a batch run.
type Report struct {
	Status              string  `json:"status"`
	TargetKBPS          int     `json:"target_kbps"`
	ActualKBPS          float64 `json:"actual_kbps"`
	BitrateDeltaPct     float64 `json:"bitrate_delta_pct"`
	SampleCount         int     `json:"sample_count"`
	UniqueFragmentCount int     `json:"unique_fragment_count"`
	SampleDurP50MS      float64 `json:"sample_dur_p50_ms"`
	SampleDurP95MS      float64 `json:"sample_dur_p95_ms"`
	SampleDurP99MS      float64 `json:"sample_dur_p99_ms"`
	SyncPointCount      int     `json:"sync_point_count"`
	SyncIntervalP50MS   float64 `json:"sync_interval_p50_ms"`
	SyncIntervalP95MS   float64 `json:"sync_interval_p95_ms"`
	ManifestSkewMS      float64 `json:"manifest_skew_ms"`
	SeekOK              int     `json:"seek_ok"`
	SeekTotal           int     `json:"seek_total"`
	MissingCount        int     `json:"missing_count"`
	DuplicateCount      int     `json:"duplicate_count"`
	OutOfOrderCount     int     `json:"out_of_order_count"`
	IsolatedCount       int     `json:"isolated_count"`
	ClockDriftPPM       float64 `json:"clock_drift_ppm"`
	FirstErrorByte      int64   `json:"first_error_byte,omitempty"`
	Rollback            int     `json:"rollback,omitempty"`
	Recovered           int     `json:"recovered,omitempty"`
	Committed           int     `json:"committed,omitempty"`
	SHA256              string  `json:"sha256,omitempty"`
	Generation          string  `json:"generation,omitempty"`
}

// Summarize computes metrics from scan and repaired timelines.
func Summarize(scan *boxes.BatchReport, tl *clock.Result, targetKBPS int) *Report {
	r := &Report{Status: "ok", TargetKBPS: targetKBPS}
	if scan != nil {
		r.MissingCount = scan.Sequences.MissingCount()
		r.DuplicateCount = scan.Sequences.DuplicateCount()
		r.OutOfOrderCount = len(scan.Sequences.OutOfOrder)
		r.IsolatedCount = len(scan.Isolated)
		if scan.Fatal != nil {
			r.FirstErrorByte = scan.Fatal.Offset
		}
	}
	if tl == nil {
		return r
	}

	var payloadBytes int64
	var durationsMS []float64
	var syncTimes []int64
	minStart := int64(math.MaxInt64)
	maxEnd := int64(math.MinInt64)
	var driftSum float64
	var driftN int
	for _, tt := range tl.Tracks {
		driftSum += tt.DriftPPM
		driftN++
		if tt.StartShared < minStart {
			minStart = tt.StartShared
		}
		if tt.EndShared > maxEnd {
			maxEnd = tt.EndShared
		}
		tb := media.TrackTimebase(tt.Timescale)
		sharedTB := tl.SharedBase
		for _, ft := range tt.Fragments {
			for _, smp := range ft.Samples {
				r.SampleCount++
				payloadBytes += int64(smp.Size)
				if ticks, err := media.ConvertTicks(int64(smp.Duration), tb, sharedTB); err == nil {
					durationsMS = append(durationsMS, sharedTB.Milliseconds(ticks))
				}
			}
			for _, idx := range ft.SyncIndexes {
				if idx >= 0 && idx < len(ft.Samples) {
					syncTimes = append(syncTimes, ft.Samples[idx].SharedDTS)
				}
			}
		}
	}
	r.UniqueFragmentCount = len(tl.Fragments)
	if driftN > 0 {
		r.ClockDriftPPM = math.Round(driftSum/float64(driftN)*100) / 100
	}
	if minStart != math.MaxInt64 && maxEnd > minStart {
		durationSec := tl.SharedBase.Seconds(maxEnd - minStart)
		if durationSec > 0 {
			r.ActualKBPS = float64(payloadBytes) * 8 / durationSec / 1000
		}
	}
	if targetKBPS > 0 && r.ActualKBPS > 0 {
		r.BitrateDeltaPct = math.Abs(r.ActualKBPS-float64(targetKBPS)) / float64(targetKBPS) * 100
	}
	r.SampleDurP50MS = percentile(durationsMS, .50)
	r.SampleDurP95MS = percentile(durationsMS, .95)
	r.SampleDurP99MS = percentile(durationsMS, .99)

	sort.Slice(syncTimes, func(i, j int) bool { return syncTimes[i] < syncTimes[j] })
	r.SyncPointCount = len(syncTimes)
	var intervals []float64
	for i := 1; i < len(syncTimes); i++ {
		intervals = append(intervals, tl.SharedBase.Milliseconds(syncTimes[i]-syncTimes[i-1]))
	}
	r.SyncIntervalP50MS = percentile(intervals, .50)
	r.SyncIntervalP95MS = percentile(intervals, .95)
	return r
}

// SetSeekVerification records random-access verification results.
func (r *Report) SetSeekVerification(ok, total int, skewMS float64) {
	r.SeekOK = ok
	r.SeekTotal = total
	r.ManifestSkewMS = math.Round(skewMS*1000) / 1000
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	cp := append([]float64(nil), values...)
	sort.Float64s(cp)
	idx := int(math.Ceil(p*float64(len(cp)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return math.Round(cp[idx]*1000) / 1000
}
