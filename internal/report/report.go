// Package report 聚合编码结果并输出终端表格与 JSON 报告。
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"rcx/internal/codec"
	"rcx/internal/job"
	"rcx/internal/rc"
)

// SegReport 是单段报告。
type SegReport struct {
	Index        int       `json:"index"`
	QP           float64   `json:"qp"`
	Status       string    `json:"status"`
	TargetKbps   float64   `json:"target_kbps"`
	ActualKbps   float64   `json:"actual_kbps"`
	DeviationPct float64   `json:"deviation_pct"`
	PSNRMeanDB   float64   `json:"psnr_mean_db"`
	PSNRStdDB    float64   `json:"psnr_std_db"`
	VBVMinRatio  float64   `json:"vbv_min_ratio"`
	FramePSNR    []float64 `json:"frame_psnr"`
}

// RunReport 是一次运行的汇总报告。
type RunReport struct {
	TargetKbps    float64     `json:"target_kbps"`
	ActualKbps    float64     `json:"actual_kbps"`
	DeviationPct  float64     `json:"deviation_pct"`
	PSNRMeanDB    float64     `json:"psnr_mean_db"`
	PSNRSegStdDB  float64     `json:"psnr_seg_std_db"`
	VBVMinRatio   float64     `json:"vbv_min_ratio"`
	SegmentsTotal int         `json:"segments_total"`
	Verdict       string      `json:"verdict"`
	Segments      []SegReport `json:"segments"`
}

// Build 由计划与检查点汇总运行报告。
func Build(plan *rc.Plan, j *job.Journal) *RunReport {
	r := &RunReport{SegmentsTotal: len(plan.Segments), Verdict: "PASS"}
	durTotal := float64(plan.Frames) / float64(plan.FPS)
	var totalBits, totalTarget int64
	var allPSNR []float64
	segMeans := []float64{}
	r.VBVMinRatio = 1
	for i, seg := range plan.Segments {
		cp := j.Segments[i]
		dur := float64(seg.Frames) / float64(plan.FPS)
		sr := SegReport{
			Index: i, QP: cp.QP, Status: cp.Status,
			TargetKbps:   float64(seg.TargetBits) / dur / 1000,
			ActualKbps:   float64(cp.Bits) / dur / 1000,
			DeviationPct: rc.DeviationPct(cp.Bits, seg.TargetBits),
			PSNRMeanDB:   cp.PSNRMeanDB,
			PSNRStdDB:    cp.PSNRStdDB,
			VBVMinRatio:  cp.VBVMinRatio,
			FramePSNR:    cp.FramePSNR,
		}
		r.Segments = append(r.Segments, sr)
		totalBits += cp.Bits
		totalTarget += seg.TargetBits
		allPSNR = append(allPSNR, cp.FramePSNR...)
		segMeans = append(segMeans, cp.PSNRMeanDB)
		if cp.VBVMinRatio < r.VBVMinRatio {
			r.VBVMinRatio = cp.VBVMinRatio
		}
		if cp.Status != "done" {
			r.Verdict = "FAIL"
		}
	}
	r.TargetKbps = float64(totalTarget) / durTotal / 1000
	r.ActualKbps = float64(totalBits) / durTotal / 1000
	r.DeviationPct = rc.DeviationPct(totalBits, totalTarget)
	r.PSNRMeanDB, _ = codec.MeanStd(allPSNR)
	_, r.PSNRSegStdDB = codec.MeanStd(segMeans)
	return r
}

// SegDiff 是逐段回归差值。
type SegDiff struct {
	Index          int     `json:"index"`
	PSNRDiffDB     float64 `json:"psnr_diff_db"`
	BitrateDiffPct float64 `json:"bitrate_diff_pct"`
}

// Compare 逐段比对两次运行，任一超阈值判 FAIL。
func Compare(run, base *RunReport, psnrThreshDB, bitrateThreshPct float64) ([]SegDiff, string) {
	diffs := []SegDiff{}
	verdict := "PASS"
	n := len(run.Segments)
	if len(base.Segments) < n {
		n = len(base.Segments)
	}
	for i := 0; i < n; i++ {
		d := SegDiff{
			Index:          i,
			PSNRDiffDB:     run.Segments[i].PSNRMeanDB - base.Segments[i].PSNRMeanDB,
			BitrateDiffPct: run.Segments[i].ActualKbps - base.Segments[i].ActualKbps,
		}
		if base.Segments[i].ActualKbps != 0 {
			d.BitrateDiffPct = (run.Segments[i].ActualKbps - base.Segments[i].ActualKbps) / base.Segments[i].ActualKbps * 100
		}
		if d.PSNRDiffDB < -psnrThreshDB || abs(d.BitrateDiffPct) > bitrateThreshPct {
			verdict = "FAIL"
		}
		diffs = append(diffs, d)
	}
	return diffs, verdict
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// PrintTable 输出终端表格。
func PrintTable(w io.Writer, r *RunReport, diffs []SegDiff) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SEG\tQP\tSTATUS\tTARGET_KBPS\tACTUAL_KBPS\tDEV_%\tPSNR_DB\tPSNR_STD\tVBV_MIN")
	for _, s := range r.Segments {
		fmt.Fprintf(tw, "%d\t%.2f\t%s\t%.1f\t%.1f\t%.2f\t%.2f\t%.2f\t%.3f\n",
			s.Index, s.QP, s.Status, s.TargetKbps, s.ActualKbps, s.DeviationPct,
			s.PSNRMeanDB, s.PSNRStdDB, s.VBVMinRatio)
	}
	tw.Flush()
	fmt.Fprintf(w, "TOTAL\ttarget=%.1fkbps\tactual=%.1fkbps\tdev=%.2f%%\tpsnr=%.2fdB\tseg_std=%.2fdB\tvbv_min=%.3f\tsegments=%d\tverdict=%s\n",
		r.TargetKbps, r.ActualKbps, r.DeviationPct, r.PSNRMeanDB, r.PSNRSegStdDB,
		r.VBVMinRatio, r.SegmentsTotal, r.Verdict)
	if len(diffs) > 0 {
		fmt.Fprintln(w, "REGRESSION (vs baseline):")
		tw2 := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw2, "SEG\tPSNR_DIFF_DB\tBITRATE_DIFF_%")
		for _, d := range diffs {
			fmt.Fprintf(tw2, "%d\t%+.2f\t%+.2f\n", d.Index, d.PSNRDiffDB, d.BitrateDiffPct)
		}
		tw2.Flush()
	}
}

// SaveJSON 落盘 JSON 报告。
func SaveJSON(path string, r *RunReport) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// LoadRun 读取运行报告。
func LoadRun(path string) (*RunReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r RunReport
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
