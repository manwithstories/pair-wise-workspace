// Package report 输出对齐 JSON 报告与终端摘要。
package report

import (
	"encoding/json"
	"fmt"
	"os"

	"dashpack/internal/align"
	"dashpack/internal/errs"
)

// RepReport 是一路表示层的对齐指标。
type RepReport struct {
	RepID              string  `json:"rep_id"`
	ContentType        string  `json:"content_type"`
	Timescale          int64   `json:"timescale"`
	MaxDeviationTicks  int64   `json:"max_deviation_ticks"`
	MaxDeviationMs     float64 `json:"max_deviation_ms"`
	MeanDeviationMs    float64 `json:"mean_deviation_ms"`
	MisalignedSegments []int64 `json:"misaligned_segments"`
}

// Report 是 alignment.json 的顶层结构。
type Report struct {
	Policy           string         `json:"policy"`
	PeriodDurationMs int64          `json:"period_duration_ms"`
	CommonStartMs    int64          `json:"common_start_ms"`
	RepairedItems    int            `json:"repaired_items"`
	Repairs          []align.Repair `json:"repairs"`
	Representations  []RepReport    `json:"representations"`
}

// FromAlignment 由对齐结果与修补统计构建报告。
func FromAlignment(res *align.Result, periodMs int64, repairs []align.Repair, repairedItems int) *Report {
	r := &Report{
		Policy:           res.Policy,
		PeriodDurationMs: periodMs,
		CommonStartMs:    res.CommonStartMs,
		RepairedItems:    repairedItems,
		Repairs:          repairs,
	}
	if r.Repairs == nil {
		r.Repairs = []align.Repair{}
	}
	for _, rr := range res.Reps {
		mis := rr.MisalignedSegments
		if mis == nil {
			mis = []int64{}
		}
		r.Representations = append(r.Representations, RepReport{
			RepID:              rr.RepID,
			ContentType:        rr.ContentType,
			Timescale:          rr.Timescale,
			MaxDeviationTicks:  rr.MaxDeviationTicks,
			MaxDeviationMs:     rr.MaxDeviationMs,
			MeanDeviationMs:    rr.MeanDeviationMs,
			MisalignedSegments: mis,
		})
	}
	return r
}

// Write 落盘 JSON 报告；失败报 E_WRITE 且不留半成品。
func Write(path string, r *Report) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return errs.New(errs.EWrite, errs.ExitWrite, "encode report: %v", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return errs.New(errs.EWrite, errs.ExitWrite, "write report: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return errs.New(errs.EWrite, errs.ExitWrite, "rename report: %v", err)
	}
	return nil
}

// Summary 生成终端可读摘要。
func Summary(r *Report) string {
	s := fmt.Sprintf("policy=%s period=%dms repaired_items=%d reps=%d",
		r.Policy, r.PeriodDurationMs, r.RepairedItems, len(r.Representations))
	for _, rep := range r.Representations {
		s += fmt.Sprintf("\n  %s: max_dev=%dticks/%.3fms mean_dev=%.3fms misaligned=%v",
			rep.RepID, rep.MaxDeviationTicks, rep.MaxDeviationMs,
			rep.MeanDeviationMs, rep.MisalignedSegments)
	}
	return s
}
