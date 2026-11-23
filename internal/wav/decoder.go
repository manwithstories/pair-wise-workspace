// Package wav 读取并校验 PCM/WAV 输入：RIFF 长度、fmt/data 块次序、
// 块对齐与尾部截断，所有损坏均定位到字节偏移并换算采样帧位置。
package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"media-pipeline/internal/clock"
	"media-pipeline/internal/fault"
)

// PCMFormatTag 是整数 PCM 的 WAV 格式标签，其它（有损/浮点）一律拒绝。
const PCMFormatTag uint16 = 1

// Header 保存校验通过后的 WAV 头信息。
type Header struct {
	Format      clock.Timebase
	AudioFormat uint16
	ByteRate    uint32
	BlockAlign  uint16
	RiffSize    uint32
	DataStart   int64 // data 载荷在文件中的绝对字节偏移
	DataSize    uint32 // data 块声明的载荷字节数
	FileSize    int64
}

// 块标识。
var (
	idRIFF = []byte("RIFF")
	idWAVE = []byte("WAVE")
	idFmt  = []byte("fmt ")
	idData = []byte("data")
)

// DecodeHeader 从 r 解析并校验 WAV 头，blockID 用于错误溯源。
func DecodeHeader(blockID string, r io.ReadSeeker) (*Header, error) {
	fileSize, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, fault.New(fault.KindOther, blockID, -1, -1, false,
			"cannot seek WAV input").WithOp("wav.decode").Wrap(err)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, fault.New(fault.KindOther, blockID, -1, -1, false,
			"cannot rewind WAV input").WithOp("wav.decode").Wrap(err)
	}
	if fileSize < 12 {
		return nil, fault.New(fault.KindMalformedInput, blockID, fileSize, -1, true,
			"file too small for RIFF header").WithOp("wav.decode")
	}

	h := &Header{FileSize: fileSize}
	if err := h.readRiff(blockID, r); err != nil {
		return nil, err
	}
	if err := h.walkChunks(blockID, r); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *Header) readRiff(blockID string, r io.Reader) error {
	head := make([]byte, 12)
	if _, err := io.ReadFull(r, head); err != nil {
		return fault.New(fault.KindMalformedInput, blockID, 0, -1, true,
			"truncated RIFF header").WithOp("wav.decode").Wrap(err)
	}
	if !bytes.Equal(head[0:4], idRIFF) {
		return malformed(blockID, 0, -1, "missing RIFF magic")
	}
	h.RiffSize = binary.LittleEndian.Uint32(head[4:8])
	if !bytes.Equal(head[8:12], idWAVE) {
		return malformed(blockID, 8, -1, "missing WAVE magic")
	}
	// RIFF 长度字段 + 8 字节必须与实际文件一致，否则报告精确偏移。
	want := int64(h.RiffSize) + 8
	switch {
	case want > h.FileSize:
		return malformed(blockID, h.FileSize, -1,
			fmt.Sprintf("RIFF declares %d bytes but file is truncated at %d", want, h.FileSize))
	case want < h.FileSize:
		return malformed(blockID, want, -1,
			fmt.Sprintf("trailing %d bytes beyond RIFF declared size", h.FileSize-want))
	}
	return nil
}

func (h *Header) walkChunks(blockID string, r io.ReadSeeker) error {
	var (
		offset  int64 = 12
		fmtDone       = false
		dataDone      = false
	)
	for offset+8 <= h.FileSize {
		chHead := make([]byte, 8)
		if _, err := io.ReadFull(r, chHead); err != nil {
			return malformed(blockID, offset, -1, "truncated chunk header")
		}
		id := chHead[0:4]
		size := int64(binary.LittleEndian.Uint32(chHead[4:8]))
		payloadStart := offset + 8
		payloadEnd := payloadStart + size
		if payloadEnd > h.FileSize {
			return malformed(blockID, h.FileSize, -1,
				fmt.Sprintf("chunk %q payload truncated: declares %d bytes", string(id), size))
		}
		switch {
		case bytes.Equal(id, idFmt):
			if dataDone {
				return malformed(blockID, offset, -1, "fmt chunk must appear before data chunk")
			}
			if err := h.readFmt(blockID, r, payloadStart, size); err != nil {
				return err
			}
			fmtDone = true
		case bytes.Equal(id, idData):
			if !fmtDone {
				return malformed(blockID, offset, -1, "data chunk appears before fmt chunk")
			}
			h.DataStart = payloadStart
			h.DataSize = uint32(size)
			dataDone = true
		}
		// 块按字对齐，奇数长度带 1 字节填充。
		next := payloadEnd + size%2
		if _, err := r.Seek(next, io.SeekStart); err != nil {
			return fault.New(fault.KindOther, blockID, next, -1, false,
				"chunk seek failed").WithOp("wav.decode").Wrap(err)
		}
		offset = next
	}
	if !fmtDone {
		return malformed(blockID, offset, -1, "missing fmt chunk")
	}
	if !dataDone {
		return malformed(blockID, offset, -1, "missing data chunk")
	}
	return nil
}

