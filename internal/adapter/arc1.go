// arc1 是本项目自带的轻量归档容器（纯 Go，无 CGO），用于自生成样例与
// 表达标准容器拒绝的故障形态（时间戳回绕、截断尾包）。
//
// 布局（全部小端）：
//
//	magic(4)="ARC1" | version(u8)=1 | flags(u8) | track_count(u16)
//	repeat track_count: TrackHeader
//	repeat packets:     PacketHeader + payload
//
// TrackHeader:
//
//	type(u8) | codec_len(u8) | codec(bytes) | timebase_num(i64) |
//	timebase_den(i64) | width(u32) | height(u32) | sample_rate(u32) |
//	channels(u16)
//
// PacketHeader:
//
//	track(u16) | start_ticks(i64) | duration_ticks(i64) |
//	payload_len(u32) | flags(u8, bit0=keyframe) | content_hash(u64)
//
// 时间戳回绕不做容器层禁止：写入器允许 start_ticks 任意回退，解析器
// 依据 track 的 wrap_bits（编码在 flags 高位）展开，展开过程对内核可见。
package adapter

import (
	"encoding/binary"
	"hash/fnv"
	"io"
	"os"

	"archivemedia/internal/model"
)

// ARC1 容器常量。
const (
	ARCMagic       = "ARC1"
	ARCVersion     = uint8(1)
	ARCAdapterName = "arc1"
	ARCAdapterVer  = "1.0.0"

	flagKeyFrame  = uint8(1 << 0)
	flagWrapMask  = uint8(0xf0) // 高 4 位存回绕位宽代码
	flagTruncated = uint8(1 << 7)

	// 回绕位宽代码：0=无回绕，1..4 对应 16/24/32/33 位。
	wrapCodeNone = 0
)

// ARCTrackSpec 是写入侧的轨道描述。
type ARCTrackSpec struct {
	Type       MediaType
	Codec      string
	TimeBase   model.Rational
	Width      int
	Height     int
	SampleRate int
	Channels   int
	WrapBits   int // 0 表示不回绕
}

// ARCPacket 是一帧/一个采样块的写入描述。
type ARCPacket struct {
	TrackIndex    int
	StartTicks    int64
	DurationTicks int64
	KeyFrame      bool
	Payload       []byte
}

// ARCWriter 生成确定性的 .arc 文件。
type ARCWriter struct {
	w        io.Writer
	tracks   []ARCTrackSpec
	written  int
	truncate bool
}

// NewARCWriter 创建写入器并立即落盘文件头与轨道头。
func NewARCWriter(w io.Writer, tracks []ARCTrackSpec) (*ARCWriter, error) {
	if len(tracks) == 0 {
		return nil, model.NewError(model.KindInvalidArgument, "arc1: at least one track required")
	}
	if len(tracks) > 1<<16-1 {
		return nil, model.NewError(model.KindQuotaExceeded, "arc1: too many tracks")
	}
	aw := &ARCWriter{w: w, tracks: tracks}
	if err := aw.writeHeader(); err != nil {
		return nil, err
	}
	return aw, nil
}

// MarkTruncated 在文件头标记尾包将被截断（解析器据此期待 EOF 中途出现）。
func (a *ARCWriter) MarkTruncated() { a.truncate = true }

func (a *ARCWriter) writeHeader() error {
	flags := uint8(0)
	if a.truncate {
		flags |= flagTruncated
	}
	buf := make([]byte, 0, 16)
	buf = append(buf, []byte(ARCMagic)...)
	buf = append(buf, ARCVersion, flags)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(a.tracks)))
	if _, err := a.w.Write(buf); err != nil {
		return model.Wrap(model.KindCorruptMedia, err, "arc1: header write")
	}
	for i, t := range a.tracks {
		if err := a.writeTrackHeader(i, t); err != nil {
			return err
		}
	}
	return nil
}

func (a *ARCWriter) writeTrackHeader(idx int, t ARCTrackSpec) error {
	if len(t.Codec) > 255 {
		return model.NewError(model.KindInvalidArgument, "arc1: codec name too long")
	}
	buf := make([]byte, 0, 64)
	buf = append(buf, uint8(t.Type), uint8(len(t.Codec)))
	buf = append(buf, []byte(t.Codec)...)
	buf = binary.LittleEndian.AppendUint64(buf, uint64(t.TimeBase.Num))
	buf = binary.LittleEndian.AppendUint64(buf, uint64(t.TimeBase.Den))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(t.Width))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(t.Height))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(t.SampleRate))
	buf = binary.LittleEndian.AppendUint16(buf, uint16(t.Channels))
	// 回绕位宽代码写入轨道头尾部的 1 字节（复用 flags 高位约定）。
	buf = append(buf, wrapBitsToCode(t.WrapBits))
	_, err := a.w.Write(buf)
	return model.Wrap(model.KindCorruptMedia, err, "arc1: track %d header", idx)
}

