// Package fragment 读取项目自有 fMP4 容器的分片索引与时间戳，
// 并校验边界、CRC 与资源限制。
package fragment

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"

	"review/internal/fault"
)

// 资源限制。
const (
	MaxSegmentBytes = 8 << 20  // 单分片上限
	MaxTotalBytes   = 128 << 20 // 总处理内存上限
	MaxSamples      = 1 << 16   // 单分片样本上限
)

// InitInfo 初始化段信息（moov/rinit）。
type InitInfo struct {
	Codec         string // "jpeg" 或 "lpcm"
	Timescale     int64
	Width, Height int
	SampleRate    int
	Channels      int
	FrameDuration int64 // 每样本时长（timescale 单位）
}

// SampleMeta 单个样本的索引与时间戳。
type SampleMeta struct {
	Index      int
	Time       int64 // timescale 单位
	Duration   int64
	Size       uint32
	CRC        uint32
	DataOffset int64 // 文件内绝对偏移
}

// Segment 解析后的分片。
type Segment struct {
	Path       string
	BaseTime   int64
	Samples    []SampleMeta
	IndexCount int // sidx 条目数
	data       []byte
}

type box struct {
	typ    string
	off    int64 // box 头偏移
	size   int64 // 含头
	payload []byte
}

// walkBoxes 顺序扫描盒子；截断时返回已扫描盒子与故障。
func walkBoxes(data []byte) ([]box, *fault.Error) {
	var boxes []box
	off := int64(0)
	for off < int64(len(data)) {
		if int64(len(data))-off < 8 {
			return boxes, fault.Newf(fault.KindTruncated, "", off, true,
				"truncated box header, %d bytes left", int64(len(data))-off)
		}
		size := int64(binary.BigEndian.Uint32(data[off:]))
		typ := string(data[off+4 : off+8])
		if size < 8 {
			return boxes, fault.Newf(fault.KindTruncated, "", off, true,
				"box %s invalid size %d", typ, size)
		}
		if off+size > int64(len(data)) {
			// 截断盒子：保留实际剩余数据，便于挽救完整样本
			boxes = append(boxes, box{typ: typ, off: off, size: size,
				payload: data[off+8:]})
			return boxes, fault.Newf(fault.KindTruncated, "", off, true,
				"box %s size %d out of range (file %d)", typ, size, len(data))
		}
		boxes = append(boxes, box{typ: typ, off: off, size: size,
			payload: data[off+8 : off+size]})
		off += size
	}
	return boxes, nil
}

// ParseInit 解析初始化段。
func ParseInit(path string) (*InitInfo, *fault.Error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fault.Newf(fault.KindMissingInit, path, 0, true, "read init: %v", err)
	}
	boxes, ferr := walkBoxes(data)
	if ferr != nil {
		ferr.Source = path
		return nil, ferr
	}
	for _, b := range boxes {
		if b.typ != "moov" {
			continue
		}
		inner, _ := walkBoxes(b.payload)
		for _, ib := range inner {
			if ib.typ == "rinf" && len(ib.payload) >= 26 {
				p := ib.payload
				return &InitInfo{
					Codec:         string(p[0:4]),
					Timescale:     int64(binary.BigEndian.Uint32(p[4:])),
					Width:         int(binary.BigEndian.Uint16(p[8:])),
					Height:        int(binary.BigEndian.Uint16(p[10:])),
					SampleRate:    int(binary.BigEndian.Uint32(p[12:])),
					Channels:      int(binary.BigEndian.Uint16(p[16:])),
					FrameDuration: int64(binary.BigEndian.Uint32(p[18:])),
				}, nil
			}
		}
	}
	return nil, fault.New(fault.KindMissingInit, path, 0, false, "no moov/rinf box")
}

