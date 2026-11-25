package audit

import (
	"fmt"

	"mediqc/internal/media"
)

// BitrateReport contains target/actual bitrate and parts-per-million deviation.
type BitrateReport struct {
	TargetBPS int64 `json:"bitrate_target_bps"`
	ActualBPS int64 `json:"bitrate_actual_bps"`
	DeltaPPM  int64 `json:"bitrate_delta_ppm"`
}

// ComputeBitrate calculates elementary payload bitrate over manifest duration.
func ComputeBitrate(targetBPS int64, payloadBytes int, durationMS int64) BitrateReport {
	var actual int64
	if durationMS > 0 {
		actual = int64(float64(payloadBytes*8) * 1000 / float64(durationMS))
	}
	return BitrateReport{TargetBPS: targetBPS, ActualBPS: actual, DeltaPPM: PPM(targetBPS, actual)}
}

// PPM returns signed relative deviation in integer parts per million.
func PPM(target, actual int64) int64 {
	if target == 0 {
		return 0
	}
	return int64(float64(actual-target) / float64(target) * 1_000_000)
}

// FormatPercent renders the exact suite percentage convention.
func FormatPercent(matched, total int) string {
	if total == 0 {
		return "0%"
	}
	pct := float64(matched) * 100 / float64(total)
	if pct == 100 {
		return "100%"
	}
	return fmt.Sprintf("%.2f%%", pct)
}

// DurationSkew assigns VFR durations, recomputes duration and returns |skew|.
func DurationSkew(manifestDurationMS int64, samples []media.Sample) (auditedMS, skewMS int64) {
	media.AssignDurations(samples, 3000)
	auditedMS = media.DurationMS(samples)
	d := manifestDurationMS - auditedMS
	if d < 0 {
		d = -d
	}
	return auditedMS, d
}

// QualitySummary aggregates hash checks and maximum timeline/manifest skew.
type QualitySummary struct {
	Matched int    `json:"sample_hash_matched"`
	Total   int    `json:"sample_hash_total"`
	Match   string `json:"sample_hash_match"`
	MaxSkew int64  `json:"manifest_skew_ms"`
}

// NewQualitySummary builds the stable quality summary fields.
func NewQualitySummary(matched, total int, maxSkewMS int64) QualitySummary {
	return QualitySummary{
		Matched: matched, Total: total,
		Match: FormatPercent(matched, total), MaxSkew: maxSkewMS,
	}
}
