// Package report renders comparable text and JSON quality reports.
package report

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"

	"turncode/internal/config"
	"turncode/internal/quality"
	"turncode/internal/state"
)

const Version = 1

type SegmentReport struct {
	Index      int     `json:"index"`
	StartFrame int     `json:"start_frame"`
	Frames     int     `json:"frames"`
	PSNRMeanDB float64 `json:"psnr_mean_db"`
}

type Report struct {
	Version          int             `json:"version"`
	TargetBPS        int64           `json:"target_bps"`
	ActualBPS        int64           `json:"actual_bps"`
	DeviationPct     float64         `json:"deviation_pct"`
	PSNRMeanDB       float64         `json:"psnr_mean_db"`
	PSNRSegmentStdDB float64         `json:"psnr_segment_std_db"`
	PSNRMinDB        float64         `json:"psnr_min_db"`
	GOPCount         int             `json:"gop_count"`
	ResumedFromGOP   int             `json:"resumed_from_gop"`
	Regression       bool            `json:"regression"`
	DegradedSegments []int           `json:"degraded_segments,omitempty"`
	Segments         []SegmentReport `json:"segments,omitempty"`
}

type Input struct {
	Spec           config.Spec
	BitstreamBytes int64
	Checkpoint     state.Checkpoint
}

func Build(in Input) Report {
	acc := in.Checkpoint.Quality
	r := Report{
		Version:          Version,
		TargetBPS:        in.Spec.TargetBPS,
		PSNRMeanDB:       round(acc.Mean(), 4),
		PSNRSegmentStdDB: round(acc.SegmentStdDB(), 4),
		PSNRMinDB:        round(acc.MinSegmentDB(), 4),
		GOPCount:         in.Checkpoint.CompletedGOPs,
		ResumedFromGOP:   in.Checkpoint.ResumedFromGOP,
	}
	if in.Spec.Frames > 0 && in.Spec.FPS > 0 {
		r.ActualBPS = int64(math.Round(float64(in.BitstreamBytes) * 8 * float64(in.Spec.FPS) / float64(in.Spec.Frames)))
	}
	if r.TargetBPS > 0 {
		r.DeviationPct = round(float64(r.ActualBPS-r.TargetBPS)/float64(r.TargetBPS)*100, 4)
	}
	for _, seg := range acc.Segments {
		if seg.Frames == 0 {
			continue
		}
		r.Segments = append(r.Segments, SegmentReport{
			Index:      seg.Index,
			StartFrame: seg.StartFrame,
			Frames:     seg.Frames,
			PSNRMeanDB: round(seg.Mean(), 4),
		})
	}
	return r
}

// ApplyBaseline mutates r with the regression decision and degraded segments.
func ApplyBaseline(r *Report, baseline Report) quality.Regression {
	cur := r.Summary()
	base := baseline.Summary()
	reg := quality.Compare(cur, base)
	r.Regression = reg.Regression
	r.DegradedSegments = append([]int(nil), reg.DegradedSegments...)
	return reg
}

func (r Report) Summary() quality.Summary {
	means := make([]float64, 0, len(r.Segments))
	segFrames := 0
	for _, s := range r.Segments {
		means = append(means, s.PSNRMeanDB)
		if segFrames == 0 {
			segFrames = s.Frames
		}
	}
	return quality.Summary{
		MeanDB:        r.PSNRMeanDB,
		SegmentStdDB:  r.PSNRSegmentStdDB,
		MinDB:         r.PSNRMinDB,
		SegmentMeans:  means,
		SegmentFrames: segFrames,
	}
}

// WriteJSON atomically writes a report.
func WriteJSON(path string, r Report) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return state.AtomicWrite(path, raw, 0o644)
}

func Load(path string) (Report, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Report{}, err
	}
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return Report{}, fmt.Errorf("report: decode baseline: %w", err)
	}
	if r.Version != Version {
		return Report{}, fmt.Errorf("report: unsupported baseline version %d", r.Version)
	}
	if r.PSNRMeanDB <= 0 || len(r.Segments) == 0 {
		return Report{}, errors.New("report: baseline lacks quality data")
	}
	return r, nil
}

// WriteText renders stable key/value output suitable for terminal review.
func WriteText(w io.Writer, r Report) error {
	if _, err := fmt.Fprintf(w, "target_bps: %d\n", r.TargetBPS); err != nil {
		return err
	}
	lines := []struct {
		key string
		val any
	}{
		{"actual_bps", r.ActualBPS},
		{"deviation_pct", r.DeviationPct},
		{"psnr_mean_db", r.PSNRMeanDB},
		{"psnr_segment_std_db", r.PSNRSegmentStdDB},
		{"psnr_min_db", r.PSNRMinDB},
		{"gop_count", r.GOPCount},
		{"resumed_from_gop", r.ResumedFromGOP},
		{"regression", r.Regression},
	}
	for _, line := range lines {
		if _, err := fmt.Fprintf(w, "%s: %v\n", line.key, line.val); err != nil {
			return err
		}
	}
	if len(r.DegradedSegments) > 0 {
		if _, err := fmt.Fprintf(w, "degraded_segments: %v\n", r.DegradedSegments); err != nil {
			return err
		}
	}
	for _, seg := range r.Segments {
		if _, err := fmt.Fprintf(w, "segment[%d] frames=%d-%d psnr_mean_db=%.4f\n",
			seg.Index, seg.StartFrame, seg.StartFrame+seg.Frames-1, seg.PSNRMeanDB); err != nil {
			return err
		}
	}
	return nil
}

func WorkReportPath(workDir string) string { return filepath.Join(workDir, "report.json") }

func round(v float64, places int) float64 {
	p := math.Pow10(places)
	return math.Round(v*p) / p
}
