// Package wav 实现 RIFF/WAVE 的增量流式解析，支持 PCM 与 IEEE float、
// 多声道 fmt/data 块，并对越界长度、非法声道数与不完整样本帧做严格拒绝。
package wav

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"mediad/internal/clock"
)

// 格式标签（wFormatTag）。
const (
	FormatPCM   uint16 = 1 // WAVE_FORMAT_PCM
	FormatFloat uint16 = 3 // WAVE_FORMAT_IEEE_FLOAT
	FormatExt   uint16 = 0xFFFE
)

// MaxChunkBytes 限定单个 RIFF 块上限 1 MiB。
const MaxChunkBytes uint64 = 1 << 20

// Format 描述 fmt 块解析结果。
type Format struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	// ExtFormatTag 仅在 WAVE_FORMAT_EXTENSIBLE 下有效，否则等于 FormatTag。
	ExtFormatTag uint16
}

// FrameSize 返回单帧（每声道一个样本）字节数，带溢出校验。
func (f Format) FrameSize() (uint64, error) {
	if f.Channels == 0 || f.BitsPerSample == 0 || f.BitsPerSample%8 != 0 {
		return 0, errors.New("wav: invalid channel count or bits-per-sample")
	}
	return clock.CheckedMul(uint64(f.Channels), uint64(f.BitsPerSample)/8)
}

// IsPCM 是否整数 PCM。
func (f Format) IsPCM() bool { return f.effectiveTag() == FormatPCM }

// IsFloat 是否 IEEE float。
func (f Format) IsFloat() bool { return f.effectiveTag() == FormatFloat }

func (f Format) effectiveTag() uint16 {
	if f.FormatTag == FormatExt {
		return f.ExtFormatTag
	}
	return f.FormatTag
}

// DataBlock 是一次 ReadData 交出的样本块，复用同一底层数组以控制峰值内存。
type DataBlock struct {
	Data       []byte
	FrameCount uint64 // 完整帧数量
	FirstFrame uint64 // 本块在整条流中的起始帧序号
}

// Parser 以增量方式消费 WAV 字节流。
type Parser struct {
	r         io.Reader
	peek      []byte // 单字节前瞻缓冲（最多 1 字节）
	formatted bool
	format    Format
	dataLeft  uint64 // 当前 data 块剩余字节
	inData    bool
	frameIdx  uint64 // 已交付的帧数
	rootLeft  uint64 // RIFF 外层剩余字节（0 表示未知/不校验）
	closed    bool
}

// NewParser 基于任意 io.Reader 构造流式解析器。
func NewParser(r io.Reader) *Parser {
	return &Parser{r: r}
}

// readByte 取一个字节，优先消费前瞻缓冲。
func (p *Parser) readByte() (byte, error) {
	if len(p.peek) > 0 {
		b := p.peek[0]
		p.peek = p.peek[:0]
		return b, nil
	}
	var b [1]byte
	_, err := io.ReadFull(p.r, b[:])
	return b[0], err
}

// hasMoreData 前瞻是否仍有字节可读（不消费数据）。
func (p *Parser) hasMoreData() bool {
	if len(p.peek) > 0 {
		return true
	}
	b, err := p.readByte()
	if err != nil {
		return false
	}
	p.peek = append(p.peek, b)
	return true
}

// chunkHeader 是 RIFF 块头：4 字节 FourCC + 32 位小端长度。
type chunkHeader struct {
	id   [4]byte
	size uint64
}

// ParseHeader 读取并校验 RIFF/WAVE 外层头，必须在其它方法前调用一次。
func (p *Parser) ParseHeader() error {
	var hdr [12]byte
	if _, err := io.ReadFull(p.r, hdr[:]); err != nil {
		return fmt.Errorf("wav: RIFF header incomplete: %w", err)
	}
	if string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return errors.New("wav: not a RIFF/WAVE stream")
	}
	size := uint64(binary.LittleEndian.Uint32(hdr[4:8]))
	// 声明长度不得超过实际可读上限的算术边界（防后续加法溢出）。
	if size > clock.MaxUint32-uint64(len(hdr)) {
		return errors.New("wav: RIFF size exceeds uint32 boundary")
	}
	p.rootLeft = size + uint64(len(hdr)) - 8 // size 字段之后的总字节
	return nil
}

func (p *Parser) readChunkHeader() (chunkHeader, error) {
	var h [8]byte
	for i := range h {
		b, err := p.readByte()
		if err != nil {
			return chunkHeader{}, err
		}
		h[i] = b
	}
	ch := chunkHeader{id: [4]byte(h[0:4]), size: uint64(binary.LittleEndian.Uint32(h[4:8]))}
	if ch.size > MaxChunkBytes {
		return ch, fmt.Errorf("wav: chunk %q size %d exceeds 1MiB limit", ch.id, ch.size)
	}
	return ch, nil
}

// NextChunk 跳到下一个块并返回其 FourCC。遇到 fmt 自动解析，遇到 data 进入数据态。
func (p *Parser) NextChunk() (string, error) {
	if p.closed {
		return "", io.EOF
	}
	if p.inData && p.dataLeft > 0 {
		// 调用方未消费完 data 不允许跨越块。
		return "", errors.New("wav: previous data chunk not fully consumed")
	}
	ch, err := p.readChunkHeader()
	if err != nil {
		return "", err
	}
	id := string(ch.id[:])
	switch id {
	case "fmt ":
		if err := p.parseFmt(ch.size); err != nil {
			return id, err
		}
	case "data":
		if !p.formatted {
			return id, errors.New("wav: data chunk appears before fmt")
		}
		p.inData = true
		p.dataLeft = ch.size
	default:
		// 未知块：按声明长度跳过（仍受 1MiB 上限约束）。
		if _, err := io.CopyN(io.Discard, p.r, int64(ch.size)); err != nil {
			return id, fmt.Errorf("wav: skip chunk %q failed: %w", id, err)
		}
		if ch.size%2 == 1 { // RIFF 垫字节
			if _, err := io.CopyN(io.Discard, p.r, 1); err != nil {
				return id, err
			}
		}
	}
	return id, nil
}