// ParseSegment 解析分片：sidx 索引、moof 时间戳、mdat 数据。
// 截断与索引缺项为可恢复故障，保留已解析部分。
func ParseSegment(path string) (*Segment, []*fault.Error) {
	var errs []*fault.Error
	info, err := os.Stat(path)
	if err != nil {
		return nil, []*fault.Error{fault.Newf(fault.KindMissingIndex, path, 0, true, "stat: %v", err)}
	}
	if info.Size() > MaxSegmentBytes {
		return nil, []*fault.Error{fault.Newf(fault.KindLimit, path, MaxSegmentBytes, false,
			"segment %d bytes exceeds limit %d", info.Size(), MaxSegmentBytes)}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, []*fault.Error{fault.Newf(fault.KindMissingIndex, path, 0, true, "read: %v", err)}
	}
	seg := &Segment{Path: path, data: data}
	boxes, ferr := walkBoxes(data)
	if ferr != nil {
		ferr.Source = path
		errs = append(errs, ferr)
	}
	var sidxSizes []uint32
	var mdatOff int64 = -1
	var mdatLen int64
	for _, b := range boxes {
		switch b.typ {
		case "sidx":
			if len(b.payload) < 4 {
				break
			}
			n := binary.BigEndian.Uint32(b.payload)
			for i := uint32(0); i < n && 4+8*i+8 <= uint32(len(b.payload)); i++ {
				sidxSizes = append(sidxSizes, binary.BigEndian.Uint32(b.payload[4+8*i:]))
			}
			seg.IndexCount = len(sidxSizes)
		case "moof":
			inner, _ := walkBoxes(b.payload)
			for _, ib := range inner {
				switch ib.typ {
				case "tfdt":
					if len(ib.payload) >= 8 {
						seg.BaseTime = int64(binary.BigEndian.Uint64(ib.payload))
					}
				case "trun":
					parseTrun(ib.payload, b, seg)
				}
			}
		case "mdat":
			mdatOff = b.off + 8
			mdatLen = int64(len(b.payload)) // 截断时为实际剩余
		}
	}
	// 数据偏移换算为文件绝对偏移并校验越界
	if mdatOff >= 0 {
		rel := int64(0)
		for i := range seg.Samples {
			s := &seg.Samples[i]
			s.DataOffset = mdatOff + rel
			rel += int64(s.Size)
			if s.DataOffset+int64(s.Size) > int64(len(data)) {
				errs = append(errs, fault.Newf(fault.KindTruncated, path, s.DataOffset, true,
					"sample %d data out of range", i))
				seg.Samples = seg.Samples[:i]
				break
			}
		}
		if rel > mdatLen && len(seg.Samples) > 0 && len(errs) == 0 {
			errs = append(errs, fault.Newf(fault.KindTruncated, path, mdatOff+mdatLen, true,
				"samples exceed mdat"))
		}
	}
	// 索引缺项：sidx 条目数少于 trun 样本数（多于则是截断所致，不重复报告）
	if seg.IndexCount < len(seg.Samples) {
		errs = append(errs, fault.Newf(fault.KindMissingIndex, path, int64(seg.IndexCount), true,
			"sidx entries %d != trun samples %d", seg.IndexCount, len(seg.Samples)))
	}
	return seg, errs
}

func parseTrun(p []byte, moof box, seg *Segment) {
	if len(p) < 4 {
		return
	}
	n := binary.BigEndian.Uint32(p)
	if n > MaxSamples {
		n = MaxSamples
	}
	t := seg.BaseTime
	for i := uint32(0); i < n; i++ {
		ent := p[4+12*i:]
		if len(ent) < 12 {
			break
		}
		dur := binary.BigEndian.Uint32(ent)
		size := binary.BigEndian.Uint32(ent[4:])
		crc := binary.BigEndian.Uint32(ent[8:])
		seg.Samples = append(seg.Samples, SampleMeta{
			Index: int(i), Time: t, Duration: int64(dur), Size: size, CRC: crc,
		})
		t += int64(dur)
	}
}

// ReadData 读取样本数据并校验 CRC；损坏报可恢复故障，不伪报成功。
func (s *Segment) ReadData(i int) ([]byte, *fault.Error) {
	if i < 0 || i >= len(s.Samples) {
		return nil, fault.Newf(fault.KindMissingIndex, s.Path, int64(i), true,
			"sample index %d out of range", i)
	}
	m := s.Samples[i]
	end := m.DataOffset + int64(m.Size)
	if m.DataOffset < 0 || end > int64(len(s.data)) {
		return nil, fault.Newf(fault.KindTruncated, s.Path, m.DataOffset, true,
			"sample %d offset out of range", i)
	}
	payload := s.data[m.DataOffset:end]
	if crc32.ChecksumIEEE(payload) != m.CRC {
		return nil, fault.Newf(fault.KindCRC, s.Path, m.DataOffset, true,
			"sample %d crc mismatch", i)
	}
	return payload, nil
}

// String 便于日志输出。
func (i *InitInfo) String() string {
	return fmt.Sprintf("codec=%s timescale=%d %dx%d rate=%d ch=%d",
		i.Codec, i.Timescale, i.Width, i.Height, i.SampleRate, i.Channels)
}
