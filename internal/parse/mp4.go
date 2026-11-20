// Package parse 流式解析 MP4 箱结构与样本表，以及 RTP 时间戳映射。
// 解析错误一律携带输入原始字节偏移。
package parse

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"

	mp4 "github.com/abema/go-mp4"

	"mediarepair/internal/fault"
	"mediarepair/internal/model"
)

// containerBoxes 是需要递归展开的箱类型。
var containerBoxes = map[mp4.BoxType]bool{
	mp4.BoxTypeMoov(): true, mp4.BoxTypeTrak(): true, mp4.BoxTypeMdia(): true,
	mp4.BoxTypeMinf(): true, mp4.BoxTypeStbl(): true, mp4.BoxTypeDinf(): true,
	mp4.BoxTypeEdts(): true, mp4.BoxTypeMoof(): true, mp4.BoxTypeTraf(): true,
	mp4.BoxTypeMvex(): true, mp4.BoxTypeUdta(): true,
}

// WalkBoxes 校验 MP4 箱结构：截断、非法长度都会在出错位置报告原始字节偏移。
func WalkBoxes(rs io.ReadSeeker, fileSize int64) error {
	var lastOffset int64
	sawMoov := false
	_, err := mp4.ReadBoxStructure(rs, func(h *mp4.ReadHandle) (interface{}, error) {
		bi := &h.BoxInfo
		if bi.Size < bi.HeaderSize || bi.Size < 8 {
			return nil, fault.New(fault.StageParse, 0, int64(bi.Offset), false,
				fmt.Sprintf("非法箱长度 size=%d type=%s", bi.Size, bi.Type))
		}
		if int64(bi.Offset)+int64(bi.Size) > fileSize {
			return nil, fault.New(fault.StageParse, 0, int64(bi.Offset), false,
				fmt.Sprintf("箱超出文件末尾（截断）type=%s size=%d", bi.Type, bi.Size))
		}
		lastOffset = int64(bi.Offset) + int64(bi.Size)
		if bi.Type == mp4.BoxTypeMoov() {
			sawMoov = true
		}
		if containerBoxes[bi.Type] {
			return h.Expand()
		}
		return nil, nil
	})
	if err != nil {
		if _, ok := fault.As(err); ok {
			return err
		}
		return fault.Wrap(fault.StageParse, 0, lastOffset, false, "箱结构解析失败", err)
	}
	if !sawMoov {
		return fault.New(fault.StageParse, 0, 0, false, "缺少 moov 箱")
	}
	return nil
}

// wrapDomain 是 32 位时间戳域大小。
const wrapDomain = uint64(1) << 32

// trackTable 是一条轨道的样本表。
type trackTable struct {
	track   model.Track
	samples []model.Sample // Data 字段为空，按需从文件读取
	sizes   []uint32
	keys    []uint64 // 回绕展开后的统一时间基准 DTS，用于跨轨归并
}

// MP4File 是已解析索引的 MP4 输入。
type MP4File struct {
	Path   string
	Size   int64
	f      *os.File
	tracks []trackTable
	total  uint64
}

// OpenMP4 打开并解析 MP4 的箱结构与样本表（不读样本负载）。
func OpenMP4(path string) (*MP4File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fault.Wrap(fault.StageIO, 0, -1, false, "打开输入失败", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fault.Wrap(fault.StageIO, 0, -1, false, "读取文件信息失败", err)
	}
	if err := WalkBoxes(f, st.Size()); err != nil {
		f.Close()
		return nil, err
	}
	mf := &MP4File{Path: path, Size: st.Size(), f: f}
	if err := mf.buildTables(); err != nil {
		f.Close()
		return nil, err
	}
	return mf, nil
}

// Close 关闭底层文件。
func (m *MP4File) Close() error { return m.f.Close() }

// Tracks 返回轨道列表。
func (m *MP4File) Tracks() []model.Track {
	out := make([]model.Track, 0, len(m.tracks))
	for _, t := range m.tracks {
		out = append(out, t.track)
	}
	return out
}

// TotalSamples 返回样本总数。
func (m *MP4File) TotalSamples() uint64 { return m.total }

// ValidateSamples 校验所有样本区间落在文件内。越界视为可恢复的输入截断，
// 返回首个违规样本的原始字节偏移与轨道。
func (m *MP4File) ValidateSamples() error {
	for _, tt := range m.tracks {
		for i := range tt.samples {
			off := uint64(tt.samples[i].SourceOffset)
			sz := uint64(tt.sizes[i])
			if off+sz < off || off+sz > uint64(m.Size) {
				return fault.New(fault.StageParse, tt.track.ID, tt.samples[i].SourceOffset, true,
					fmt.Sprintf("样本 %d 超出文件末尾（输入截断）", i))
			}
		}
	}
	return nil
}

// TrackByID 查找轨道。
func (m *MP4File) TrackByID(id uint32) (model.Track, bool) {
	for _, tt := range m.tracks {
		if tt.track.ID == id {
			return tt.track, true
		}
	}
	return model.Track{}, false
}

