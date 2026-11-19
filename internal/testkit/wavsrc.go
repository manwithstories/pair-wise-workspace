// Package testkit 自生成正常、损坏与边界采集素材，
// 全部由确定性 PRNG 驱动，不引用任何外部文件。
package testkit

import (
	"encoding/binary"
	"math"

	"github.com/pion/rtp"
)

// PCMFormat 描述合成素材的 PCM 格式。
type PCMFormat struct {
	SampleRate    uint32
	Channels      uint16
	BitsPerSample uint16
}

// FrameBytes 返回单帧字节数。
func (f PCMFormat) FrameBytes() int {
	return int(f.Channels) * int(f.BitsPerSample) / 8
}

// SynthesizePCM 生成确定性 PCM 采样：正弦信号中嵌入削波样本与静音帧，
// 使报告阶段的 clipped/silent 指标有非退化输入。
func SynthesizePCM(f PCMFormat, frameIndex, frameCount int, seed int64) []byte {
	buf := make([]byte, frameCount*f.FrameBytes())
	freq := 220.0 + float64(seed%7)*55.0
	for i := 0; i < frameCount; i++ {
		globalFrame := frameIndex*frameCount + i
		for ch := 0; ch < int(f.Channels); ch++ {
			var v int16
			switch {
			case globalFrame%97 == 0:
				v = math.MaxInt16 // 周期性削波样本
			case globalFrame%53 == 0:
				v = 0 // 周期性静音样本
			default:
				v = int16(8000 * math.Sin(2*math.Pi*freq*float64(globalFrame)/float64(f.SampleRate)))
			}
			off := (i*int(f.Channels) + ch) * 2
			binary.LittleEndian.PutUint16(buf[off:], uint16(v))
		}
	}
	return buf
}

// BuildWAVBlock 把 PCM 字节封装为最小 RIFF/WAVE 块（fmt + data）。
func BuildWAVBlock(f PCMFormat, pcm []byte) []byte {
	dataLen := len(pcm)
	total := 12 + 8 + 16 + 8 + dataLen
	buf := make([]byte, 0, total+total&1)
	buf = append(buf, 'R', 'I', 'F', 'F')
	buf = binary.LittleEndian.AppendUint32(buf, uint32(4+(8+16)+(8+dataLen)))
	buf = append(buf, 'W', 'A', 'V', 'E')
	// fmt chunk
	buf = append(buf, 'f', 'm', 't', ' ')
	buf = binary.LittleEndian.AppendUint32(buf, 16)
	buf = binary.LittleEndian.AppendUint16(buf, 1) // PCM
	buf = binary.LittleEndian.AppendUint16(buf, f.Channels)
	buf = binary.LittleEndian.AppendUint32(buf, f.SampleRate)
	byteRate := f.SampleRate * uint32(f.FrameBytes())
	buf = binary.LittleEndian.AppendUint32(buf, byteRate)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(f.FrameBytes()))
	buf = binary.LittleEndian.AppendUint16(buf, f.BitsPerSample)
	// data chunk
	buf = append(buf, 'd', 'a', 't', 'a')
	buf = binary.LittleEndian.AppendUint32(buf, uint32(dataLen))
	buf = append(buf, pcm...)
	return buf
}

// TruncateWAV 模拟 WAV 头截断：只保留前缀字节。
func TruncateWAV(block []byte, keep int) []byte {
	if keep >= len(block) {
		return block
	}
	return append([]byte(nil), block[:keep]...)
}

// BuildRTPPacket 构造一个 PT=96 的 RTP 包（SSRC 由设备派生）。
func BuildRTPPacket(seq uint16, ts uint32, ssrc uint32, payload []byte) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           ssrc,
		},
		Payload: append([]byte(nil), payload...),
	}
}

// MutatePayload 翻转一个字节，用于制造“同序号内容冲突”的重复副本。
func MutatePayload(p []byte) []byte {
	out := append([]byte(nil), p...)
	if len(out) > 0 {
		out[len(out)/2] ^= 0xff
	}
	return out
}
