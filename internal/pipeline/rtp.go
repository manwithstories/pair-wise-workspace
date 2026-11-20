package pipeline

import (
	"context"
	"os"
	"path/filepath"

	"mediarepair/internal/fault"
	"mediarepair/internal/model"
	"mediarepair/internal/parse"
	"mediarepair/internal/repair"
	"mediarepair/internal/report"
)

// RTPReport 是 RTP 时间戳映射的修复统计。
type RTPReport = report.RTPStat

// processRTP 解析同目录 rtp.dump 的时间戳映射并套用同一修复策略。
// 文件缺失时返回 Parsed=false，不视为错误（RTP 为可选输入）。
func processRTP(ctx context.Context, inputPath string, tl *model.Timeline, strat repair.Strategy) (RTPReport, error) {
	rep := RTPReport{}
	if tl.RTPTrackID == 0 {
		return rep, nil
	}
	path := filepath.Join(filepath.Dir(inputPath), "rtp.dump")
	f, err := os.Open(path)
	if err != nil {
		return rep, nil
	}
	defer f.Close()
	track, ok := tl.TrackByID(tl.RTPTrackID)
	if !ok {
		return rep, fault.New(fault.StageParse, tl.RTPTrackID, -1, false, "RTP 轨道 ID 不在时间线中")
	}
	// 仅用该轨道构建修复器：RTP 时间戳是 32 位域，允许回绕展开。
	rtpTL := &model.Timeline{Tracks: []model.Track{track}}
	clock := repair.NewClock(rtpTL, strat)
	first := true
	err = parse.ParseRTPDump(ctx, f, func(p parse.RTPSample) error {
		if first {
			rep.FirstTS = p.Timestamp
			first = false
		}
		rep.LastTS = p.Timestamp
		rep.Packets++
		_, err := clock.Process(model.Sample{
			TrackID: track.ID, Seq: uint64(p.Seq), DTS: uint64(p.Timestamp),
			CTS: uint64(p.Timestamp), Duration: 0, SourceOffset: p.Offset,
		})
		return err
	})
	if err != nil {
		return rep, err
	}
	stats := clock.Stats()
	if len(stats) == 1 {
		rep.Wraps = stats[0].Wraps
		rep.Duplicates = stats[0].Duplicates
		rep.Regressions = stats[0].Regressions
	}
	rep.File = path
	rep.TrackID = track.ID
	rep.Parsed = true
	return rep, nil
}