// TrackSampleCount 返回指定轨道的样本数。
func (m *MP4File) TrackSampleCount(trackID uint32) int {
	for _, tt := range m.tracks {
		if tt.track.ID == trackID {
			return len(tt.samples)
		}
	}
	return 0
}

// buildTables 提取每条轨道的 stts/stsc/stsz/stco/stss 并展开为样本表。
func (m *MP4File) buildTables() error {
	if _, err := m.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	trakBoxes, err := mp4.ExtractBoxes(m.f, nil, []mp4.BoxPath{
		{mp4.BoxTypeMoov(), mp4.BoxTypeTrak()},
	})
	if err != nil {
		return fault.Wrap(fault.StageParse, 0, -1, false, "提取 trak 失败", err)
	}
	if len(trakBoxes) == 0 || len(trakBoxes) > model.MaxTracks {
		return fault.New(fault.StageParse, 0, -1, false,
			fmt.Sprintf("轨道数 %d 超出上限 [1,%d]", len(trakBoxes), model.MaxTracks))
	}
	for _, tb := range trakBoxes {
		tt, err := m.buildTrack(tb)
		if err != nil {
			return err
		}
		m.total += uint64(len(tt.samples))
		if m.total > model.MaxSamplesPerRun {
			return fault.New(fault.StageParse, tt.track.ID, -1, false, "样本总数超出上限")
		}
		m.tracks = append(m.tracks, tt)
	}
	return nil
}

func (m *MP4File) buildTrack(trak *mp4.BoxInfo) (trackTable, error) {
	bips, err := mp4.ExtractBoxesWithPayload(m.f, trak, []mp4.BoxPath{
		{mp4.BoxTypeTkhd()},
		{mp4.BoxTypeMdia(), mp4.BoxTypeMdhd()},
		{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStts()},
		{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsc()},
		{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsz()},
		{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStco()},
		{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStss()},
		{mp4.BoxTypeMdia(), mp4.BoxTypeHdlr()},
	})
	if err != nil {
		return trackTable{}, fault.Wrap(fault.StageParse, 0, int64(trak.Offset), false, "提取样本表失败", err)
	}
	var tkhd *mp4.Tkhd
	var mdhd *mp4.Mdhd
	var stts *mp4.Stts
	var stsc *mp4.Stsc
	var stsz *mp4.Stsz
	var stco *mp4.Stco
	var stss *mp4.Stss
	var hdlr *mp4.Hdlr
	for _, bip := range bips {
		switch p := bip.Payload.(type) {
		case *mp4.Tkhd:
			tkhd = p
		case *mp4.Mdhd:
			mdhd = p
		case *mp4.Stts:
			stts = p
		case *mp4.Stsc:
			stsc = p
		case *mp4.Stsz:
			stsz = p
		case *mp4.Stco:
			stco = p
		case *mp4.Stss:
			stss = p
		case *mp4.Hdlr:
			hdlr = p
		}
	}
	if tkhd == nil || mdhd == nil || stts == nil || stsc == nil || stsz == nil || stco == nil {
		return trackTable{}, fault.New(fault.StageParse, 0, int64(trak.Offset), false, "样本表不完整（缺 tkhd/mdhd/stts/stsc/stsz/stco）")
	}
	if mdhd.Timescale == 0 || mdhd.Timescale > model.MaxTimescale {
		return trackTable{}, fault.New(fault.StageParse, tkhd.TrackID, int64(trak.Offset), false,
			fmt.Sprintf("时间基准溢出或非法: timescale=%d", mdhd.Timescale))
	}
	kind := model.TrackVideo
	if hdlr != nil && hdlr.HandlerType == [4]byte{'s', 'o', 'u', 'n'} {
		kind = model.TrackAudio
	}
	tt := trackTable{track: model.Track{ID: tkhd.TrackID, Kind: kind, Timescale: mdhd.Timescale}}

	// 展开 stts → 每样本时长；stsz → 每样本尺寸。
	count := int(stsz.SampleCount)
	if stsz.SampleSize != 0 && count == 0 {
		return trackTable{}, fault.New(fault.StageParse, tt.track.ID, int64(trak.Offset), false, "stsz 样本数为 0")
	}
	deltas := make([]uint32, 0, count)
	for _, e := range stts.Entries {
		for i := uint32(0); i < e.SampleCount; i++ {
			deltas = append(deltas, e.SampleDelta)
		}
	}
	if len(deltas) != count {
		return trackTable{}, fault.New(fault.StageParse, tt.track.ID, int64(trak.Offset), false,
			fmt.Sprintf("stts 样本数 %d 与 stsz %d 不一致", len(deltas), count))
	}
	// stsc + stco → 每样本字节偏移与尺寸。
	offsets := make([]uint64, 0, count)
	sizes := make([]uint32, 0, count)
	sampleIdx := 0
	for ci, chunkOff := range stco.ChunkOffset {
		spc := samplesPerChunk(stsc, uint32(ci)+1)
		off := uint64(chunkOff)
		for j := uint32(0); j < spc && sampleIdx < count; j++ {
			sz := sampleSize(stsz, sampleIdx)
			if sz > model.MaxSampleSize {
				return trackTable{}, fault.New(fault.StageParse, tt.track.ID, int64(off), false, "样本尺寸超出上限")
			}
			offsets = append(offsets, off)
			sizes = append(sizes, sz)
			off += uint64(sz)
			sampleIdx++
		}
	}
	if len(offsets) != count {
		return trackTable{}, fault.New(fault.StageParse, tt.track.ID, int64(trak.Offset), false, "stsc/stco 与样本数不一致")
	}
	key := map[int]bool(nil)
	if stss != nil {
		key = make(map[int]bool, len(stss.SampleNumber))
		for _, n := range stss.SampleNumber {
			key[int(n)-1] = true
		}
	}
	// DTS 在 32 位时间戳域累计：stts 的 delta 可跨过 2^32 回绕。
	// 样本越界不在此处拒绝：流式读取时会以可恢复错误精确报告，从而允许
	// 已验证片段继续输出。verify 可调用 ValidateSamples 做前置全量校验。
	tt.samples = make([]model.Sample, count)
	tt.keys = make([]uint64, count)
	var dts uint32
	var wrapBase uint64
	for i := 0; i < count; i++ {
		if i > 0 {
			prev := uint64(tt.samples[i-1].DTS)
			if uint64(dts) < prev && prev-uint64(dts) > wrapDomain {
				wrapBase += uint64(1) << 32
			}
		}
		isKey := key == nil || key[i]
		tt.samples[i] = model.Sample{
			TrackID: tt.track.ID, Seq: uint64(i), DTS: uint64(dts), CTS: uint64(dts),
			Duration: deltas[i], Keyframe: isKey, SourceOffset: int64(offsets[i]),
		}
		tt.keys[i] = model.MustUnified(tt.track.Timescale, uint64(dts)+wrapBase)
		dts += deltas[i]
	}
	tt.sizes = sizes
	return tt, nil
}

