// Package report aggregates pipeline results into a deterministic summary.
package report

import (
	"fmt"
	"io"
	"math"
	"sort"
)

// Metrics carries the aggregated pipeline outcome.
type Metrics struct {
	Fixture           string
	Segments          int
	Repaired          int
	Unrecoverable     int
	IndexEntries      int
	RejectedPackets   int
	RecoveredSegments int
	TargetKbps        int
	ActualKbps        int
	DeltaPct          float64
	PSNRdB            float64
	ManifestErrMs     float64
	AlignErrMs        float64
	PeakMemBytes      int64
	BudgetMiB         int64
}

// PSNR computes peak signal-to-noise ratio in dB from summed squared error
// over numSamples 8-bit samples.
func PSNR(sumSqErr float64, numSamples uint64) float64 {
	if sumSqErr <= 0 || numSamples == 0 {
		return 99.0
	}
	mse := sumSqErr / float64(numSamples)
	return 10 * math.Log10(255.0*255.0/mse)
}

// DeltaPercent computes (actual-target)/target in percent.
func DeltaPercent(actual, target int) float64 {
	if target == 0 {
		return 0
	}
	return float64(actual-target) * 100.0 / float64(target)
}

// Render writes the summary as sorted key=value lines, one per line.
func (m Metrics) Render(w io.Writer) {
	lines := map[string]string{
		"fixture":              m.Fixture,
		"segments":             fmt.Sprint(m.Segments),
		"repaired":             fmt.Sprint(m.Repaired),
		"unrecoverable":        fmt.Sprint(m.Unrecoverable),
		"index_entries":        fmt.Sprint(m.IndexEntries),
		"target_bitrate_kbps":  fmt.Sprint(m.TargetKbps),
		"actual_bitrate_kbps":  fmt.Sprint(m.ActualKbps),
		"bitrate_delta_pct":    fmt.Sprintf("%.2f", m.DeltaPct),
		"quality_psnr_db":      fmt.Sprintf("%.1f", m.PSNRdB),
		"manifest_error_ms":    fmt.Sprintf("%.1f", m.ManifestErrMs),
		"peak_memory_mib":      fmt.Sprint(m.PeakMemBytes >> 20),
		"budget_mib":           fmt.Sprint(m.BudgetMiB),
		"rejected_packets":     fmt.Sprint(m.RejectedPackets),
		"recovered_segments":   fmt.Sprint(m.RecoveredSegments),
		"alignment_error_ms":   fmt.Sprintf("%.1f", m.AlignErrMs),
	}
	keys := make([]string, 0, len(lines))
	for k := range lines {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s=%s\n", k, lines[k])
	}
}
