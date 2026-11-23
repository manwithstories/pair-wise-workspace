// Package engine 把 RTP 重排、WAV 校验、切分、静音、指纹、fMP4 封装
// 与断点恢复子系统串联为一条有背压的确定性处理流水线。
package engine

import (
	"bytes"
	"encoding/binary"

	"media-pipeline/internal/clock"
)

// BuildWAV 用给定时间基准与 PCM 载荷构造最小合法 RIFF/WAV 字节。
func BuildWAV(tb clock.Timebase, pcm []byte) []byte {
	blockAlign := uint16(tb.FrameSize())
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+len(pcm)))
	b.WriteString("WAVE")
	b.WriteString("fmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1)) // 整数 PCM
	binary.Write(&b, binary.LittleEndian, tb.Channels)
	binary.Write(&b, binary.LittleEndian, tb.SampleRate)
	binary.Write(&b, binary.LittleEndian, tb.SampleRate*uint32(blockAlign))
	binary.Write(&b, binary.LittleEndian, blockAlign)
	binary.Write(&b, binary.LittleEndian, tb.BitsPerSample)
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}
