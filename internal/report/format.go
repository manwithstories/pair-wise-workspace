package report

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"loudcheck/internal/batch"
)

type jsonShortTerm struct {
	MaxLUFS   SafeFloat `json:"max_lufs"`
	StartMs   int64     `json:"start_ms"`
	Partial   bool      `json:"partial"`
	BlockSpan int       `json:"block_span"`
}

type jsonEngineResult struct {
	IntegratedLUFS       SafeFloat     `json:"integrated_lufs"`
	Defined              bool          `json:"defined"`
	RelativeGateLUFS     SafeFloat     `json:"relative_gate_lufs"`
	BlocksTotal          int           `json:"blocks_total"`
	BlocksAfterAbsolute  int           `json:"blocks_after_absolute"`
	BlocksAfterRelative  int           `json:"blocks_after_relative"`
	ShortTerm            jsonShortTerm `json:"short_term"`
	SampleRate           int           `json:"sample_rate"`
	Channels             int           `json:"channels"`
	TruePeakDBTP         SafeFloat     `json:"true_peak_dbtp"`
	SamplePeakLinear     SafeFloat     `json:"sample_peak_linear"`
	OversampledPeak      SafeFloat     `json:"oversampled_peak_linear"`
	InterSampleOvershoot bool          `json:"intersample_overshoot"`
}

type jsonFile struct {
	Path       string           `json:"path"`
	Status     batch.Status     `json:"status"`
	Result     jsonEngineResult `json:"result"`
	ReadError  string           `json:"read_error,omitempty"`
	DataOffset int64            `json:"data_offset,omitempty"`
	Declared   int64            `json:"declared,omitempty"`
}

type jsonReport struct {
	Profile string      `json:"profile"`
	Summary batch.Summary `json:"summary"`
	Files   []jsonFile  `json:"files"`
}

func toJSONReport(profile string, results []batch.FileResult, summary batch.Summary) jsonReport {
	out := jsonReport{Profile: profile, Summary: summary, Files: make([]jsonFile, 0, len(results))}
	for _, result := range results {
		metric := result.Result
		item := jsonFile{
			Path: result.Path, Status: result.Status,
			ReadError: result.ReadError, DataOffset: result.DataOffset, Declared: result.Declared,
			Result: jsonEngineResult{
				IntegratedLUFS:       SafeFloat(metric.IntegratedLUFS),
				Defined:              metric.Defined,
				RelativeGateLUFS:     SafeFloat(metric.RelativeGateLUFS),
				BlocksTotal:          metric.BlocksTotal,
				BlocksAfterAbsolute:  metric.BlocksAbsolute,
				BlocksAfterRelative:  metric.BlocksRelative,
				ShortTerm: jsonShortTerm{
					MaxLUFS: SafeFloat(metric.ShortTerm.MaxLUFS),
					StartMs: metric.ShortTerm.StartMs,
					Partial: metric.ShortTerm.Partial,
					BlockSpan: metric.ShortTerm.BlockSpan,
				},
				SampleRate:           metric.SampleRate,
				Channels:             metric.Channels,
				TruePeakDBTP:         SafeFloat(metric.TruePeakDBTP),
				SamplePeakLinear:     SafeFloat(metric.SamplePeakLinear),
				OversampledPeak:      SafeFloat(metric.OversampledPeak),
				InterSampleOvershoot: metric.InterSampleOvershoot,
			},
		}
		out.Files = append(out.Files, item)
	}
	return out
}

func formatDB(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "undefined"
	}
	return fmt.Sprintf("%7.2f", value)
}

func JudgementLine(profile batch.Profile, result batch.FileResult) string {
	if result.Status == batch.StatusReadError {
		return fmt.Sprintf("%-10s %s  %s", result.Status, result.Path, result.ReadError)
	}
	metric := result.Result
	loudDev := metric.IntegratedLUFS - profile.TargetLUFS
	peakDev := metric.TruePeakDBTP - profile.MaxTruePeakDBTP
	loudVerdict := "OK"
	if math.Abs(loudDev) > profile.ToleranceLU || !metric.Defined {
		loudVerdict = "OUT"
	}
	peakVerdict := "OK"
	if peakDev > 0 {
		peakVerdict = "OUT"
	}
	return strings.Join([]string{
		fmt.Sprintf("%-10s %s", result.Status, result.Path),
		fmt.Sprintf("integrated=%s LUFS (target %.1f, dev %+.2f LU) [%s]",
			formatDB(metric.IntegratedLUFS), profile.TargetLUFS, loudDev, loudVerdict),
		fmt.Sprintf("true_peak=%s dBTP (limit %.1f, dev %+.2f dB) [%s]",
			formatDB(metric.TruePeakDBTP), profile.MaxTruePeakDBTP, peakDev, peakVerdict),
	}, "\n           ")
}

func PrintAnalyze(result batch.FileResult, profile batch.Profile) {
	if result.Status == batch.StatusReadError {
		if result.DataOffset > 0 || result.Declared > 0 {
			fmt.Printf("read error: %s\ndata payload byte offset: %d, declared length: %d bytes\n",
				result.ReadError, result.DataOffset, result.Declared)
		} else {
			fmt.Printf("read error: %s\n", result.ReadError)
		}
		return
	}
	metric := result.Result
	fmt.Println(JudgementLine(profile, result))
	fmt.Printf("short_term_max_lufs=%s at %.1fs%s\n",
		formatDB(metric.ShortTerm.MaxLUFS), float64(metric.ShortTerm.StartMs)/1000,
		partialMark(metric.ShortTerm.Partial))
	fmt.Printf("blocks_total=%d blocks_after_absolute=%d blocks_after_relative=%d\n",
		metric.BlocksTotal, metric.BlocksAbsolute, metric.BlocksRelative)
	fmt.Printf("relative_gate_lufs=%s sample_rate=%d channels=%d\n",
		formatDB(metric.RelativeGateLUFS), metric.SampleRate, metric.Channels)
	fmt.Printf("sample_peak=%.6f oversampled_peak=%.6f intersample_overshoot=%t\n",
		metric.SamplePeakLinear, metric.OversampledPeak, metric.InterSampleOvershoot)
	fmt.Printf("verdict=%s\n", result.Status)
}

func partialMark(partial bool) string {
	if partial {
		return " (partial: shorter than 3 s window)"
	}
	return ""
}

func PrintBatch(results []batch.FileResult, summary batch.Summary) {
	for _, result := range results {
		fmt.Println(batchLine(result))
	}
	fmt.Printf("summary: total=%d pass=%d fail=%d read_error=%d\n",
		summary.Total, summary.Pass, summary.Fail, summary.ReadError)
}

func batchLine(result batch.FileResult) string {
	if result.Status == batch.StatusReadError {
		return fmt.Sprintf("%-10s %s  %s", result.Status, result.Path, result.ReadError)
	}
	metric := result.Result
	return fmt.Sprintf("%-10s %s  integrated=%s LUFS short=%s LUFS peak=%s dBTP",
		result.Status, result.Path, formatDB(metric.IntegratedLUFS),
		formatDB(metric.ShortTerm.MaxLUFS), formatDB(metric.TruePeakDBTP))
}

func WriteJSON(path string, profile string, results []batch.FileResult, summary batch.Summary) error {
	report := toJSONReport(profile, results, summary)
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o644)
}