func samplesPerChunk(stsc *mp4.Stsc, chunk uint32) uint32 {
	spc := uint32(0)
	for _, e := range stsc.Entries {
		if e.FirstChunk <= chunk {
			spc = e.SamplesPerChunk
		}
	}
	return spc
}

func sampleSize(stsz *mp4.Stsz, i int) uint32 {
	if stsz.SampleSize != 0 {
		return stsz.SampleSize
	}
	return stsz.EntrySize[i]
}

// StreamSamples 按 DTS 归并各轨道样本，逐个读取负载并回调。
// 任一回调错误都会终止遍历；ctx 取消时及时停止。
func (m *MP4File) StreamSamples(ctx context.Context, emit func(model.Sample) error) error {
	return m.StreamSamplesFiltered(ctx, nil, emit)
}

// StreamSamplesFiltered 与 StreamSamples 相同，但仅当 want 返回 true 时读取
// 样本负载；want 为 nil 时全部读取。断点续跑用它在已验证片段上跳过 I/O。
func (m *MP4File) StreamSamplesFiltered(ctx context.Context, want func(model.Sample) bool, emit func(model.Sample) error) error {
	idx := make([]int, len(m.tracks))
	remaining := m.total
	buf := make([]byte, 0, 4096)
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		// 按统一时间基准（回绕展开后）归并各轨道样本，避免跨 timescale 误比。
		best := -1
		var bestKey uint64
		for ti, tt := range m.tracks {
			if idx[ti] >= len(tt.samples) {
				continue
			}
			k := tt.keys[idx[ti]]
			if best < 0 || k < bestKey || (k == bestKey && tt.track.ID < m.tracks[best].track.ID) {
				best, bestKey = ti, k
			}
		}
		if best < 0 {
			break
		}
		s := m.tracks[best].samples[idx[best]]
		sz := m.tracks[best].sizes[idx[best]]
		idx[best]++
		remaining--
		if want == nil || want(s) {
			if cap(buf) < int(sz) {
				buf = make([]byte, 0, sz)
			}
			buf = buf[:sz]
			if _, err := m.f.ReadAt(buf, s.SourceOffset); err != nil {
				return fault.Wrap(fault.StageParse, s.TrackID, s.SourceOffset, true, "读取样本负载失败", err)
			}
			s.Data = make([]byte, len(buf))
			copy(s.Data, buf)
		}
		if err := emit(s); err != nil {
			return err
		}
	}
	return nil
}

// SortedSamples 返回全部样本（按轨道内顺序），供测试与小输入使用。
func (m *MP4File) SortedSamples() []model.Sample {
	var out []model.Sample
	for _, tt := range m.tracks {
		out = append(out, tt.samples...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DTS != out[j].DTS {
			return out[i].DTS < out[j].DTS
		}
		return out[i].TrackID < out[j].TrackID
	})
	return out
}
