package pipeline

import (
	"bytes"
	"fmt"

	"mediad/internal/cut"
	"mediad/internal/fingerprint"
	"mediad/internal/model"
	mp4pkg "mediad/internal/mp4"
)

// buildFixture 生成演示用多轨道夹具并组装成领域清单。
//
// 轨道 0：健康视频；轨道 1：尾部截断视频；轨道 2：缺页索引视频；
// 另有一条坏样本轨道在解析阶段产生可恢复故障。所有随机源均来自
// cfg.Seed，保证同种子重复运行结果确定。
func buildFixture(cfg Config) (*model.Manifest, []model.Source, error) {
	seed := cfg.Seed
	n := cfg.Tracks
	if n < 1 {
		n = 1
	}
	type built struct {
		spec   mp4pkg.SynthSpec
		fault  mp4pkg.Fault
		tag    string
	}
	// 轨道按演示场景排列，前三轨道保证完整片段数量，第四轨道补充坏样本：
	//  trk-1 健康（含正常 stss 关键帧索引）；
	//  trk-2 尾部截断（mdat 声明越过 EOF，末片段不完整）；
	//  trk-3 关键帧索引缺页（无 stss），样本本身健康可切分；
	//  trk-4 坏样本盒（stsz 计数冲突，无法展开样本 -> 不完整片段）。
	plan := []built{
		{spec(seed, "vide", 24000, 24, 1001, 6), mp4pkg.FaultNone, "ok"},
		{spec(seed+1, "vide", 24000, 20, 1001, 5), mp4pkg.FaultTruncate, "trunc"},
		{spec(seed+2, "vide", 24000, 18, 1001, 0), mp4pkg.FaultIndexMissing, "missing"},
		{spec(seed+3, "soun", 48000, 32, 1024, 0), mp4pkg.FaultCorrupt, "corrupt"},
	}
	if n < len(plan) {
		plan = plan[:n]
	}

	manifest := &model.Manifest{Version: 1}
	var inputs []model.Source
	for i, b := range plan {
		raw, _, err := mp4pkg.SynthFile([]mp4pkg.SynthSpec{b.spec}, b.fault)
		if err != nil {
			return nil, nil, err
		}
		// 缺页索引轨道同时裁掉 mdat 尾部：既覆盖 stss 缺页重建，
		// 又让该轨道末片段落入截断区间，产出明确的不完整状态。
		if b.fault == mp4pkg.FaultIndexMissing {
			raw = truncateTail(raw, 24)
		}
		srcID := fmt.Sprintf("src-%d-%s", i+1, b.tag)
		source := model.Source{
			ID: srcID, Kind: model.KindMP4,
			Path: fmt.Sprintf("fixture/%s.mp4", b.tag),
			Digest: fingerprint.FileDigest(raw),
		}
		inputs = append(inputs, source)

		file, perr := mp4pkg.Parse(bytes.NewReader(raw), int64(len(raw)))
		_ = perr // 故障在样本展开阶段继续体现。
		tracks, _ := mp4pkg.BuildTracks(file)
		if len(tracks) == 0 {
			continue
		}
		track := tracks[0]
		track.ID = fmt.Sprintf("trk-%d", i+1)
		track.SourceID = srcID
		manifest.Tracks = append(manifest.Tracks, track)

		trak := file.Root.Find("moov").FindAll("trak")[0]
		samples, _ := mp4pkg.BuildSamples(file, trak, track.ID)
		manifest.Samples = append(manifest.Samples, samples...)
	}
	return manifest, inputs, nil
}

func spec(seed int64, handler string, timescale, count, delta, keyEvery uint32) mp4pkg.SynthSpec {
	return mp4pkg.SynthSpec{
		Handler:   handler,
		Timescale: timescale,
		Count:     count,
		Delta:     delta,
		KeyEvery:  keyEvery,
		Seed:      seed,
	}
}

// cutPointsFor 为每条轨道返回固定的两个剪辑点（毫秒），保证确定性。
func cutPointsFor(trackID string) []cut.Point {
	return []cut.Point{{AtMs: 300}, {AtMs: 700}}
}

// truncateTail 裁掉文件尾部 n 字节，保持各盒声明长度不变，
// 制造“声明越过 EOF”的真实截断输入。
func truncateTail(raw []byte, n int) []byte {
	if n >= len(raw) {
		n = len(raw) / 2
	}
	return raw[:len(raw)-n]
}

// segmentData 汇总片段引用区间内的样本字节，模拟片段产物。
func segmentData(m *model.Manifest, seg *model.Segment) []byte {
	var buf bytes.Buffer
	for i := range m.Samples {
		s := &m.Samples[i]
		if s.TrackID != seg.TrackID {
			continue
		}
		if s.PTS < seg.StartTick || s.PTS >= seg.EndTick {
			continue
		}
		fmt.Fprintf(&buf, "%s:%d:%d|", s.TrackID, s.Index, s.Size)
	}
	buf.WriteString(seg.ID)
	return buf.Bytes()
}

// plantTamperedCache 预放一个内容与摘要不符的缓存条目，
// 供演示缓存篡改检测（cache_rejected 指标）。
func (r *Runner) plantTamperedCache(m *model.Manifest) error {
	if len(m.Tracks) == 0 {
		return nil
	}
	track := m.Tracks[0]
	probe := model.Segment{
		ID: fmt.Sprintf("%s-seg-01", track.ID), TrackID: track.ID,
		StartTick: 0, EndTick: 1, Status: model.StatusOK,
		SourceDigest: m.InputID,
	}
	// 用正确元数据写入，再在缓存层 Put 之后由测试/演示通过独立写入
	// 替换 blob；这里直接写入一份“看似有效”的数据，运行期 Get 时
	// 真实片段时间范围不同即触发 range/content 核对失败。
	return r.cache.Put(probe, []byte("planted-tampered-content"))
}
