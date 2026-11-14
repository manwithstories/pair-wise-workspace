package job

import (
	"fmt"
	"os"

	"rcx/internal/codec"
	"rcx/internal/quality"
	"rcx/internal/report"
)

func RunVerify(jobDir string) (int, error) {
	paths := NewPaths(jobDir)
	plan, err := loadPlan(paths.Plan)
	if err != nil {
		return 0, err
	}
	verified := 0
	for _, sp := range plan.Segments {
		data, err := os.ReadFile(paths.Segment(sp.ID))
		if err != nil {
			return verified, fmt.Errorf("%w: read segment %d: %v", codec.ErrCorrupt, sp.ID, err)
		}
		decoded, err := codec.DecodeSegment(data)
		if err != nil {
			return verified, err
		}
		if decoded.Meta.ID != sp.ID || decoded.Meta.QP != sp.QP || decoded.Meta.FPS != plan.FPS || decoded.Meta.Frames != plan.SegmentFrames {
			return verified, fmt.Errorf("%w: segment %d metadata mismatch", codec.ErrCorrupt, sp.ID)
		}
		verified++
	}
	return verified, nil
}

func RunReport(jobDir, jsonOut string) (report.Report, error) {
	paths := NewPaths(jobDir)
	if jsonOut != "" {
		if err := ValidateInside(jobDir, jsonOut); err != nil {
			return report.Report{}, err
		}
	}
	plan, err := loadPlan(paths.Plan)
	if err != nil {
		return report.Report{}, err
	}
	rep, err := buildReportFromPlan(paths, plan)
	if err != nil {
		return report.Report{}, err
	}
	if jsonOut != "" {
		if err := report.Write(jsonOut, rep); err != nil {
			return report.Report{}, err
		}
	}
	return rep, nil
}

func RunRegress(baselineDir, candidateDir string, dropThreshold float64) (quality.RegressionResult, error) {
	base, err := reportForJob(baselineDir)
	if err != nil {
		return quality.RegressionResult{}, fmt.Errorf("baseline: %w", err)
	}
	candidate, err := reportForJob(candidateDir)
	if err != nil {
		return quality.RegressionResult{}, fmt.Errorf("candidate: %w", err)
	}
	baseQuality := make([]quality.SegmentQuality, 0, len(base.Segments))
	for _, s := range base.Segments {
		baseQuality = append(baseQuality, quality.SegmentQuality{ID: s.ID, PSNRDB: s.PSNRDB})
	}
	candQuality := make([]quality.SegmentQuality, 0, len(candidate.Segments))
	for _, s := range candidate.Segments {
		candQuality = append(candQuality, quality.SegmentQuality{ID: s.ID, PSNRDB: s.PSNRDB})
	}
	result, err := quality.Compare(baseQuality, candQuality, dropThreshold)
	if err != nil {
		return quality.RegressionResult{}, err
	}
	if result.RegressedCount > 0 {
		return result, ErrRegression
	}
	return result, nil
}

func reportForJob(jobDir string) (report.Report, error) {
	paths := NewPaths(jobDir)
	plan, err := loadPlan(paths.Plan)
	if err == nil {
		return buildReportFromPlan(paths, plan)
	}
	if !os.IsNotExist(err) {
		return report.Report{}, err
	}
	return report.Load(paths.Report)
}
