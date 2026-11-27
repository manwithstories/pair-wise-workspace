// Package ingest 负责 WAV 容器解析与传输包记录的整理、去重、排序。
package ingest

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"github.com/go-audio/riff"
	"github.com/go-audio/wav"

	"bcastqc/internal/model"
)

// PCM 格式标签与支持的位深。
const (
	WaveFormatPCM        uint16 = 1
	WaveFormatExtensible uint16 = 0xFFFE
)

var supportedBitDepths = map[uint16]bool{8: true, 16: true, 24: true, 32: true}

// WAVInfo 保存 WAV 容器核验结果。
type WAVInfo struct {
	Path           string
	AudioFormat    uint16
	NumChans       uint16
	SampleRate     uint32
	BitsPerSample  uint16
	BlockAlign     uint16
	AvgBytesPerSec uint32
	// DataBytes 为 data 块声明的 PCM 字节数（不含 padding）。
	DataBytes int64
	// TotalFrames 为每声道采样帧数。
	TotalFrames int64
	// Chunks 按文件中出现顺序记录所有附属块（含 data）。
	Chunks []ChunkInfo
}

// ChunkInfo 描述一个 RIFF 子块。
type ChunkInfo struct {
	ID   string
	Size int64
	Pos  int64
}

// FormatSummary 返回便于终端展示的格式摘要。
func (w WAVInfo) FormatSummary() string {
	return fmt.Sprintf("PCM %dHz %dbit %dch blockAlign=%d dataBytes=%d frames=%d",
		w.SampleRate, w.BitsPerSample, w.NumChans, w.BlockAlign, w.DataBytes, w.TotalFrames)
}

// ParseWAVFile 打开并完整核验一个 WAV 文件。
// 解析过程严格校验 RIFF 边界：任何声明长度超出文件实际大小的块都会被拒绝。
func ParseWAVFile(path string) (*WAVInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: 打开 WAV 失败: %v", model.ErrInvalidInput, err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: 读取文件状态失败: %v", model.ErrInvalidInput, err)
	}
	if stat.Size() > model.MaxFileBytes {
		return nil, fmt.Errorf("%w: 文件长度 %d 超过上限 %d",
			model.ErrRiffStructure, stat.Size(), model.MaxFileBytes)
	}
	return ParseWAV(f, stat.Size())
}

