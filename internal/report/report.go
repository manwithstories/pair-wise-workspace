// Package report builds JSON metrics reports and baseline regressions.
package report

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// SegReport holds per-segment metrics.
type SegReport struct {
	Index  int     `json:"index"`
	Name   string  `json:"name"`
	Frames int     `json:"frames"`
	QP     float64 `json:"qp"`
	Bits   int64   `json:"bits"`
	PSNRDb float64 `json:"psnr_db"`
}

// Report is the top-level metrics document.
type Report struct {
	TargetKbps   int         `json:"target_kbps"`
	ActualKbps   float64     `json:"actual_kbps"`
	DeviationPct float64     `json:"deviation_pct"`
	PSNRMeanDb   float64     `json:"psnr_mean_db"`
	PSNRSegStdDb float64     `json:"psnr_seg_std_db"`
	Segments     []SegReport `json:"segments"`
}

func round3(v float64) float64 { return float64(int(v*1000+0.5)) / 1000 }

// New assembles a report, rounding persisted values for stable diffs.
func New(targetKbps int, actualKbps, deviationPct, psnrMean, psnrStd float64, segs []SegReport) *Report {
	sort.Slice(segs, func(i, j int) bool { return segs[i].Index < segs[j].Index })
	for i := range segs {
		segs[i].PSNRDb = round3(segs[i].PSNRDb)
		segs[i].QP = round3(segs[i].QP)
	}
	return &Report{
		TargetKbps:   targetKbps,
		ActualKbps:   round3(actualKbps),
		DeviationPct: round3(deviationPct),
		PSNRMeanDb:   round3(psnrMean),
		PSNRSegStdDb: round3(psnrStd),
		Segments:     segs,
	}
}

// Save writes the report as indented JSON.
func (r *Report) Save(path string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// Load reads a report JSON file.
func Load(path string) (*Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read report %q: %w", path, err)
	}
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("report %q is not valid JSON: %w", path, err)
	}
	if len(r.Segments) == 0 {
		return nil, fmt.Errorf("report %q has no segments", path)
	}
	return &r, nil
}

// Summary renders the report as terminal-friendly lines.
func (r *Report) Summary() string {
	out := fmt.Sprintf("target_kbps %d  actual_kbps %.1f  deviation_pct %.1f  psnr_mean_db %.1f  psnr_seg_std_db %.1f",
		r.TargetKbps, r.ActualKbps, r.DeviationPct, r.PSNRMeanDb, r.PSNRSegStdDb)
	for _, s := range r.Segments {
		out += fmt.Sprintf("\n  seg %d %-8s frames=%d qp=%.1f bits=%d psnr_db=%.2f",
			s.Index, s.Name, s.Frames, s.QP, s.Bits, s.PSNRDb)
	}
	return out
}

// Regress compares current against baseline; ok is false on regression.
func Regress(baseline, current *Report, maxPSNRDrop, maxSegStd float64) (ok bool, reasons []string) {
	drop := baseline.PSNRMeanDb - current.PSNRMeanDb
	if drop > maxPSNRDrop {
		reasons = append(reasons, fmt.Sprintf("psnr_mean_db dropped %.2f dB (%.2f -> %.2f), allowed %.2f",
			drop, baseline.PSNRMeanDb, current.PSNRMeanDb, maxPSNRDrop))
	}
	if current.PSNRSegStdDb > maxSegStd {
		reasons = append(reasons, fmt.Sprintf("psnr_seg_std_db %.2f exceeds threshold %.2f",
			current.PSNRSegStdDb, maxSegStd))
	}
	return len(reasons) == 0, reasons
}
