package main

import (
	"fmt"

	"pairflow/internal/report"
)

// printMetrics 按验收字段名逐行输出单轨道指标。
func printMetrics(m report.Metrics) {
	fmt.Printf("track_id=%d device=%q\n", m.TrackID, m.DeviceID)
	fmt.Printf("target_bitrate_bps=%d\n", m.TargetBitrateBps)
	fmt.Printf("actual_bitrate_bps=%d\n", m.ActualBitrateBps)
	fmt.Printf("bitrate_deviation_pct=%.4f\n", m.BitrateDeviationPct)
	fmt.Printf("clipped_pct=%.4f\n", m.ClippedPct)
	fmt.Printf("silent_pct=%.4f\n", m.SilentPct)
	fmt.Printf("missing_sample_pct=%.4f\n", m.MissingSamplePct)
	fmt.Printf("quality_p50=%.4f\n", m.QualityP50)
	fmt.Printf("quality_p95=%.4f\n", m.QualityP95)
	fmt.Printf("manifest_alignment_ms=%.4f\n", m.ManifestAlignmentMS)
}
