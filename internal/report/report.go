// Package report aggregates job checkpoints into quality/bitrate reports.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"

	"rcbench/internal/job"
	"rcbench/internal/quality"
)

// Report is the aggregated view of a job.
type Report struct {
	TargetBps         int64     `json:"target_bps"`
	ActualBps         int64     `json:"actual_bps"`
	DeviationPct      float64   `json:"deviation_pct"`
	Segments          int       `json:"segments"`
	CompletedSegments int       `json:"completed_segments"`
	State             string    `json:"state"`
	PSNRMeanDB        float64   `json:"psnr_mean_db"`
	PSNRStdDB         float64   `json:"psnr_std_db"`
	PSNRSpreadDB      float64   `json:"psnr_spread_db"`
	BitrateBySegment  []int64   `json:"bitrate_by_segment"`
	PSNRBySegment     []float64 `json:"psnr_by_segment"`
	QPBySegment       []int     `json:"qp_by_segment"`
	Poisoned          []int     `json:"poisoned,omitempty"`
}

// Build reads the job manifest and all valid checkpoints.
func Build(jobDir string) (*Report, error) {
	meta, err := job.ReadMeta(jobDir)
	if err != nil {
		return nil, err
	}
	r := &Report{
		TargetBps: meta.TargetBps,
		Segments:  meta.Segments,
		State:     meta.State,
		Poisoned:  meta.Poisoned,
	}
	var totalBits int64
	var totalFrames int
	for seg := 1; seg <= meta.Segments; seg++ {
		ck, err := job.ReadCheckpoint(jobDir, seg)
		if err != nil {
			continue // missing or half-written: not completed
		}
		r.CompletedSegments++
		totalBits += ck.Bits
		totalFrames += ck.Frames
		bps := int64(0)
		if ck.Frames > 0 && meta.FPS > 0 {
			bps = ck.Bits * int64(meta.FPS) / int64(ck.Frames)
		}
		r.BitrateBySegment = append(r.BitrateBySegment, bps)
		r.PSNRBySegment = append(r.PSNRBySegment, round2(ck.PSNR))
		r.QPBySegment = append(r.QPBySegment, ck.QP)
	}
	if totalFrames > 0 && meta.FPS > 0 {
		r.ActualBps = totalBits * int64(meta.FPS) / int64(totalFrames)
	}
	if meta.TargetBps > 0 {
		r.DeviationPct = float64(r.ActualBps-meta.TargetBps) / float64(meta.TargetBps) * 100
	}
	mean, std := quality.MeanStd(r.PSNRBySegment)
	r.PSNRMeanDB = round2(mean)
	r.PSNRStdDB = round2(std)
	r.PSNRSpreadDB = round2(quality.Spread(r.PSNRBySegment))
	return r, nil
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// Exit codes shared by the CLI.
const (
	ExitOK          = 0
	ExitUsage       = 2
	ExitBitrate     = 3
	ExitQuality     = 4
	ExitFailed      = 5
	ExitInterrupted = 130
)

// Evaluate checks thresholds; returns the exit code and human reasons.
// psnrFloor/psnrSpread/maxDeviationPct <= 0 disable the respective check.
func (r *Report) Evaluate(psnrFloor, psnrSpread, maxDeviationPct float64) (int, []string) {
	var reasons []string
	code := ExitOK
	if psnrFloor > 0 {
		for i, p := range r.PSNRBySegment {
			if p < psnrFloor {
				reasons = append(reasons, fmt.Sprintf("segment %d psnr %.2f dB below floor %.2f dB", i+1, p, psnrFloor))
			}
		}
	}
	if psnrSpread > 0 && r.PSNRSpreadDB > psnrSpread {
		reasons = append(reasons, fmt.Sprintf("psnr spread %.2f dB exceeds %.2f dB", r.PSNRSpreadDB, psnrSpread))
	}
	if len(reasons) > 0 {
		code = ExitQuality
	}
	if maxDeviationPct > 0 && math.Abs(r.DeviationPct) > maxDeviationPct {
		reasons = append(reasons, fmt.Sprintf("bitrate deviation %.2f%% exceeds %.2f%%", r.DeviationPct, maxDeviationPct))
		if code == ExitOK {
			code = ExitBitrate
		}
	}
	return code, reasons
}

// WriteJSON emits the report as indented JSON.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteText emits a stable, segment-sorted text table.
func (r *Report) WriteText(w io.Writer) {
	fmt.Fprintf(w, "state:            %s\n", r.State)
	fmt.Fprintf(w, "target_bps:       %d\n", r.TargetBps)
	fmt.Fprintf(w, "actual_bps:       %d\n", r.ActualBps)
	fmt.Fprintf(w, "deviation_pct:    %.2f\n", r.DeviationPct)
	fmt.Fprintf(w, "segments:         %d (completed %d)\n", r.Segments, r.CompletedSegments)
	fmt.Fprintf(w, "psnr_mean_db:     %.2f\n", r.PSNRMeanDB)
	fmt.Fprintf(w, "psnr_std_db:      %.2f\n", r.PSNRStdDB)
	fmt.Fprintf(w, "psnr_spread_db:   %.2f\n", r.PSNRSpreadDB)
	if len(r.Poisoned) > 0 {
		fmt.Fprintf(w, "poisoned:         %v\n", r.Poisoned)
	}
	fmt.Fprintln(w, "segment  qp     bitrate_bps  psnr_db")
	for i := range r.BitrateBySegment {
		fmt.Fprintf(w, "%7d  %3d  %12d  %7.2f\n", i+1, r.QPBySegment[i], r.BitrateBySegment[i], r.PSNRBySegment[i])
	}
}

// SortedSegments is a helper proving stable ordering (segments are 1..N).
func (r *Report) SortedSegments() []int {
	out := make([]int, len(r.BitrateBySegment))
	for i := range out {
		out[i] = i + 1
	}
	sort.Ints(out)
	return out
}
