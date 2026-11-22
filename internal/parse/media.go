package parse

import (
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"fieldcut/internal/timeline"
)

// Sample 是从样本表恢复的单个样本描述。
type Sample struct {
	Index       uint32 // 1 基序号
	Offset      uint64 // 文件绝对字节偏移
	Size        uint32
	TimeNs      int64  // 共同时间线纳秒
	DurationNs  int64
	RawTick     uint64 // 原始媒体时基 tick（映射前）
	Sync        bool   // 随机访问点
	SyncKnown   bool   // Sync 标记是否可信
}

// TrackInfo 是一条 trak 的语义解析结果。
type TrackInfo struct {
	TrackID   uint32
	Kind      string
	Codec     string
	Timescale uint32
	EpochNs   int64
	Samples   []Sample
	Raw       *RawBox
	Clock     *timeline.Clock
	IndexGap  bool // 关键帧索引缺失或不完整
	Supported bool
	rawSizes  []uint32 // stsz 原始尺寸，供 chunk 布局展开
}

// ParseTracks 从盒树中解析全部 trak，顺序与文件一致。
// 媒体时基与样本先于跨轨事件解析（由调用方按依赖图排序驱动）。
// fileSize 用于识别尾部截断造成的越界样本（0 表示不做边界检查）。
func ParseTracks(r io.ReadSeeker, roots []*RawBox, lim Limits,
	rep *Report) ([]*TrackInfo, error) {
	fileSize := int64(0)
	if sz, err := r.Seek(0, io.SeekEnd); err == nil {
		fileSize = sz
	}
	var out []*TrackInfo
	for _, n := range roots {
		if n.Type() != "moov" {
			continue
		}
		for _, trak := range n.All("trak") {
			ti, err := parseOneTrack(r, trak, lim, rep, fileSize)
			if err != nil {
				if fe, ok := err.(*Error); ok && fe.Level == LevelFatal {
					return nil, err
				}
				rep.add(err.(*Error))
				continue
			}
			if !ti.Supported {
				continue // 已在 parseOneTrack 中记录 skipped
			}
			out = append(out, ti)
		}
	}
	// 无可用轨道不判致命：跳过原因已在 Report 中逐条记录。
	return out, nil
}

func parseOneTrack(r io.ReadSeeker, trak *RawBox, lim Limits,
	rep *Report, fileSize int64) (*TrackInfo, error) {
	tkhdBox := trak.Find("tkhd")
	mdhdBox := trak.Find("mdia", "mdhd")
	hdlrBox := trak.Find("mdia", "hdlr")
	if tkhdBox == nil || mdhdBox == nil || hdlrBox == nil {
		return nil, &Error{Level: LevelRecoverable, BoxType: "trak",
			Offset: int64(trak.Info.Offset), Message: "missing tkhd/mdhd/hdlr"}
	}
	var tkhd mp4.Tkhd
	var mdhd mp4.Mdhd
	var hdlr mp4.Hdlr
	if err := Unmarshal(r, tkhdBox, &tkhd); err != nil {
		return nil, &Error{Level: LevelFatal, BoxType: "tkhd", Message: err.Error()}
	}
	if err := Unmarshal(r, mdhdBox, &mdhd); err != nil {
		return nil, &Error{Level: LevelFatal, BoxType: "mdhd", Message: err.Error()}
	}
	if err := Unmarshal(r, hdlrBox, &hdlr); err != nil {
		return nil, &Error{Level: LevelRecoverable, BoxType: "hdlr", Message: err.Error()}
	}
	kind, supported := handlerKind(string(hdlr.HandlerType[:]))
	clock, err := timeline.NewClock(tkhd.TrackID, kind, mdhd.Timescale, 0)
	if err != nil {
		return nil, &Error{Level: LevelFatal, BoxType: "mdhd",
			TrackID: tkhd.TrackID, Message: err.Error()}
	}
	ti := &TrackInfo{
		TrackID:   tkhd.TrackID,
		Kind:      kind,
		Codec:     codecOf(r, trak),
		Timescale: mdhd.Timescale,
		Raw:       trak,
		Clock:     clock,
		Supported: supported,
	}
	if !supported {
		rep.add(&Error{Level: LevelSkipped, BoxType: "hdlr",
			TrackID: tkhd.TrackID,
			Message: fmt.Sprintf("unsupported handler %q", hdlr.HandlerType)})
		return ti, nil
	}
	if err := ti.readSamples(r, trak, lim, rep, fileSize); err != nil {
		return nil, err
	}
	return ti, nil
}

