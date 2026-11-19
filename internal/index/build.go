// Package index 以 go-mp4 构建可随机访问的采样块索引交付容器，
// 并在验证阶段复读索引、校验首尾/间隔与随机跳转读回位置一致性。
package index

import (
	"fmt"
	"io"

	mp4 "github.com/abema/go-mp4"

	"pairflow/internal/clock"
)

// BoxTypePidx 是自定义采样块索引 box（fourCC 'pidx'）。
const BoxTypePidx = "pidx"

// digestSize 是 mdat 负载头部固定的输入摘要长度。
const digestSize = 16

// Entry 是单个采样块的随机访问索引项。
type Entry struct {
	TrackID      uint32 `json:"track_id"`
	StartSample  int64  `json:"start_sample"`
	SampleCount  int64  `json:"sample_count"`
	DataOffset   int64  `json:"data_offset"`   // PCM 字节在 mdat 负载中的绝对偏移
	DataLength   int64  `json:"data_length"`   // PCM 字节数
	SourceOffset int64  `json:"source_offset"` // 原始采集文件中的字节偏移
	RTPSeq       uint64 `json:"rtp_seq"`
	Flags        uint32 `json:"flags"` // bit0=uncertain
}

// FlagUncertain 标记该索引项覆盖的时间线区间含不确定采样。
const FlagUncertain uint32 = 1

// pidxBox 是 go-mp4 可序列化的自定义 box 定义。
type pidxBox struct {
	mp4.Box
	EntryCount uint32    `mp4:"0,size=32"`
	Entries    []entry64 `mp4:"1,len=dynamic,size=512"` // size 为单元素位宽：8*uint64
}

type entry64 struct {
	TrackID      uint64 `mp4:"0,size=64"`
	StartSample  uint64 `mp4:"1,size=64"`
	SampleCount  uint64 `mp4:"2,size=64"`
	DataOffset   uint64 `mp4:"3,size=64"`
	DataLength   uint64 `mp4:"4,size=64"`
	SourceOffset uint64 `mp4:"5,size=64"`
	RTPSeq       uint64 `mp4:"6,size=64"`
	Flags        uint64 `mp4:"7,size=64"`
}

func (*pidxBox) GetType() mp4.BoxType { return mp4.StrToBoxType(BoxTypePidx) }

// GetFieldLength 让 go-mp4 在回读时按 EntryCount 解析动态数组。
func (p *pidxBox) GetFieldLength(name string, _ mp4.Context) uint {
	if name == "Entries" {
		return uint(p.EntryCount)
	}
	panic(fmt.Errorf("invalid dynamic-length field: %s", name))
}

func init() { mp4.AddBoxDef(&pidxBox{}, 0) }

// Descriptor 是一个轨道的索引项集合与格式元数据。
type Descriptor struct {
	TrackID       uint32
	DeviceID      string
	SampleRate    uint32
	Channels      uint16
	BitsPerSample uint16
	Entries       []Entry
}

// Builder 先把 PCM 串联写入 mdat，再回填 pidx 索引并闭合容器。
type Builder struct {
	w      io.WriteSeeker
	mw     *mp4.Writer
	curOff int64 // 下一块 PCM 在 mdat 负载中的偏移
	open   bool
}

// NewBuilder 写出 ftyp 与打开 moov/mdat 之前的准备结构。
func NewBuilder(w io.WriteSeeker) *Builder {
	return &Builder{w: w, mw: mp4.NewWriter(w)}
}

// Begin 写 ftyp，并打开 mdat（先放 16 字节输入摘要）。
func (b *Builder) Begin(digest []byte) error {
	if _, err := b.mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(b.mw, &mp4.Ftyp{
		MajorBrand:   mp4.StrToBoxType("pfid"),
		MinorVersion: 1,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: mp4.StrToBoxType("pfid")},
			{CompatibleBrand: mp4.StrToBoxType("iso4")},
		},
	}, mp4.Context{}); err != nil {
		return err
	}
	if _, err := b.mw.EndBox(); err != nil {
		return err
	}
	if _, err := b.mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()}); err != nil {
		return err
	}
	digestBuf := make([]byte, digestSize)
	copy(digestBuf, digest)
	if _, err := b.mw.Write(digestBuf); err != nil {
		return err
	}
	b.curOff = digestSize
	b.open = true
	return nil
}

// AddPCM 写入一块 PCM，返回可放入索引的 mdat 负载偏移与长度。
func (b *Builder) AddPCM(pcm []byte) (off, length int64, err error) {
	if !b.open {
		return 0, 0, fmt.Errorf("index: builder not begun")
	}
	off = b.curOff
	if _, err = b.mw.Write(pcm); err != nil {
		return 0, 0, err
	}
	b.curOff += int64(len(pcm))
	return off, int64(len(pcm)), nil
}

