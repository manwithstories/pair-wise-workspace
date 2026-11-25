package pipeline

import (
	"fmt"
	"path/filepath"
	"strings"

	"fieldcut/internal/manifest"
	"fieldcut/internal/recover"
)

func fmtSegmentName(i int) string { return fmt.Sprintf("seg-%03d", i) }

func fmtBitrate(actual float64, delta float64) string {
	return fmt.Sprintf("actual_bps=%.2f bitrate_delta_pct=%.3f", actual, delta)
}

// MetricLines returns the stable terminal key=value report.
func MetricLines(m *manifest.Manifest) []string {
	if m == nil {
		return nil
	}
	x := m.Metrics
	lines := []string{
		fmt.Sprintf("target_bps=%d", x.TargetBPS),
		fmt.Sprintf("actual_bps=%.2f", x.ActualBPS),
		fmt.Sprintf("bitrate_delta_pct=%.3f", x.BitrateDeltaPct),
		fmt.Sprintf("rms_dbfs=%.3f", x.RMSDBFS),
		fmt.Sprintf("peak_dbfs=%.3f", x.PeakDBFS),
		fmt.Sprintf("clipped_samples=%d", x.ClippedSamples),
		fmt.Sprintf("segment_count=%d", x.SegmentCount),
		fmt.Sprintf("dedup_count=%d", x.DedupeCount),
		fmt.Sprintf("manifest_max_skew_ns=%d", x.ManifestMaxSkewNs),
	}
	if x.ReusedNodes > 0 || x.OutputSHA256 != "" {
		lines = append(lines, fmt.Sprintf("reused_nodes=%d", x.ReusedNodes))
	}
	if x.OutputSHA256 != "" {
		lines = append(lines, fmt.Sprintf("output_sha256=%s", x.OutputSHA256))
	}
	if x.ErrorCode != "" {
		lines = append(lines,
			fmt.Sprintf("error_code=%s", x.ErrorCode),
			fmt.Sprintf("track_id=%d", x.TrackID),
			fmt.Sprintf("interval_ns=%d", x.IntervalNs),
		)
	}
	return lines
}

// WriteMetrics atomically writes the same key=value report used by the CLI.
func WriteMetrics(outDir string, m *manifest.Manifest) error {
	var b strings.Builder
	for _, line := range MetricLines(m) {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return recover.AtomicWriteFile(filepath.Join(outDir, "metrics.txt"), []byte(b.String()))
}