func handlerKind(h string) (string, bool) {
	switch h {
	case "vide":
		return timeline.KindVideo, true
	case "soun":
		return timeline.KindAudio, true
	case "sbtl", "text", "subt":
		return timeline.KindSubtitle, true
	}
	return "unknown:" + h, false
}

func codecOf(r io.ReadSeeker, trak *RawBox) string {
	stsd := trak.Find("mdia", "minf", "stbl", "stsd")
	if stsd == nil {
		return "unknown"
	}
	// stsd 净荷：FullBox(4) + entry_count(4) + 样本条目盒
	// （条目 size 4 字节 + 类型码 4 字节），类型码位于净荷 +12。
	payload, err := ReadPayload(r, stsd, DefaultLimits())
	if err != nil || len(payload) < 16 {
		return "unknown"
	}
	return string(payload[12:16])
}

// readSamples 展开 stts/stsc/stsz/stco|co64 为逐样本记录。
// fileSize>0 时剔除超出文件边界的尾部样本（截断恢复），并逐条记录。
func (ti *TrackInfo) readSamples(r io.ReadSeeker, trak *RawBox,
	lim Limits, rep *Report, fileSize int64) error {
	stbl := trak.Find("mdia", "minf", "stbl")
	if stbl == nil {
		return &Error{Level: LevelRecoverable, BoxType: "stbl",
			TrackID: ti.TrackID, Message: "sample table missing"}
	}
	sizes, err := readSizes(r, stbl, lim, ti, rep)
	if err != nil {
		return err
	}
	ti.rawSizes = sizes
	deltas, err := readDeltas(r, stbl, ti, rep)
	if err != nil {
		return err
	}
	chunks, err := readChunks(r, stbl, ti, rep)
	if err != nil {
		return err
	}
	if len(sizes) > lim.MaxSampleN {
		return &Error{Level: LevelFatal, BoxType: "stsz", TrackID: ti.TrackID,
			Message: fmt.Sprintf("sample count %d over limit", len(sizes))}
	}
	syncSet, syncKnown, gap := readSync(r, stbl, uint32(len(sizes)), ti, rep)
	ti.IndexGap = gap
	ti.Samples = make([]Sample, len(sizes))
	var tick uint64
	for i := range sizes {
		var d uint32
		if i < len(deltas) {
			d = deltas[i]
		} else {
			rep.add(&Error{Level: LevelRecoverable, BoxType: "stts",
				TrackID: ti.TrackID, RawTick: tick,
				Message: "stts shorter than sample count"})
		}
		ns, _, err := ti.Clock.ToNanos(int64(tick))
		if err != nil {
			rep.add(&Error{Level: LevelRecoverable, BoxType: "timestamp",
				TrackID: ti.TrackID, RawTick: tick, Message: err.Error()})
		}
		durNs, _, _ := ti.Clock.ToNanos(int64(d))
		off, ok := chunks[i]
		if !ok {
			rep.add(&Error{Level: LevelRecoverable, BoxType: "stco",
				TrackID: ti.TrackID, Message: "chunk layout shorter than samples"})
		}
		sync, has := syncSet[uint32(i+1)]
		ti.Samples[i] = Sample{
			Index: uint32(i + 1), Offset: off, Size: sizes[i],
			TimeNs: ns, DurationNs: durNs, RawTick: tick,
			Sync: sync && has, SyncKnown: syncKnown,
		}
		tick += uint64(d)
	}
	if fileSize > 0 {
		ti.dropOutOfRange(fileSize, rep)
	}
	return nil
}

// dropOutOfRange 移除字节区间越过文件末尾的尾部样本（尾部截断）。
// 保留最后一个完整样本，损坏偏移记入 Report，时间线 tick 不做改写。
func (ti *TrackInfo) dropOutOfRange(fileSize int64, rep *Report) {
	kept := ti.Samples[:0]
	dropped := 0
	for _, s := range ti.Samples {
		end := int64(s.Offset) + int64(s.Size)
		if s.Offset > 0 && end > fileSize {
			dropped++
			rep.add(&Error{Level: LevelRecoverable, BoxType: "stco",
				TrackID: ti.TrackID, Offset: int64(s.Offset), RawTick: s.RawTick,
				Message: "sample beyond truncated file boundary"})
			continue
		}
		kept = append(kept, s)
	}
	ti.Samples = kept
	if dropped > 0 {
		ti.IndexGap = ti.IndexGap || false
	}
}

