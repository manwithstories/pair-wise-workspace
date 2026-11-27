// Package pipeline 串联包接收/解码、时间线、音频处理与断点清单，
// 输出统一的 48kHz 立体声 16-bit PCM WAV，并保证断点续跑与全新跑逐字节一致。
package pipeline

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"os"

	"archivemedia/internal/audio"
	"archivemedia/internal/media"
)

// wavHeader 是 44 字节标准 PCM WAV 头（立体声、16-bit）。
const wavHeaderLen = 44

// seek 常量封装 io.Seek*，避免散落魔法数字。
const (
	ioSeekStart = io.SeekStart
	ioSeekEnd   = io.SeekEnd
)

// writeWAVHeader 写入标准 RIFF/WAVE 头；dataLen 为 PCM 字节数。
func writeWAVHeader(buf *bytes.Buffer, sampleRate, channels, bitsPerSample, dataLen int) {
	byteRate := sampleRate * channels * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8
	put := func(s string) { buf.WriteString(s) }
	putU32 := func(v uint32) { _ = binary.Write(buf, binary.LittleEndian, v) }
	putU16 := func(v uint16) { _ = binary.Write(buf, binary.LittleEndian, v) }

	put("RIFF")
	putU32(uint32(36 + dataLen))
	put("WAVE")
	put("fmt ")
	putU32(16) // PCM fmt chunk size
	putU16(1)  // PCM format
	putU16(uint16(channels))
	putU32(uint32(sampleRate))
	putU32(uint32(byteRate))
	putU16(uint16(blockAlign))
	putU16(uint16(bitsPerSample))
	put("data")
	putU32(uint32(dataLen))
}

// encodePCM16 把交错 float32 样本量化为 16-bit 小端 PCM，带硬限幅保护。
func encodePCM16(samples []float32) []byte {
	out := make([]byte, len(samples)*2)
	for i, s := range samples {
		v := float64(s)
		if v > 1 {
			v = 1
		} else if v < -1 {
			v = -1
		}
		n := int16(math.Round(v * 32767))
		binary.LittleEndian.PutUint16(out[i*2:], uint16(n))
	}
	return out
}

// WAVWriter 支持断点续跑的 WAV 输出：先写占位头，PCM 追加，最后回填长度。
type WAVWriter struct {
	path        string
	f           *os.File
	dataBytes   int
	sampleRate  int
	channels    int
	headerKnown bool
}

// NewWAVWriter 打开输出文件；resume=true 且文件已存在时追加 PCM 并保留原头。
func NewWAVWriter(path string, sampleRate, channels int, resume bool, existingDataBytes int) (*WAVWriter, error) {
	w := &WAVWriter{path: path, sampleRate: sampleRate, channels: channels}
	if resume && existingDataBytes > 0 {
		f, err := os.OpenFile(path, os.O_RDWR, 0o644)
		if err == nil {
			if _, err := f.Seek(0, ioSeekEnd); err != nil {
				f.Close()
				return nil, media.E(media.KindCheckpoint, "pipeline.wav-open",
					media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
					"定位输出尾部失败", err)
			}
			w.f = f
			w.dataBytes = existingDataBytes
			w.headerKnown = true
			return w, nil
		}
		// 文件不存在则退化为首跑。
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, media.E(media.KindCheckpoint, "pipeline.wav-open",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"创建输出 WAV 失败", err)
	}
	var head bytes.Buffer
	writeWAVHeader(&head, sampleRate, channels, 16, 0)
	if _, err := f.Write(head.Bytes()); err != nil {
		f.Close()
		return nil, media.E(media.KindAudio, "pipeline.wav-header",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"写 WAV 头失败", err)
	}
	w.f = f
	w.headerKnown = true
	return w, nil
}

// WriteBuffer 追加一个立体声浮点缓冲。
func (w *WAVWriter) WriteBuffer(buf *audio.Buffer) error {
	if buf == nil || buf.Frames == 0 {
		return nil
	}
	pcm := encodePCM16(buf.Samples)
	if _, err := w.f.Write(pcm); err != nil {
		return media.E(media.KindAudio, "pipeline.wav-write",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"写 PCM 失败", err)
	}
	w.dataBytes += len(pcm)
	return nil
}

// DataBytes 返回已写 PCM 字节数（不含头）。
func (w *WAVWriter) DataBytes() int { return w.dataBytes }

// Finalize 回填 RIFF/data 长度并 fsync 关闭。
func (w *WAVWriter) Finalize() error {
	if _, err := w.f.Seek(0, ioSeekStart); err != nil {
		return media.E(media.KindAudio, "pipeline.wav-finalize",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"回填定位失败", err)
	}
	var head bytes.Buffer
	writeWAVHeader(&head, w.sampleRate, w.channels, 16, w.dataBytes)
	if _, err := w.f.Write(head.Bytes()); err != nil {
		return media.E(media.KindAudio, "pipeline.wav-finalize",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"回填 WAV 头失败", err)
	}
	if err := w.f.Sync(); err != nil {
		return media.E(media.KindAudio, "pipeline.wav-finalize",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"fsync 输出失败", err)
	}
	return w.f.Close()
}

// Close 无 Finalize 时仅关闭句柄。
func (w *WAVWriter) Close() {
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
}
