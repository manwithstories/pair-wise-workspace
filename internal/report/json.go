// Package report renders and persists segment quality reports.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"rcctl/internal/metrics"
	"rcctl/internal/plan"
	"rcctl/internal/state"
)

const Version = 1

type Summary struct {
	Segments        int     `json:"segments"`
	Completed       int     `json:"completed"`
	Failed          int     `json:"failed"`
	MeanPSNRDB      float64 `json:"mean_psnr_db"`
	PSNRStdDB       float64 `json:"psnr_std_db"`
	MaxDevPct       float64 `json:"max_dev_pct"`
	ResumedSegments int     `json:"resumed_segments"`
}

type Report struct {
	Version         int                   `json:"version"`
	PlanID          string                `json:"plan_id"`
	Clip            string                `json:"clip"`
	GeneratedAt     time.Time             `json:"generated_at"`
	ResumedSegments int                   `json:"resumed_segments"`
	Summary         Summary               `json:"summary"`
	Segments        []state.SegmentResult `json:"segments"`
}

// Build fills every plan segment and computes aggregate metrics.
func Build(p *plan.Plan, results []state.SegmentResult, resumed int) Report {
	byIndex := make(map[int]state.SegmentResult, len(results))
	for _, r := range results {
		byIndex[r.Index] = r
	}
	rows := make([]state.SegmentResult, 0, len(p.Segments))
	var psnrs []float64
	summary := Summary{Segments: len(p.Segments), ResumedSegments: resumed}
	for _, seg := range p.Segments {
		r, ok := byIndex[seg.Index]
		if !ok {
			r = state.SegmentResult{Index: seg.Index, Frames: seg.Frames, TargetKbps: seg.TargetKbps,
				DevPct: 100, Status: state.Failed}
		}
		if r.Status == "" {
			r.Status = state.Done
		}
		rows = append(rows, r)
		if r.Status == state.Done {
			summary.Completed++
			psnrs = append(psnrs, r.PSNRDB)
			if math.Abs(r.DevPct) > summary.MaxDevPct {
				summary.MaxDevPct = math.Abs(r.DevPct)
			}
		} else {
			summary.Failed++
		}
	}
	summary.MeanPSNRDB, summary.PSNRStdDB = metrics.Aggregate(psnrs)
	return Report{Version: Version, PlanID: p.PlanID, Clip: p.Clip, GeneratedAt: time.Now().UTC(),
		ResumedSegments: resumed, Summary: summary, Segments: rows}
}

func (r *Report) Validate() error {
	if r.Version != Version {
		return fmt.Errorf("unsupported report version %d", r.Version)
	}
	if r.PlanID == "" {
		return fmt.Errorf("report plan_id is empty")
	}
	if r.Summary.Segments != len(r.Segments) {
		return fmt.Errorf("summary segment count %d differs from rows %d", r.Summary.Segments, len(r.Segments))
	}
	if r.Summary.Completed+r.Summary.Failed > r.Summary.Segments {
		return fmt.Errorf("summary status counts exceed segment count")
	}
	for _, s := range r.Segments {
		if s.TargetKbps <= 0 || s.Frames < 0 {
			return fmt.Errorf("invalid segment %d values", s.Index)
		}
	}
	return nil
}

func Save(path string, r Report) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func Load(path string) (Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Report{}, err
	}
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		return Report{}, fmt.Errorf("decode report: %w", err)
	}
	if err := r.Validate(); err != nil {
		return Report{}, err
	}
	return r, nil
}

func Print(w io.Writer, r Report) {
	fmt.Fprintf(w, "summary{segments:%d, mean_psnr_db:%.2f, psnr_std_db:%.2f, max_dev_pct:%.2f, resumed_segments:%d}\n",
		r.Summary.Segments, r.Summary.MeanPSNRDB, r.Summary.PSNRStdDB, r.Summary.MaxDevPct, r.ResumedSegments)
	fmt.Fprintln(w, "segments[]{index,target_kbps,actual_kbps,dev_pct,psnr_db}")
	for _, s := range r.Segments {
		fmt.Fprintf(w, "  {%d,%.2f,%.2f,%.2f,%.2f}\n", s.Index, s.TargetKbps, s.ActualKbps, s.DevPct, s.PSNRDB)
	}
}

type Violation struct {
	Index  int
	Reason string
}

// Verify enforces the deviation and segment-quality variance limits.
func Verify(r Report, maxDevPct, maxPSNRStd float64) []Violation {
	var out []Violation
	for _, s := range r.Segments {
		if s.Status != state.Done {
			out = append(out, Violation{Index: s.Index, Reason: "segment not completed"})
			continue
		}
		if math.Abs(s.DevPct) > maxDevPct {
			out = append(out, Violation{Index: s.Index,
				Reason: fmt.Sprintf("deviation %.2f%% exceeds %.2f%%", s.DevPct, maxDevPct)})
		}
	}
	if r.Summary.PSNRStdDB > maxPSNRStd {
		out = append(out, Violation{Index: -1,
			Reason: fmt.Sprintf("PSNR std %.2f dB exceeds %.2f dB", r.Summary.PSNRStdDB, maxPSNRStd)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}
