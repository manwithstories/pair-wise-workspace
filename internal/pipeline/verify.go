package pipeline

import (
	"os"
	"strings"

	"pairflow/internal/clock"
	"pairflow/internal/index"
	"pairflow/internal/manifest"
	"pairflow/internal/repair"
	"pairflow/internal/report"
)

// VerifyReport 是 verify 命令对整个交付目录的对账结果。
type VerifyReport struct {
	Metrics        []report.Metrics
	AlignmentByID  map[uint32]float64
	HasUnrecoverable bool
	StageErrors    []*clock.StageError
}

// VerifyDirectory 复读交付目录中的索引容器与清单，重算全部质量与对齐指标。
// 返回的错误为索引/清单级失败（退出码 4）；媒体不确定区间通过报告字段表达（退出码 3）。
func VerifyDirectory(outDir, manifestPath string, targetBitrate int64) (*VerifyReport, error) {
	idxPath, err := SafeJoin(outDir, "delivery.pfidx")
	if err != nil {
		return nil, err
	}
	rf, err := os.Open(idxPath)
	if err != nil {
		return nil, clock.Wrap("pipeline.verify", clock.KindIndexSeek, "", 0,
			clock.NoSeq, "index container missing: "+idxPath, "先运行 repair/demo", err)
	}
	defer rf.Close()
	idxEntries, mdatBase, fileDigest, err := index.ReadFile(rf)
	if err != nil {
		return nil, err
	}
	mf, err := manifest.Load(manifestPath)
	if err != nil {
		return nil, err
	}
	cp, err := LoadCheckpoint(outDir)
	if err != nil && cp == nil {
		return nil, err
	}
	if cp != nil && strings.TrimRight(string(fileDigest), "\x00") != cp.InputDigest {
		return nil, clock.New("pipeline.verify", clock.KindCheckpointMismatch, "", 0,
			clock.NoSeq, "checkpoint digest disagrees with index container",
			"重新运行修复阶段")
	}

	out := &VerifyReport{AlignmentByID: map[uint32]float64{}}
	align := report.ManifestAlignment(mf, idxEntries)
	for _, tr := range mf.Tracks {
		es := idxEntries[tr.TrackID]
		if se := index.VerifyEntries(tr.TrackID, es); se != nil {
			out.StageErrors = append(out.StageErrors, se)
			return out, se
		}
		if se := verifySeekSamples(rf, mdatBase, es); se != nil {
			out.StageErrors = append(out.StageErrors, se)
			return out, se
		}
		// 从清单、索引与 mdat 重建内存修复结果用于指标重算。
		rr := reconstructResult(rf, mdatBase, tr, es)
		m := report.Compute(rr, es, report.Options{TargetBitrateBps: targetBitrate})
		m.ManifestAlignmentMS = align[tr.TrackID]
		out.Metrics = append(out.Metrics, m)
		out.AlignmentByID[tr.TrackID] = align[tr.TrackID]
		if len(rr.Uncertain) > 0 {
			out.HasUnrecoverable = rr.HasIndeterminate() || hasAnyUncertain(rr)
		}
		if align[tr.TrackID] > 2 {
			se := clock.New("pipeline.verify", clock.KindManifestAlignment, tr.DeviceID,
				0, clock.NoSeq, "manifest alignment exceeds 2ms", "重建清单与索引")
			out.StageErrors = append(out.StageErrors, se)
			return out, se
		}
	}
	return out, nil
}

// reconstructResult 从清单、索引项与 mdat 重建修复结果（PCM 从容器读回），
// 使 verify 不依赖当次内存状态即可独立重算指标。
// Timeline 用清单的总采样数与缺失采样数显式填充，不依赖重放 Place()。
func reconstructResult(rf *os.File, mdatBase int64, tr manifest.Track, es []index.Entry) *repair.Result {
	tl := clock.NewTimeline(tr.DeviceID, tr.SampleRate)
	rr := &repair.Result{DeviceID: tr.DeviceID, Timeline: tl}
	for _, u := range tr.UncertainRanges {
		rr.Uncertain = append(rr.Uncertain, u)
		tl.Uncertain = append(tl.Uncertain, u)
	}
	// 用清单声明的时间范围重建时间线游标，保证 TotalSamples() 与缺失率口径正确。
	tl.SeedRange(tr.StartSample, tr.EndSample)
	for _, e := range es {
		var data []byte
		if e.DataLength > 0 {
			buf, rerr := index.ReadAt(rf, mdatBase, e.DataOffset, int(e.DataLength))
			if rerr == nil {
				data = buf
			}
		}
		rr.Segments = append(rr.Segments, repair.Segment{
			StartSample: e.StartSample, SampleCount: e.SampleCount,
			SourceOffset: e.SourceOffset, RTPSeq: e.RTPSeq,
			Channels: tr.Channels, SampleRate: tr.SampleRate, Data: data,
		})
	}
	return rr
}

func hasAnyUncertain(rr *repair.Result) bool {
	for _, u := range rr.Uncertain {
		if u.Length() > 0 {
			return true
		}
	}
	return false
}
