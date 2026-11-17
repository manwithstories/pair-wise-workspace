package garc

import (
	"encoding/binary"
	"fmt"
)

// 文件头与轨道表的固定布局常量（见 docs/garc-format.md）。
const (
	MagicGARC        = "GARC"
	HeaderSize       = 64
	TrackEntrySize   = 32
	SupportedVersion = 1
	EndianLittle     = 1
	EndianBig        = 2
	FlagHDRContainer = 0x0001
	TrackTypeVideo   = 1
)

// Header 是 GARC 文件头解析结果。
type Header struct {
	Version    byte
	Endianness byte
	Flags      uint16
	FileSize   uint32
	// 三个声明区间（长度为 0 表示该区不存在）。
	Metadata Region
	SEI      Region
}

// Region 是 [Offset, Offset+Length) 绝对字节区间。
type Region struct {
	Offset uint32
	Length uint32
}

// End 返回区间末端，ok=false 表示加法溢出。
func (rg Region) End() (uint32, bool) { return CheckAddU32(rg.Offset, rg.Length) }

func (rg Region) asInt64() (int64, int64) { return int64(rg.Offset), int64(rg.Length) }

// Track 是轨道表项。
type Track struct {
	ID         uint32
	Type       byte
	HDRFlag    byte
	SEIRecords uint32
}

// IsVideo 报告该轨是否为视频轨。
func (t Track) IsVideo() bool { return t.Type == TrackTypeVideo }

// File 是解析后的容器视图：头部 + 轨道表 + 底层 Reader。
type File struct {
	Reader *Reader
	Header Header
	Tracks []Track
}

// HasContainerHDR 报告文件头是否声明携带容器级 HDR 元数据。
func (f *File) HasContainerHDR() bool { return f.Header.Flags&FlagHDRContainer != 0 }

// VideoTracks 返回全部视频轨（保持表内顺序）。
func (f *File) VideoTracks() []Track {
	var out []Track
	for _, t := range f.Tracks {
		if t.IsVideo() {
			out = append(out, t)
		}
	}
	return out
}

// Parse 执行完整容器层解析：magic → 版本/字节序 → 头 CRC → 声明区间 →
// 文件长度 → 轨道表。任何结构损坏都以 *Error 返回（退出码 3 类）。
func Parse(data []byte) (*File, error) {
	r, err := NewReader(data)
	if err != nil {
		return nil, err
	}
	if len(data) < HeaderSize {
		return nil, newError("TRUNCATED_HEADER", int64(len(data)),
			fmt.Sprintf("need %d header bytes, got %d", HeaderSize, len(data)),
			"file is truncated before the header; re-export")
	}
	if string(data[0:4]) != MagicGARC {
		return nil, newError("BAD_MAGIC", 0,
			fmt.Sprintf("magic is %q, want %q", string(data[0:4]), MagicGARC),
			"not a GARC container; check the input path")
	}
	h := Header{Version: data[4], Endianness: data[5]}
	switch h.Endianness {
	case EndianLittle:
		r.SetOrder(binary.LittleEndian)
	case EndianBig:
		r.SetOrder(binary.BigEndian)
	default:
		return nil, newError("BAD_BYTE_ORDER", 5,
			fmt.Sprintf("endianness=%d (1=LE,2=BE)", h.Endianness),
			"re-export with a supported byte order")
	}
	if h.Version != SupportedVersion {
		return nil, newError("UNKNOWN_VERSION", 4,
			fmt.Sprintf("version=%d, supported=%d", h.Version, SupportedVersion),
			"upgrade hdrcheck or re-export at container v1")
	}
	wantCRC, err := r.U32(HeaderSize - 4)
	if err != nil {
		return nil, err
	}
	if err := r.CheckCRC(0, HeaderSize-4, wantCRC); err != nil {
		return nil, err
	}

	h.Flags, _ = r.U16(6)
	h.FileSize, _ = r.U32(8)
	trackCount, _ := r.U32(12)
	tableOff, _ := r.U32(16)
	mdOff, _ := r.U32(20)
	mdLen, _ := r.U32(24)
	seiOff, _ := r.U32(28)
	seiLen, _ := r.U32(32)
	reserved, _ := r.U32(36)
	if reserved != 0 || !allZero(data[40:60]) {
		return nil, newError("RESERVED_NONZERO", 36,
			"reserved header fields must be zero", "re-export the container")
	}
	if h.Flags&^FlagHDRContainer != 0 {
		return nil, newError("RESERVED_NONZERO", 6,
			"unknown header flag bits are set", "re-export the container")
	}
	if uint64(trackCount)*TrackEntrySize > MaxFileSize {
		return nil, newError("INTEGER_OVERFLOW", 12,
			fmt.Sprintf("track_count %d would overflow file limit", trackCount),
			"reject the file; track table is implausibly large")
	}
	h.Metadata = Region{mdOff, mdLen}
	h.SEI = Region{seiOff, seiLen}
	if err := checkDeclaredRegion(r, h.Metadata, "metadata"); err != nil {
		return nil, err
	}
	if err := checkDeclaredRegion(r, h.SEI, "sei"); err != nil {
		return nil, err
	}
	if int64(h.FileSize) != int64(len(data)) {
		return nil, newError("LENGTH_MISMATCH", 8,
			fmt.Sprintf("declared file_size=%d, actual=%d", h.FileSize, len(data)),
			"the file was truncated or padded; re-export")
	}

	tracks, err := parseTrackTable(r, tableOff, trackCount)
	if err != nil {
		return nil, err
	}
	return &File{Reader: r, Header: h, Tracks: tracks}, nil
}

func checkDeclaredRegion(r *Reader, rg Region, name string) error {
	if rg.Length == 0 {
		return nil
	}
	end, ok := rg.End()
	if !ok {
		return newError("INTEGER_OVERFLOW", -1,
			fmt.Sprintf("%s region offset+length overflows uint32", name),
			"reject the file; region lengths are corrupt")
	}
	if int64(end) > int64(r.Len()) {
		return newError("OUT_OF_BOUNDS", int64(rg.Offset),
			fmt.Sprintf("%s region ends at %d, file length is %d", name, end, r.Len()),
			"the declared region runs past EOF; re-export or reject the file")
	}
	return nil
}

func parseTrackTable(r *Reader, off, count uint32) ([]Track, error) {
	need := int64(count) * TrackEntrySize
	if err := r.CheckRange(int64(off), need); err != nil {
		return nil, err
	}
	out := make([]Track, 0, count)
	seen := make(map[uint32]bool)
	for i := uint32(0); i < count; i++ {
		base := int64(off) + int64(i)*TrackEntrySize
		id, _ := r.U32(base)
		typ, _ := r.U8(base + 4)
		hdr, _ := r.U8(base + 5)
		res, _ := r.U16(base + 6)
		recs, _ := r.U32(base + 8)
		if res != 0 {
			return nil, newError("RESERVED_NONZERO", base+6,
				"track reserved16 must be zero", "re-export the container")
		}
		if seen[id] {
			return nil, newError("DUPLICATE_TRACK", base,
				fmt.Sprintf("track_id %d appears twice", id),
				"re-export; track ids must be unique")
		}
		seen[id] = true
		out = append(out, Track{ID: id, Type: typ, HDRFlag: hdr, SEIRecords: recs})
	}
	return out, nil
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
