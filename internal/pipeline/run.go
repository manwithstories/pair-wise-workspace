package pipeline

import (
	"errors"
	"io"
	"os"
	"sort"
	"time"

	"pairflow/internal/clock"
	"pairflow/internal/index"
	"pairflow/internal/repair"
	"pairflow/internal/rtp"
	"pairflow/internal/wav"
)

// Options 控制一次流水线运行。
type Options struct {
	OutDir        string
	CacheBytes    int64 // 单轨缓存上限，<=0 用规格默认 256MB
	InputFiles    []string
	TargetBitrate int64
}

// TrackState 是单设备的运行态与续跑状态。
type TrackState struct {
	DeviceID  string
	reord     *rtp.Reorderer
	rec       *repair.Reconciler
	sidecar   string
	cp        DeviceCheckpoint
	desc      index.Descriptor
	formatSet bool
}

// RunResult 汇总一次运行的结果与各阶段结构化错误。
type RunResult struct {
	Checkpoint   *Checkpoint
	Tracks       []*TrackState
	TrackErrors  []*clock.StageError
	IndexPath    string
	ManifestPath string
	Digest       string
}

// Run 执行扫描/变换/验证编排；存在检查点且摘要匹配时自动续跑。
func Run(opt Options) (*RunResult, error) {
	if opt.CacheBytes <= 0 {
		opt.CacheBytes = MaxTrackCacheBytes
	}
	if err := os.MkdirAll(opt.OutDir, 0o755); err != nil {
		return nil, clock.Wrap("pipeline", clock.KindPathEscape, "", 0,
			clock.NoSeq, "create out dir failed", "检查输出目录", err)
	}
	digest, total, err := digestFiles(opt.InputFiles)
	if err != nil {
		return nil, err
	}
	cp, err := LoadCheckpoint(opt.OutDir)
	if err != nil {
		return nil, err
	}
	resumed := cp != nil
	if resumed {
		if err := cp.VerifyDigest(digest, total); err != nil {
			return nil, err
		}
	} else {
		cp = &Checkpoint{Version: 1, Phase: PhaseRepair, InputDigest: digest, InputBytes: total}
	}

	res := &RunResult{Checkpoint: cp, Digest: digest}
	tracks := make(map[string]*TrackState)
	for _, d := range cp.Devices {
		tracks[d.DeviceID] = restoreTrack(d, opt.CacheBytes)
	}
	getTrack := func(id string) *TrackState {
		if t, ok := tracks[id]; ok {
			return t
		}
		t := newTrack(id, opt, cp)
		tracks[id] = t
		return t
	}

	for fi, fp := range opt.InputFiles {
		wm := watermarkFor(cp, fp)
		f, err := os.Open(fp)
		if err != nil {
			return nil, clock.Wrap("pipeline.scan", clock.KindTruncatedHeader, "", 0,
				clock.NoSeq, "open capture failed: "+fp, "检查输入路径", err)
		}
		reader := rtp.NewCaptureReader(f, 0)
		if _, err := reader.ReadHeader(); err != nil {
			f.Close()
			return nil, err
		}
		replayUntil := wm.Offset
		for {
			rec, perr := reader.Next()
			if errors.Is(perr, io.EOF) {
				break
			}
			if perr != nil {
				f.Close()
				return nil, perr
			}
			if rec.HeaderOffset < replayUntil {
				continue // 续跑：跳过已确认消费的记录
			}
			t := getTrack(rec.DeviceID)
			pkts, _, pushErr := t.reord.Push(rec)
			if pushErr != nil {
				// 单包解析错误不阻断同轨后续包与其他轨道。
				res.TrackErrors = append(res.TrackErrors, mustStage(pushErr))
				continue
			}
			for _, p := range pkts {
				t.feedPacket(p, res)
			}
			// 水位推进到本记录之后，续跑时从下一条记录开始。
			wm.Offset = rec.HeaderOffset + 5 + int64(len(rec.Packet)) +
				int64(1+8+2+len(rec.DeviceID)+4)
			cp.UpdatedNano = time.Now().UnixNano()
		}
		wm.Name = fp
		upsertWatermark(cp, wm)
		_ = fi
	}

	for _, t := range tracks {
		pkts, gaps := t.reord.Flush()
		for _, p := range pkts {
			t.feedPacket(p, res)
		}
		if rr := t.rec.TailGaps(gaps); rr != nil {
			t.syncCheckpoint(cp)
		}
		t.syncCheckpoint(cp)
	}
	sortCP(cp)
	if err := cp.Save(opt.OutDir); err != nil {
		return nil, err
	}

	// 第二阶段：从 sidecar 重建交付容器与清单，并执行验证。
	if err := buildDeliverables(opt, cp, tracks, digest, res); err != nil {
		return res, err
	}
	cp.Phase = PhaseDone
	if err := cp.Save(opt.OutDir); err != nil {
		return res, err
	}
	_ = resumed
	return res, nil
}