// Finalize 闭合 mdat，写 moov>udta>pidx（每轨道一个），返回文件总字节数。
// 注意：mdat 在前、moov 在后，使写入阶段可以流式追加 PCM。
func (b *Builder) Finalize(descs []Descriptor) (int64, error) {
	if !b.open {
		return 0, fmt.Errorf("index: builder not begun")
	}
	if _, err := b.mw.EndBox(); err != nil { // mdat
		return 0, err
	}
	b.open = false
	if _, err := b.mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return 0, err
	}
	if _, err := b.mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeUdta()}); err != nil {
		return 0, err
	}
	for _, d := range descs {
		pb := &pidxBox{EntryCount: uint32(len(d.Entries))}
		for _, e := range d.Entries {
			pb.Entries = append(pb.Entries, entry64{
				TrackID: uint64(e.TrackID), StartSample: uint64(e.StartSample),
				SampleCount: uint64(e.SampleCount), DataOffset: uint64(e.DataOffset),
				DataLength: uint64(e.DataLength), SourceOffset: uint64(e.SourceOffset),
				RTPSeq: e.RTPSeq, Flags: uint64(e.Flags),
			})
		}
		if _, err := b.mw.StartBox(&mp4.BoxInfo{Type: pb.GetType()}); err != nil {
			return 0, err
		}
		if _, err := mp4.Marshal(b.mw, pb, mp4.Context{}); err != nil {
			return 0, err
		}
		if _, err := b.mw.EndBox(); err != nil {
			return 0, err
		}
	}
	if _, err := b.mw.EndBox(); err != nil { // udta
		return 0, err
	}
	if _, err := b.mw.EndBox(); err != nil { // moov
		return 0, err
	}
	return b.w.Seek(0, io.SeekEnd)
}

// ReadFile 复读交付容器，按轨道返回索引项、mdat 负载基址与输入摘要。
func ReadFile(r io.ReadSeeker) (map[uint32][]Entry, int64, []byte, error) {
	var (
		entries     = make(map[uint32][]Entry)
		mdatPayload int64
		digest      []byte
	)
	handler := func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type.String() {
		case BoxTypePidx:
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			pb, ok := box.(*pidxBox)
			if !ok {
				return nil, clock.New("index.verify", clock.KindIndexSeek, "",
					int64(h.BoxInfo.Offset), clock.NoSeq,
					"pidx box payload type mismatch", "重建交付容器")
			}
			for _, e := range pb.Entries {
				entries[uint32(e.TrackID)] = append(entries[uint32(e.TrackID)], Entry{
					TrackID: uint32(e.TrackID), StartSample: int64(e.StartSample),
					SampleCount: int64(e.SampleCount), DataOffset: int64(e.DataOffset),
					DataLength: int64(e.DataLength), SourceOffset: int64(e.SourceOffset),
					RTPSeq: e.RTPSeq, Flags: uint32(e.Flags),
				})
			}
			return nil, nil
		case "mdat":
			mdatPayload = int64(h.BoxInfo.Offset + h.BoxInfo.HeaderSize)
			if _, err := h.BoxInfo.SeekToPayload(r); err != nil {
				return nil, err
			}
			digest = make([]byte, digestSize)
			if _, err := io.ReadFull(r, digest); err != nil {
				return nil, err
			}
			return nil, nil
		}
		return h.Expand()
	}
	if _, err := mp4.ReadBoxStructure(r, handler); err != nil {
		return nil, 0, nil, clock.Wrap("index.verify", clock.KindIndexSeek, "",
			0, clock.NoSeq, "mp4 structure unreadable", "重建交付容器", err)
	}
	return entries, mdatPayload, digest, nil
}

// ReadAt 从 mdat 负载的指定偏移读回 n 字节，用于随机跳转一致性校验。
func ReadAt(r io.ReadSeeker, mdatPayload, off int64, n int) ([]byte, error) {
	if _, err := r.Seek(mdatPayload+off, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// VerifyEntries 校验索引的首尾、间隔连续性与起点单调，返回首个违规项。
func VerifyEntries(trackID uint32, es []Entry) *clock.StageError {
	if len(es) == 0 {
		return clock.New("index.verify", clock.KindIndexSeek, "", 0, clock.NoSeq,
			fmt.Sprintf("track %d has no index entries", trackID), "检查上游修复阶段")
	}
	if es[0].StartSample != 0 {
		return clock.New("index.verify", clock.KindIndexSeek, "", es[0].SourceOffset,
			es[0].RTPSeq, "first entry does not start at sample 0", "重建时间线")
	}
	for i := 1; i < len(es); i++ {
		prev, cur := es[i-1], es[i]
		if cur.StartSample < prev.StartSample+prev.SampleCount {
			return clock.New("index.verify", clock.KindIndexSeek, "", cur.SourceOffset,
				cur.RTPSeq, "index entries overlap or go backwards", "重建时间线")
		}
		if cur.DataOffset < prev.DataOffset {
			return clock.New("index.verify", clock.KindIndexSeek, "", cur.SourceOffset,
				cur.RTPSeq, "mdat data offsets are not monotonic", "重建交付容器")
		}
	}
	return nil
}
