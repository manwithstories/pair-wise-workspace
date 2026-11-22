package parse

import (
	"io"

	"github.com/abema/go-mp4"
)

// readSync 读取 stss 关键帧索引。缺失或不完整时标记 IndexGap，
// 由 RebuildKeyframeIndex 通过样本表/净荷扫描重建。
// 返回 syncSet（1 基样本号）、syncKnown（可信与否）、gap（索引缺页）。
func readSync(r io.ReadSeeker, stbl *RawBox, n uint32,
	ti *TrackInfo, rep *Report) (map[uint32]bool, bool, bool) {
	b := stbl.Find("stss")
	if b == nil {
		rep.add(&Error{Level: LevelRecoverable, BoxType: "stss",
			TrackID: ti.TrackID, Message: "keyframe index missing"})
		return map[uint32]bool{}, false, true
	}
	var stss mp4.Stss
	if err := Unmarshal(r, b, &stss); err != nil {
		rep.add(&Error{Level: LevelRecoverable, BoxType: "stss",
			TrackID: ti.TrackID, Message: err.Error()})
		return map[uint32]bool{}, false, true
	}
	set := make(map[uint32]bool, len(stss.SampleNumber))
	gap := false
	for _, sn := range stss.SampleNumber {
		if sn == 0 || sn > n {
			gap = true // 索引引用越界样本 => 不完整
			continue
		}
		set[sn] = true
	}
	if len(stss.SampleNumber) == 0 {
		gap = true
	}
	if gap {
		rep.add(&Error{Level: LevelRecoverable, BoxType: "stss",
			TrackID: ti.TrackID, Message: "keyframe index incomplete"})
	}
	return set, !gap, gap
}

// RebuildKeyframeIndex 扫描样本表重建随机访问点。
// 视频轨：样本净荷起始 NAL 单元类型为 IDR（H.264 NAL type 5）视为同步点；
// 无法确认编码或净荷时把边界标为不安全（SyncKnown=false），不生成误导性切片。
func RebuildKeyframeIndex(r io.ReadSeeker, ti *TrackInfo, lim Limits, rep *Report) {
	ti.rawSizes = collectSizes(ti)
	if ti.Kind != "video" {
		// 音频每个样本都可独立解码，视为天然同步点。
		for i := range ti.Samples {
			ti.Samples[i].Sync = true
			ti.Samples[i].SyncKnown = true
		}
		ti.IndexGap = false
		return
	}
	if !isH264(ti.Codec) {
		// 未支持编码：明确报告，不猜测随机访问点。
		rep.add(&Error{Level: LevelSkipped, BoxType: "stss",
			TrackID: ti.TrackID,
			Message: "cannot confirm sync samples for codec " + ti.Codec})
		for i := range ti.Samples {
			ti.Samples[i].Sync = false
			ti.Samples[i].SyncKnown = false
		}
		return
	}
	found := 0
	for i := range ti.Samples {
		s := &ti.Samples[i]
		isIDR, ok := probeH264IDR(r, s, lim)
		if !ok {
			s.SyncKnown = false
			continue
		}
		s.SyncKnown = true
		s.Sync = isIDR
		if isIDR {
			found++
		}
	}
	if found == 0 {
		rep.add(&Error{Level: LevelRecoverable, BoxType: "stss",
			TrackID: ti.TrackID,
			Message: "rebuilt index found no IDR; boundaries unsafe"})
		ti.IndexGap = true
	} else {
		ti.IndexGap = false
	}
}

func collectSizes(ti *TrackInfo) []uint32 {
	out := make([]uint32, len(ti.Samples))
	for i, s := range ti.Samples {
		out[i] = s.Size
	}
	return out
}

func isH264(codec string) bool {
	return codec == "avc1" || codec == "avc3"
}

// probeH264IDR 读取样本前若干字节，按 AVCC 长度前缀取首个 NAL，
// 判断其类型是否为 5（IDR）。返回 ok=false 表示净荷不可读/无法确认。
func probeH264IDR(r io.ReadSeeker, s *Sample, lim Limits) (bool, bool) {
	if s.Size == 0 || s.Offset == 0 {
		return false, false
	}
	n := int64(s.Size)
	if n > lim.MaxPayload {
		n = lim.MaxPayload
	}
	if _, err := r.Seek(int64(s.Offset), io.SeekStart); err != nil {
		return false, false
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(r, head); err != nil {
		return false, false
	}
	// AVCC: 4 字节大端 NAL 长度（简化生成器固定使用 4 字节前缀）。
	nalLen := int64(head[0])<<24 | int64(head[1])<<16 |
		int64(head[2])<<8 | int64(head[3])
	if nalLen <= 0 || nalLen+4 > n {
		return false, false
	}
	nal := make([]byte, 1)
	if _, err := io.ReadFull(r, nal); err != nil {
		return false, false
	}
	return nal[0]&0x1f == 5, true
}

// SyncSamples 返回确认的随机访问点样本序号（1 基）。
func (ti *TrackInfo) SyncSamples() []uint32 {
	var out []uint32
	for _, s := range ti.Samples {
		if s.SyncKnown && s.Sync {
			out = append(out, s.Index)
		}
	}
	return out
}
