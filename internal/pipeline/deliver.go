package pipeline

import (
	"os"
	"sort"
	"strings"
	"time"

	"pairflow/internal/clock"
	"pairflow/internal/index"
	"pairflow/internal/manifest"
	"pairflow/internal/repair"
	"pairflow/internal/report"
)

// Verification 是验证阶段对单轨道的对账结果。
type Verification struct {
	Metrics         report.Metrics
	AlignmentMS     float64
	IndexError      *clock.StageError
}

// Deliverables 汇总交付与验证产物，供 CLI 打印。
type Deliverables struct {
	IndexPath     string
	ManifestPath  string
	Verifications []Verification
	// Failed 为 true 表示索引或清单校验失败（退出码 4）。
	Failed bool
}

// buildDeliverables 从内存修复结果重建索引容器与清单，再立即复读对账。
func buildDeliverables(opt Options, cp *Checkpoint, tracks map[string]*TrackState,
	digest string, res *RunResult) error {
	ids := make([]string, 0, len(tracks))
	for id := range tracks {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	idxPath, err := SafeJoin(opt.OutDir, "delivery.pfidx")
	if err != nil {
		return err
	}
	f, err := os.Create(idxPath)
	if err != nil {
		return clock.Wrap("pipeline.index", clock.KindIndexSeek, "", 0,
			clock.NoSeq, "create index container failed", "检查输出目录", err)
	}
	builder := index.NewBuilder(f)
	if err := builder.Begin([]byte(digest)); err != nil {
		f.Close()
		return err
	}
	descs := make([]index.Descriptor, 0, len(ids))
	mf := &manifest.Manifest{Version: 1, CreatedNano: time.Now().UnixNano(),
		InputDigest: digest, TimelineBaseMS: 0}
	trackID := uint32(1)
	idToTrack := make(map[string]uint32)
	for _, id := range ids {
		t := tracks[id]
		idToTrack[id] = trackID
		rr := t.rec.Result()
		desc := index.Descriptor{TrackID: trackID, DeviceID: id}
		if rr.Timeline != nil && len(rr.Segments) > 0 {
			desc.SampleRate = rr.Segments[0].SampleRate
			desc.Channels = rr.Segments[0].Channels
			desc.BitsPerSample = 16
		}
		uncertain := uncertainSet(rr)
		tr := manifest.Track{
			TrackID: trackID, DeviceID: id,
			SampleRate: desc.SampleRate, Channels: desc.Channels,
			BitsPerSample: desc.BitsPerSample, EntryCount: len(rr.Segments),
			TargetBitrateBps: opt.TargetBitrate,
			SourceOffsets:    make([]int64, 0, len(rr.Segments)),
			UncertainRanges:  rr.Uncertain,
		}
		for _, s := range rr.Segments {
			off, length, werr := builder.AddPCM(s.Data)
			if werr != nil {
				f.Close()
				return werr
			}
			flags := uint32(0)
			if uncertain[s.StartSample] {
				flags |= index.FlagUncertain
			}
			desc.Entries = append(desc.Entries, index.Entry{
				TrackID: trackID, StartSample: s.StartSample, SampleCount: s.SampleCount,
				DataOffset: off, DataLength: length, SourceOffset: s.SourceOffset,
				RTPSeq: s.RTPSeq, Flags: flags,
			})
			tr.SourceOffsets = append(tr.SourceOffsets, s.SourceOffset)
		}
		if rr.Timeline != nil {
			tr.StartSample = rr.Timeline.FirstSample()
			tr.EndSample = rr.Timeline.NextSample()
			tr.TotalSamples = rr.Timeline.TotalSamples()
			tr.MissingSamples = sumLength(rr.Uncertain)
		}
		descs = append(descs, desc)
		mf.Tracks = append(mf.Tracks, tr)
		trackID++
	}
	if _, err := builder.Finalize(descs); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	res.IndexPath = idxPath

	mp, err := manifest.WriteAtomic(opt.OutDir, "manifest.json", mf)
	if err != nil {
		return err
	}
	res.ManifestPath = mp

	return verifyDeliverables(opt, idxPath, mf, tracks, ids, idToTrack, res)
}

// verifyDeliverables 复读索引容器，校验结构/跳转并与清单对账。
func verifyDeliverables(opt Options, idxPath string, mf *manifest.Manifest,
	tracks map[string]*TrackState, ids []string, idToTrack map[string]uint32,
	res *RunResult) error {
	rf, err := os.Open(idxPath)
	if err != nil {
		return clock.Wrap("pipeline.verify", clock.KindIndexSeek, "", 0,
			clock.NoSeq, "reopen index container failed", "重建交付容器", err)
	}
	defer rf.Close()
	idxEntries, mdatBase, fileDigest, err := index.ReadFile(rf)
	if err != nil {
		return err
	}
	storedDigest := strings.TrimRight(string(fileDigest), "\x00")
	if storedDigest != res.Digest {
		return clock.New("pipeline.verify", clock.KindCheckpointMismatch, "", 0,
			clock.NoSeq, "index digest disagrees with input digest", "重建交付容器")
	}
	var fatal []*clock.StageError
	align := map[uint32]float64{}
	for _, id := range ids {
		tid := idToTrack[id]
		es := idxEntries[tid]
		if se := index.VerifyEntries(tid, es); se != nil {
			fatal = append(fatal, se)
			continue
		}
		if se := verifySeekSamples(rf, mdatBase, es); se != nil {
			fatal = append(fatal, se)
			continue
		}
		rr := tracks[id].rec.Result()
		m := report.Compute(rr, es, report.Options{TargetBitrateBps: opt.TargetBitrate})
		am := report.ManifestAlignment(mf, idxEntries)
		align[tid] = am[tid]
		m.ManifestAlignmentMS = am[tid]
		tr := mf.TrackByID(tid)
		if tr != nil {
			tr.ActualBitrateBps = m.ActualBitrateBps
		}
		if am[tid] > 2 {
			fatal = append(fatal, clock.New("pipeline.verify", clock.KindManifestAlignment,
				id, 0, clock.NoSeq, "manifest alignment exceeds 2ms", "重建清单与索引"))
		}
		res.Tracks = append(res.Tracks, tracks[id])
	}
	// 把实际码率回写后的清单再原子替换一次，保证落盘清单自洽。
	if _, err := manifest.WriteAtomic(opt.OutDir, "manifest.json", mf); err != nil {
		return err
	}
	if len(fatal) > 0 {
		return fatal[0]
	}
	_ = align
	return nil
}

// verifySeekSamples 随机抽取若干索引项，按 mdat 偏移跳转读回并核对长度。
func verifySeekSamples(rf *os.File, mdatBase int64, es []index.Entry) *clock.StageError {
	if len(es) == 0 {
		return nil
	}
	picks := uniquePicks(len(es))
	for _, i := range picks {
		e := es[i]
		buf, err := index.ReadAt(rf, mdatBase, e.DataOffset, int(e.DataLength))
		if err != nil {
			return clock.Wrap("pipeline.verify", clock.KindIndexSeek, "",
				e.SourceOffset, e.RTPSeq, "random seek read failed", "重建交付容器", err)
		}
		if int64(len(buf)) != e.DataLength {
			return clock.New("pipeline.verify", clock.KindIndexSeek, "",
				e.SourceOffset, e.RTPSeq, "random seek returned wrong sample length",
				"重建交付容器")
		}
	}
	return nil
}

func uniquePicks(n int) []int {
	if n <= 3 {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	}
	// 固定伪随机抽取（确定性、无外部依赖）：首、中、尾加两个散布点。
	return []int{0, n / 4, n / 2, (3 * n) / 4, n - 1}
}

func uncertainSet(rr *repair.Result) map[int64]bool {
	out := make(map[int64]bool)
	for _, u := range rr.Uncertain {
		if u.Length() > 0 {
			out[u.StartSample] = true
		}
	}
	return out
}

func sumLength(rs []clock.UncertainRange) int64 {
	var n int64
	for _, r := range rs {
		if r.Length() > 0 {
			n += r.Length()
		}
	}
	return n
}