func readSizes(r io.ReadSeeker, stbl *RawBox, lim Limits,
	ti *TrackInfo, rep *Report) ([]uint32, error) {
	b := stbl.Find("stsz")
	if b == nil {
		return nil, &Error{Level: LevelRecoverable, BoxType: "stsz",
			TrackID: ti.TrackID, Message: "stsz missing"}
	}
	var stsz mp4.Stsz
	if err := Unmarshal(r, b, &stsz); err != nil {
		return nil, &Error{Level: LevelRecoverable, BoxType: "stsz",
			TrackID: ti.TrackID, Message: err.Error()}
	}
	if int(stsz.SampleCount) > lim.MaxSampleN {
		return nil, &Error{Level: LevelFatal, BoxType: "stsz",
			TrackID: ti.TrackID, Message: "sample count over limit"}
	}
	if stsz.SampleSize != 0 {
		out := make([]uint32, stsz.SampleCount)
		for i := range out {
			out[i] = stsz.SampleSize
		}
		return out, nil
	}
	if len(stsz.EntrySize) != int(stsz.SampleCount) {
		rep.add(&Error{Level: LevelRecoverable, BoxType: "stsz",
			TrackID: ti.TrackID, Message: "stsz entry count mismatch"})
	}
	return stsz.EntrySize, nil
}

func readDeltas(r io.ReadSeeker, stbl *RawBox,
	ti *TrackInfo, rep *Report) ([]uint32, error) {
	b := stbl.Find("stts")
	if b == nil {
		return nil, &Error{Level: LevelRecoverable, BoxType: "stts",
			TrackID: ti.TrackID, Message: "stts missing"}
	}
	var stts mp4.Stts
	if err := Unmarshal(r, b, &stts); err != nil {
		return nil, &Error{Level: LevelRecoverable, BoxType: "stts",
			TrackID: ti.TrackID, Message: err.Error()}
	}
	var out []uint32
	for _, e := range stts.Entries {
		for k := uint32(0); k < e.SampleCount; k++ {
			out = append(out, e.SampleDelta)
		}
	}
	return out, nil
}

func readChunks(r io.ReadSeeker, stbl *RawBox,
	ti *TrackInfo, rep *Report) (map[int]uint64, error) {
	var offsets []uint64
	switch {
	case stbl.Find("co64") != nil:
		var box mp4.Co64
		if err := Unmarshal(r, stbl.Find("co64"), &box); err != nil {
			return nil, &Error{Level: LevelRecoverable, BoxType: "co64",
				TrackID: ti.TrackID, Message: err.Error()}
		}
		offsets = box.ChunkOffset
	case stbl.Find("stco") != nil:
		var box mp4.Stco
		if err := Unmarshal(r, stbl.Find("stco"), &box); err != nil {
			return nil, &Error{Level: LevelRecoverable, BoxType: "stco",
				TrackID: ti.TrackID, Message: err.Error()}
		}
		for _, o := range box.ChunkOffset {
			offsets = append(offsets, uint64(o))
		}
	default:
		return nil, &Error{Level: LevelRecoverable, BoxType: "stco",
			TrackID: ti.TrackID, Message: "chunk offset box missing"}
	}
	var stsc mp4.Stsc
	if b := stbl.Find("stsc"); b != nil {
		if err := Unmarshal(r, b, &stsc); err != nil {
			return nil, &Error{Level: LevelRecoverable, BoxType: "stsc",
				TrackID: ti.TrackID, Message: err.Error()}
		}
	}
	out := make(map[int]uint64)
	sampleIdx := 0
	chunkIdx := uint32(0)
	for ci, base := range offsets {
		n := samplesInChunk(&stsc, uint32(ci))
		cur := base
		for k := uint32(0); k < n; k++ {
			out[sampleIdx] = cur
			if s, ok := sizeAt(ti, sampleIdx); ok {
				cur += uint64(s)
			}
			sampleIdx++
		}
		chunkIdx++
	}
	_ = chunkIdx
	return out, nil
}

func samplesInChunk(stsc *mp4.Stsc, chunk uint32) uint32 {
	if len(stsc.Entries) == 0 {
		return 1
	}
	entry := stsc.Entries[0]
	for _, e := range stsc.Entries {
		if e.FirstChunk <= chunk+1 { // first_chunk 为 1 基
			entry = e
		} else {
			break
		}
	}
	return entry.SamplesPerChunk
}

func sizeAt(ti *TrackInfo, idx int) (uint32, bool) {
	// readSamples 调用本函数时样本切片尚未填充，stsz 已先解析，
	// 因此通过 track 上暂存的原始尺寸表访问。
	if idx < len(ti.rawSizes) {
		return ti.rawSizes[idx], true
	}
	return 0, false
}