func newTrack(id string, opt Options, cp *Checkpoint) *TrackState {
	side, _ := SafeJoin(opt.OutDir, "work-"+id+".pcm")
	t := &TrackState{
		DeviceID: id, sidecar: side,
		cp: DeviceCheckpoint{DeviceID: id},
	}
	cfg := rtp.DefaultConfig()
	cfg.MaxCacheBytes = opt.CacheBytes
	cfg.OnCheckpoint = func(deviceID string, cached int64) error {
		return cp.Save(opt.OutDir)
	}
	t.reord = rtp.NewReorderer(id, cfg)
	t.rec = repair.NewReconciler(id)
	t.desc.TrackID = 0 // 轨道 ID 在交付阶段按设备排序统一分配
	return t
}

func restoreTrack(d DeviceCheckpoint, cache int64) *TrackState {
	cfg := rtp.DefaultConfig()
	cfg.MaxCacheBytes = cache
	t := &TrackState{DeviceID: d.DeviceID, cp: d,
		reord: rtp.NewReorderer(d.DeviceID, cfg), rec: repair.NewReconciler(d.DeviceID)}
	t.reord.RestoreStats(d.Stats) // 恢复统计基线（含缺口列表）
	return t
}

// feedPacket 把重排后的 RTP 包解析为 WAV 块并送入修复层，
// 可恢复块的 PCM 追加到轨道 sidecar。
func (t *TrackState) feedPacket(p rtp.Packet, res *RunResult) {
	blk := wav.ParseBlock(t.DeviceID, p.FileOffset, p.Seq, p.RawSeq,
		p.Timestamp, p.ArrivalNano, p.Payload)
	rr := t.rec.HandleBlock(blk, t.reord.Stats().Gaps)
	if blk.Format != nil && !t.formatSet {
		t.desc.DeviceID = t.DeviceID
		t.desc.SampleRate, t.desc.Channels, t.desc.BitsPerSample =
			blk.Format.SampleRate, blk.Format.Channels, blk.Format.BitsPerSample
		t.formatSet = true
	}
	for _, e := range blk.Errors {
		res.TrackErrors = append(res.TrackErrors, e)
	}
	if len(blk.Data) > 0 {
		if err := appendSidecar(t.sidecar, blk.Data); err != nil {
			res.TrackErrors = append(res.TrackErrors, mustStage(err))
			return
		}
		t.cp.WorkPCMBytes += int64(len(blk.Data))
		t.cp.SidecarEntries++
	}
	t.cp.Stats = t.reord.Stats()
	t.cp.LastSeq, t.cp.HaveSeq = blk.RTPSeq, true
	if rr.Timeline != nil {
		t.cp.NextSample, t.cp.TotalSamples = rr.Timeline.NextSample(), rr.Timeline.TotalSamples()
		t.cp.WrapEvents, t.cp.Repaired = rr.Timeline.WrapEvents, rr.Timeline.Repaired
	}
}

func (t *TrackState) syncCheckpoint(cp *Checkpoint) {
	t.cp.Stats = t.reord.Stats()
	for i := range cp.Devices {
		if cp.Devices[i].DeviceID == t.DeviceID {
			cp.Devices[i] = t.cp
			return
		}
	}
	cp.Devices = append(cp.Devices, t.cp)
}

func mustStage(err error) *clock.StageError {
	if se, ok := clock.AsStageError(err); ok {
		return se
	}
	return clock.New("pipeline", clock.KindIndeterminate, "", 0, clock.NoSeq, err.Error(), "检查上游输入")
}

func watermarkFor(cp *Checkpoint, name string) FileWatermark {
	for _, w := range cp.Watermarks {
		if w.Name == name {
			return w
		}
	}
	return FileWatermark{Name: name}
}

func upsertWatermark(cp *Checkpoint, wm FileWatermark) {
	for i := range cp.Watermarks {
		if cp.Watermarks[i].Name == wm.Name {
			cp.Watermarks[i] = wm
			return
		}
	}
	cp.Watermarks = append(cp.Watermarks, wm)
}

func sortCP(cp *Checkpoint) {
	sort.Slice(cp.Devices, func(i, j int) bool { return cp.Devices[i].DeviceID < cp.Devices[j].DeviceID })
	sort.Slice(cp.Watermarks, func(i, j int) bool { return cp.Watermarks[i].Name < cp.Watermarks[j].Name })
}

// ReordererStats 暴露重排统计，供 CLI 摘要使用。
func (t *TrackState) ReordererStats() rtp.Stats { return t.reord.Stats() }

// RepairedCount 返回时间线修复的时间戳数量。
func (t *TrackState) RepairedCount() int {
	if rr := t.rec.Result(); rr.Timeline != nil {
		return rr.Timeline.Repaired
	}
	return 0
}

// appendSidecar 以 O_APPEND 方式把 PCM 追加到工作文件。
func appendSidecar(sidecar string, pcm []byte) error {
	f, err := os.OpenFile(sidecar, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return clock.Wrap("pipeline.repair", clock.KindCacheLimit, "", 0,
			clock.NoSeq, "open sidecar failed", "检查输出目录", err)
	}
	defer f.Close()
	if _, err := f.Write(pcm); err != nil {
		return clock.Wrap("pipeline.repair", clock.KindCacheLimit, "", 0,
			clock.NoSeq, "write sidecar failed", "检查磁盘空间", err)
	}
	return nil
}