// ParseWAV 从给定读取器核验 WAV 结构，fileSize 为容器实际可用字节数。
func ParseWAV(r io.ReadSeeker, fileSize int64) (*WAVInfo, error) {
	if fileSize < 12 {
		return nil, fmt.Errorf("%w: 文件过短，不足 RIFF 头 12 字节", model.ErrRiffStructure)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("%w: seek 失败: %v", model.ErrRiffStructure, err)
	}

	// 1) RIFF 主块头核验与越界长度拒绝。
	var head struct {
		RIFF     [4]byte
		Size     uint32
		WaveID   [4]byte
	}
	if err := binary.Read(r, binary.LittleEndian, &head); err != nil {
		return nil, fmt.Errorf("%w: 读取 RIFF 头失败: %v", model.ErrRiffStructure, err)
	}
	if !bytes.Equal(head.RIFF[:], riff.RiffID[:]) {
		return nil, fmt.Errorf("%w: 缺少 RIFF 标识，得到 %q", model.ErrRiffStructure, head.RIFF)
	}
	if !bytes.Equal(head.WaveID[:], riff.WavFormatID[:]) {
		return nil, fmt.Errorf("%w: 缺少 WAVE 标识，得到 %q", model.ErrRiffStructure, head.WaveID)
	}
	if int64(head.Size)+8 > fileSize {
		return nil, fmt.Errorf("%w: RIFF 声明长度 %d 越界（文件实际 %d 字节）",
			model.ErrRiffStructure, int64(head.Size)+8, fileSize)
	}

	// 2) 借助 go-audio 解码器核验 fmt 块参数（编码、位深、采样率等）。
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("%w: seek 重置失败: %v", model.ErrRiffStructure, err)
	}
	dec := wav.NewDecoder(r)
	if !dec.IsValidFile() {
		return nil, fmt.Errorf("%w: go-audio 核验失败: %v", model.ErrRiffStructure, dec.Err())
	}
	info := &WAVInfo{
		AudioFormat:    dec.WavAudioFormat,
		NumChans:       dec.NumChans,
		SampleRate:     dec.SampleRate,
		BitsPerSample:  dec.BitDepth,
		BlockAlign:     dec.NumChans * dec.BitDepth / 8,
		AvgBytesPerSec: dec.AvgBytesPerSec,
	}
	if info.NumChans == 0 {
		return nil, fmt.Errorf("%w: 声道数为 0", model.ErrRiffStructure)
	}
	if !supportedBitDepths[info.BitsPerSample] {
		return nil, fmt.Errorf("%w: 不支持的 PCM 位深 %d", model.ErrUnsupportedCodec, info.BitsPerSample)
	}
	if info.AudioFormat != WaveFormatPCM {
		return nil, fmt.Errorf("%w: 编码标签 %d 非 PCM(1)，不支持的编码",
			model.ErrUnsupportedCodec, info.AudioFormat)
	}
	if _, err := model.NewClock(dec.SampleRate); err != nil {
		return nil, err
	}
	if expect := uint32(info.BlockAlign) * dec.SampleRate; dec.AvgBytesPerSec != expect {
		return nil, fmt.Errorf("%w: AvgBytesPerSec=%d 与格式推算 %d 不一致",
			model.ErrRiffStructure, dec.AvgBytesPerSec, expect)
	}

	// 3) 自行枚举全部子块（任意顺序），逐块做越界检查，兼容奇数长度 padding。
	if err := info.collectChunks(r, fileSize); err != nil {
		return nil, err
	}
	if info.DataBytes == 0 {
		return nil, fmt.Errorf("%w: 未找到 data 块或 PCM 数据为空", model.ErrRiffStructure)
	}
	if info.DataBytes%int64(info.BlockAlign) != 0 {
		return nil, fmt.Errorf("%w: data 长度 %d 不是块对齐 %d 的整数倍，疑似截断",
			model.ErrRiffStructure, info.DataBytes, info.BlockAlign)
	}
	info.TotalFrames = info.DataBytes / int64(info.BlockAlign)
	if err := model.CheckSampleCount(info.TotalFrames); err != nil {
		return nil, err
	}
	return info, nil
}

// collectChunks 从偏移 12 起顺序枚举子块并验证声明范围不越过文件边界。
func (w *WAVInfo) collectChunks(r io.ReadSeeker, fileSize int64) error {
	if _, err := r.Seek(12, io.SeekStart); err != nil {
		return fmt.Errorf("%w: seek 子块起点失败: %v", model.ErrRiffStructure, err)
	}
	pos := int64(12)
	// RIFF 主块声明的内部范围上限（不含末尾 padding 之外的字节）。
	riffEnd := fileSize
	for pos+8 <= riffEnd {
		var raw struct {
			ID   [4]byte
			Size uint32
		}
		if err := binary.Read(r, binary.LittleEndian, &raw); err != nil {
			return fmt.Errorf("%w: 读取偏移 %d 处子块头失败: %v",
				model.ErrRiffStructure, pos, err)
		}
		declared := int64(raw.Size)
		// 奇数长度载荷后带 1 字节 padding。
		padded := declared
		if declared%2 == 1 {
			padded++
		}
		if pos+8+padded > riffEnd {
			return fmt.Errorf("%w: 块 %q 声明长度 %d 在偏移 %d 处越界",
				model.ErrRiffStructure, raw.ID, declared, pos)
		}
		w.Chunks = append(w.Chunks, ChunkInfo{
			ID:   string(raw.ID[:]),
			Size: declared,
			Pos:  pos,
		})
		if raw.ID == riff.DataFormatID {
			w.DataBytes = declared
		}
		next := pos + 8 + padded
		if _, err := r.Seek(next, io.SeekStart); err != nil {
			return fmt.Errorf("%w: 跳过块 %q 失败: %v",
				model.ErrRiffStructure, raw.ID, err)
		}
		pos = next
	}
	return nil
}
