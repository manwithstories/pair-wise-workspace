// Package report writes deterministic JSON reports and terminal lines.
// Floats are rounded to 4 decimals and +Inf is clamped so repeated runs
// produce byte-identical output.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"math"

	"mediad/internal/fault"
	"mediad/internal/metrics"
	"mediad/internal/pipeline"
	"mediad/internal/timeline"
)

// CaseReport is the per-case summary; field order is the JSON key order.
type CaseReport struct {
	Case            string              `json:"case"`
	TargetBPS       int64               `json:"target_bps"`
	ActualBPS       int64               `json:"actual_bps"`
	DeviationPct    float64             `json:"deviation_pct"`
	Frames          int                 `json:"frames"`
	CorruptRanges   int                 `json:"corrupt_ranges"`
	RecoveredFrames int                 `json:"recovered_frames"`
	PSNRP50DB       float64             `json:"psnr_p50_db"`
	MSE             float64             `json:"mse"`
	AlignMaxMs      float64             `json:"align_max_ms"`
	AudioSamples    map[string]int64    `json:"audio_samples,omitempty"`
	Ranges          []timeline.HalfOpen `json:"ranges,omitempty"`
	Faults          []fault.Fault       `json:"faults,omitempty"`
	ExitCode        int                 `json:"exit_code"`
}

// Report is the top-level deterministic document.
type Report struct {
	Version int          `json:"version"`
	Cases   []CaseReport `json:"cases"`
}

func round4(x float64) float64 {
	if math.IsInf(x, 1) {
		return 99.99 // clamped +Inf (identical frames)
	}
	if math.IsInf(x, -1) || math.IsNaN(x) {
		return 0
	}
	return math.Round(x*1e4) / 1e4
}

// FromResult folds a pipeline result into a deterministic case report.
func FromResult(name string, targetBPS int64, res *pipeline.Result, exitCode int) CaseReport {
	actual := metrics.Bitrate(res.ProxyBytes, res.DurationSec)
	var psnrs []float64
	var mseSum float64
	for _, st := range res.FrameStats {
		psnrs = append(psnrs, st.PSNR)
		mseSum += st.MSE
	}
	mse := 0.0
	if len(res.FrameStats) > 0 {
		mse = mseSum / float64(len(res.FrameStats))
	}
	return CaseReport{
		Case:            name,
		TargetBPS:       targetBPS,
		ActualBPS:       int64(math.Round(actual)),
		DeviationPct:    round4(metrics.DeviationPct(actual, float64(targetBPS))),
		Frames:          res.Frames,
		CorruptRanges:   len(res.CorruptRanges),
		RecoveredFrames: res.RecoveredFrames,
		PSNRP50DB:       round4(metrics.Median(psnrs)),
		MSE:             round4(mse),
		AlignMaxMs:      round4(res.AlignMaxMs),
		AudioSamples:    res.AudioSamples,
		Ranges:          res.CorruptRanges,
		Faults:          res.Faults,
		ExitCode:        exitCode,
	}
}

// Line renders the single-line terminal summary of a case.
func (c CaseReport) Line() string {
	return fmt.Sprintf("case=%s target_bps=%d actual_bps=%d deviation_pct=%.4g frames=%d corrupt_ranges=%d recovered_frames=%d psnr_p50_db=%.4g mse=%.4g align_max_ms=%.4g",
		c.Case, c.TargetBPS, c.ActualBPS, c.DeviationPct, c.Frames,
		c.CorruptRanges, c.RecoveredFrames, c.PSNRP50DB, c.MSE, c.AlignMaxMs)
}

// Write emits the report as indented JSON with a trailing newline.
func Write(w io.Writer, r *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