// WritePacket 写入一个 packet 及其负载，content hash 自动按负载计算。
func (a *ARCWriter) WritePacket(p ARCPacket) error {
	if p.TrackIndex < 0 || p.TrackIndex >= len(a.tracks) {
		return model.NewError(model.KindInvalidArgument, "arc1: track index out of range")
	}
	if len(p.Payload) > 1<<32-1 {
		return model.NewError(model.KindQuotaExceeded, "arc1: packet too large")
	}
	flags := uint8(0)
	if p.KeyFrame {
		flags |= flagKeyFrame
	}
	hash := fnv.New64a()
	hash.Write(p.Payload)
	hdr := make([]byte, 0, 40)
	hdr = binary.LittleEndian.AppendUint16(hdr, uint16(p.TrackIndex))
	hdr = binary.LittleEndian.AppendUint64(hdr, uint64(p.StartTicks))
	hdr = binary.LittleEndian.AppendUint64(hdr, uint64(p.DurationTicks))
	hdr = binary.LittleEndian.AppendUint32(hdr, uint32(len(p.Payload)))
	hdr = append(hdr, flags)
	hdr = binary.LittleEndian.AppendUint64(hdr, hash.Sum64())
	if _, err := a.w.Write(hdr); err != nil {
		return model.Wrap(model.KindCorruptMedia, err, "arc1: packet header")
	}
	if _, err := a.w.Write(p.Payload); err != nil {
		return model.Wrap(model.KindCorruptMedia, err, "arc1: packet payload")
	}
	a.written++
	return nil
}

// WriteTruncated 模拟损坏尾包：头部声明完整长度，但只写 keepBytes 字节
// 负载即结束文件，解析器 ReadFull 遇到 io.ErrUnexpectedEOF 即识别损坏。
func (a *ARCWriter) WriteTruncated(p ARCPacket, keepBytes int) error {
	if keepBytes < 0 || keepBytes >= len(p.Payload) {
		return model.NewError(model.KindInvalidArgument, "arc1: truncation keepBytes must be < payload")
	}
	flags := uint8(0)
	if p.KeyFrame {
		flags |= flagKeyFrame
	}
	hash := fnv.New64a()
	hash.Write(p.Payload) // 哈希按“应有”的完整负载计算
	hdr := make([]byte, 0, 40)
	hdr = binary.LittleEndian.AppendUint16(hdr, uint16(p.TrackIndex))
	hdr = binary.LittleEndian.AppendUint64(hdr, uint64(p.StartTicks))
	hdr = binary.LittleEndian.AppendUint64(hdr, uint64(p.DurationTicks))
	hdr = binary.LittleEndian.AppendUint32(hdr, uint32(len(p.Payload))) // 声明完整长度
	hdr = append(hdr, flags)
	hdr = binary.LittleEndian.AppendUint64(hdr, hash.Sum64())
	if _, err := a.w.Write(hdr); err != nil {
		return model.Wrap(model.KindCorruptMedia, err, "arc1: truncated header")
	}
	if _, err := a.w.Write(p.Payload[:keepBytes]); err != nil {
		return model.Wrap(model.KindCorruptMedia, err, "arc1: truncated payload")
	}
	// 不计入 written：该包未完整落盘。
	return nil
}

// PacketsWritten 返回已完整写入的 packet 数。
func (a *ARCWriter) PacketsWritten() int { return a.written }

// WriteARCFile 是便捷封装：整文件原子写入临时文件后替换目标路径。
func WriteARCFile(path string, tracks []ARCTrackSpec, pkts []ARCPacket, truncated bool) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return model.Wrap(model.KindInvalidArgument, err, "arc1: create %s", tmp)
	}
	aw, err := NewARCWriter(f, tracks)
	if err != nil {
		f.Close()
		return err
	}
	if truncated {
		aw.MarkTruncated()
	}
	for i := range pkts {
		if truncated && i == len(pkts)-1 {
			if err := aw.WriteTruncated(pkts[i], len(pkts[i].Payload)/2); err != nil {
				f.Close()
				return err
			}
			break
		}
		if err := aw.WritePacket(pkts[i]); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return model.Wrap(model.KindCorruptMedia, err, "arc1: close")
	}
	return os.Rename(tmp, path)
}

func wrapBitsToCode(bits int) uint8 {
	switch bits {
	case 0:
		return wrapCodeNone
	case 16:
		return 1
	case 24:
		return 2
	case 32:
		return 3
	case 33:
		return 4
	default:
		return wrapCodeNone
	}
}

func codeToWrapBits(code uint8) int {
	switch code & 0x7 {
	case 1:
		return 16
	case 2:
		return 24
	case 3:
		return 32
	case 4:
		return 33
	default:
		return 0
	}
}

// arcTrackHeaderSize 不含变长 codec 字段，仅用于解析偏移估算。
func arcTrackHeaderSize(codecLen int) int {
	return 2 + codecLen + 8 + 8 + 4 + 4 + 4 + 2 + 1
}
