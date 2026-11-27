// Package analyze 在 ingest 产出的结构与包记录上执行 RIFF 核验、
// 损坏区段定位与同步锚点间隔分析。
package analyze

import (
	"fmt"

	"bcastqc/internal/ingest"
	"bcastqc/internal/model"
)

// StructureReport 汇总单条轨道对应 WAV 的结构核验结论。
type StructureReport struct {
	TrackID      int
	Info         ingest.WAVInfo
	ChunkCount   int
	HasData      bool
	HasFmt       bool
	AncillaryIDs []string
	// FrameConsistent 表示 WAV 帧数与包时间线期望长度一致。
	FrameConsistent bool
	DeltaFrames     int64
}

// AnalyzeStructure 对 WAV 信息做结构化复核，并与期望采样总数比对。
func AnalyzeStructure(trackID int, info *ingest.WAVInfo, expectFrames int64) (*StructureReport, error) {
	if info == nil {
		return nil, fmt.Errorf("%w: WAVInfo 为空", model.ErrInvalidInput)
	}
	if err := model.CheckSampleCount(expectFrames); err != nil {
		return nil, err
	}
	rep := &StructureReport{
		TrackID:    trackID,
		Info:       *info,
		ChunkCount: len(info.Chunks),
	}
	for _, ch := range info.Chunks {
		switch ch.ID {
		case "fmt ":
			rep.HasFmt = true
		case "data":
			rep.HasData = true
		default:
			rep.AncillaryIDs = append(rep.AncillaryIDs, ch.ID)
		}
	}
	if !rep.HasFmt || !rep.HasData {
		return nil, fmt.Errorf("%w: 缺少必需块 fmt=%v data=%v",
			model.ErrRiffStructure, rep.HasFmt, rep.HasData)
	}
	rep.DeltaFrames = info.TotalFrames - expectFrames
	rep.FrameConsistent = rep.DeltaFrames == 0
	return rep, nil
}

// AnchorReport 描述一条轨道的同步锚点间隔核验结果。
type AnchorReport struct {
	TrackID   int
	Intervals []model.AnchorInterval
	// Available 为 false 时表示锚点不足，间隔输出 N/A。
	Available bool
	MinMs     float64
	MaxMs     float64
}

// AnalyzeAnchors 计算锚点间隔；锚点少于两个时返回 Available=false（N/A）。
func AnalyzeAnchors(tl *model.TrackTimeline) AnchorReport {
	ivs := tl.AnchorIntervals()
	rep := AnchorReport{TrackID: tl.TrackID, Intervals: ivs, Available: len(ivs) > 0}
	if !rep.Available {
		return rep
	}
	rep.MinMs, rep.MaxMs = ivs[0].Ms, ivs[0].Ms
	for _, iv := range ivs[1:] {
		if iv.Ms < rep.MinMs {
			rep.MinMs = iv.Ms
		}
		if iv.Ms > rep.MaxMs {
			rep.MaxMs = iv.Ms
		}
	}
	return rep
}