func (h *Header) readFmt(blockID string, r io.Reader, start, size int64) error {
	if size < 16 {
		return malformed(blockID, start, -1,
			fmt.Sprintf("fmt chunk too short: %d bytes", size))
	}
	buf := make([]byte, 16)
	if _, err := io.ReadFull(r, buf); err != nil {
		return malformed(blockID, start, -1, "truncated fmt payload")
	}
	h.AudioFormat = binary.LittleEndian.Uint16(buf[0:2])
	if h.AudioFormat != PCMFormatTag {
		return malformed(blockID, start, -1,
			fmt.Sprintf("unsupported audio format tag %d (integer PCM only)", h.AudioFormat))
	}
	h.Format.Channels = binary.LittleEndian.Uint16(buf[2:4])
	h.Format.SampleRate = binary.LittleEndian.Uint32(buf[4:8])
	h.ByteRate = binary.LittleEndian.Uint32(buf[8:12])
	h.BlockAlign = binary.LittleEndian.Uint16(buf[12:14])
	h.Format.BitsPerSample = binary.LittleEndian.Uint16(buf[14:16])
	if err := h.Format.Valid(); err != nil {
		return malformed(blockID, start+2, -1, "invalid fmt parameters").Wrap(err)
	}
	wantAlign := uint16(h.Format.Channels) * h.Format.BitsPerSample / 8
	if h.BlockAlign != wantAlign {
		return malformed(blockID, start+12, -1,
			fmt.Sprintf("block align %d != channels*bits/8 %d", h.BlockAlign, wantAlign))
	}
	if wantRate := h.Format.SampleRate * uint32(h.BlockAlign); h.ByteRate != wantRate {
		return malformed(blockID, start+8, -1,
			fmt.Sprintf("byte rate %d != sampleRate*blockAlign %d", h.ByteRate, wantRate))
	}
	return nil
}

// CheckPCM 校验 data 载荷的帧对齐与声明长度，返回整帧数。
func (h *Header) CheckPCM(blockID string, pcm []byte) (uint64, error) {
	if int64(len(pcm)) > int64(h.DataSize) {
		return 0, malformed(blockID, h.DataStart+int64(h.DataSize), -1,
			"more PCM bytes than data chunk declares")
	}
	if int64(len(pcm)) < int64(h.DataSize) {
		goodSample := h.sampleAt(int64(len(pcm)))
		return 0, malformed(blockID, h.DataStart+int64(len(pcm)), int64(goodSample),
			"data chunk truncated: payload shorter than declared size")
	}
	fs := int64(h.Format.FrameSize())
	if r := int64(len(pcm)) % fs; r != 0 {
		good := int64(len(pcm)) - r
		return 0, malformed(blockID, h.DataStart+good, int64(h.sampleAt(good)),
			"PCM payload is not frame aligned")
	}
	return uint64(len(pcm)) / uint64(fs), nil
}

// sampleAt 把 data 内的字节偏移换算为采样帧序号。
func (h *Header) sampleAt(rel int64) uint64 {
	fs := int64(h.Format.FrameSize())
	if fs <= 0 || rel < 0 {
		return 0
	}
	return uint64(rel / fs)
}

// SampleAt 暴露 data 相对偏移到采样帧序号的换算。
func (h *Header) SampleAt(relByteOffset int64) (uint64, error) {
	if relByteOffset < 0 {
		return 0, errors.New("wav: negative offset")
	}
	return h.sampleAt(relByteOffset), nil
}

func malformed(blockID string, byteOff, sampleOff int64, msg string) *fault.Error {
	return fault.New(fault.KindMalformedInput, blockID, byteOff, sampleOff, true,
		msg).WithOp("wav.decode")
}