func (p *Parser) parseFmt(size uint64) error {
	if size < 16 {
		return fmt.Errorf("wav: fmt chunk too short: %d", size)
	}
	raw := make([]byte, size)
	if _, err := io.ReadFull(p.r, raw); err != nil {
		return fmt.Errorf("wav: fmt chunk truncated: %w", err)
	}
	f := Format{
		FormatTag:      binary.LittleEndian.Uint16(raw[0:2]),
		Channels:       binary.LittleEndian.Uint16(raw[2:4]),
		SamplesPerSec:  binary.LittleEndian.Uint32(raw[4:8]),
		AvgBytesPerSec: binary.LittleEndian.Uint32(raw[8:12]),
		BlockAlign:     binary.LittleEndian.Uint16(raw[12:14]),
		BitsPerSample:  binary.LittleEndian.Uint16(raw[14:16]),
	}
	if f.FormatTag == FormatExt {
		// cbSize(2) + ValidBitsPerSample(2) + ChannelMask(4) + SubFormat(16)
		if size < 16+2+2+4+16 {
			return errors.New("wav: extensible fmt chunk truncated")
		}
		// SubFormat 前 2 字节即实际格式标签。
		f.ExtFormatTag = binary.LittleEndian.Uint16(raw[16+2+2+4:])
	} else {
		f.ExtFormatTag = f.FormatTag
	}
	if err := validateFormat(f); err != nil {
		return err
	}
	p.format = f
	p.formatted = true
	return nil
}

func validateFormat(f Format) error {
	tag := f.effectiveTag()
	if tag != FormatPCM && tag != FormatFloat {
		return fmt.Errorf("wav: unsupported format tag %d", tag)
	}
	if f.Channels == 0 || f.Channels > 32 {
		return fmt.Errorf("wav: illegal channel count %d", f.Channels)
	}
	if f.SamplesPerSec == 0 {
		return errors.New("wav: zero sample rate")
	}
	switch f.BitsPerSample {
	case 8, 16, 24, 32:
	default:
		return fmt.Errorf("wav: unsupported bits-per-sample %d", f.BitsPerSample)
	}
	frame, err := f.FrameSize()
	if err != nil {
		return err
	}
	if uint64(f.BlockAlign) != frame {
		return fmt.Errorf("wav: block align %d != frame size %d", f.BlockAlign, frame)
	}
	avg, err := clock.CheckedMul(frame, uint64(f.SamplesPerSec))
	if err != nil || uint64(f.AvgBytesPerSec) != avg {
		return fmt.Errorf("wav: avg bytes/sec %d inconsistent with frame %d @%d",
			f.AvgBytesPerSec, frame, f.SamplesPerSec)
	}
	return nil
}

// Format 返回已解析的 fmt；未解析时返回错误。
func (p *Parser) Format() (Format, error) {
	if !p.formatted {
		return Format{}, errors.New("wav: fmt not parsed yet")
	}
	return p.format, nil
}

// ReadData 读取至多 maxBytes 的样本数据，仅按完整帧边界切分。
// 返回 io.EOF 表示 data 块（或流）结束；截断的尾部帧作为错误上报。
func (p *Parser) ReadData(maxBytes uint64) (DataBlock, error) {
	if !p.inData {
		return DataBlock{}, errors.New("wav: not inside a data chunk")
	}
	if maxBytes == 0 || maxBytes > MaxChunkBytes {
		return DataBlock{}, errors.New("wav: read size out of (0,1MiB]")
	}
	frame, err := p.format.FrameSize()
	if err != nil {
		return DataBlock{}, err
	}
	want := maxBytes
	if p.dataLeft < want {
		want = p.dataLeft
	}
	buf := make([]byte, want)
	n, err := io.ReadFull(p.r, buf)
	p.dataLeft -= uint64(n)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return DataBlock{}, err
	}
	// data 块声明长度与实际可读字节不符：截断。
	if (err == io.EOF || err == io.ErrUnexpectedEOF) && p.dataLeft > 0 {
		return DataBlock{}, fmt.Errorf("wav: data chunk truncated, %d bytes missing", p.dataLeft)
	}
	complete := uint64(n) / frame
	if complete == 0 {
		return DataBlock{}, fmt.Errorf("wav: incomplete sample frame (%d bytes < frame %d)", n, frame)
	}
	tail := uint64(n) % frame
	if tail != 0 {
		// 任何悬空字节都意味着样本帧不完整，拒绝静默补齐或丢弃。
		return DataBlock{}, fmt.Errorf("wav: %d dangling bytes do not form a frame", tail)
	}
	blk := DataBlock{
		Data:       buf[:complete*frame],
		FrameCount: complete,
		FirstFrame: p.frameIdx,
	}
	p.frameIdx += complete
	if p.dataLeft == 0 {
		p.inData = false
		// data 块声明长度之外若仍有悬空字节，必为不完整样本帧。
		if p.hasMoreData() {
			return DataBlock{}, errors.New("wav: dangling bytes do not form a complete frame")
		}
		return blk, io.EOF
	}
	return blk, nil
}

// FramesDelivered 返回已成功交付的完整帧总数。
func (p *Parser) FramesDelivered() uint64 { return p.frameIdx }
