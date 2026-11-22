// Package audio 适配原始音频样本格式：在 fMP4 解出的字节负载与内核定点 PCM
// 帧之间转换。仅支持 16/24-bit 小端无压缩 PCM（ipcm），其它格式拒绝。
package audio

import (
	"encoding/binary"

	"mediacheck/internal/model"
)

// Frame 是一帧多声道 PCM：每个声道一个 int32 定点样本，Q15（16-bit）或 Q23（24-bit）。
type Frame struct {
	Samples []int32 // 长度等于声道数
}

// PCMDecoder 把交错字节负载解码为帧序列，并记录异常计数。
type PCMDecoder struct {
	BitDepth uint16
	Channels int
}

// DecodeStats 收集解码期质量信号，供响度/报告模块消费。
type DecodeStats struct {
	SilentFrames   int
	ClippedSamples int
	TotalSamples   int
}

func NewDecoder(meta model.TrackMeta) (*PCMDecoder, error) {
	if meta.Codec != "ipcm" {
		return nil, &model.MediaError{Category: model.ErrUnknownCodec, TrackID: meta.TrackID, Message: "pcm adapter only supports ipcm: " + meta.Codec}
	}
	if meta.BitDepth != 16 && meta.BitDepth != 24 {
		return nil, &model.MediaError{Category: model.ErrUnknownCodec, TrackID: meta.TrackID, Message: "unsupported bit depth"}
	}
	ch := meta.Layout.Channels()
	if ch == 0 {
		return nil, &model.MediaError{Category: model.ErrUnknownLayout, TrackID: meta.TrackID, Message: "decoder needs known layout"}
	}
	return &PCMDecoder{BitDepth: meta.BitDepth, Channels: ch}, nil
}

// Decode 把交错 PCM 字节解码为 Frame；字节数不是帧整数倍时返回截断错误。
func (d *PCMDecoder) Decode(payload []byte) ([]Frame, DecodeStats, error) {
	bytesPerSample := int(d.BitDepth) / 8
	frameBytes := bytesPerSample * d.Channels
	if len(payload)%frameBytes != 0 {
		return nil, DecodeStats{}, &model.MediaError{Category: model.ErrTruncatedBox, Message: "pcm payload not frame aligned"}
	}
	n := len(payload) / frameBytes
	frames := make([]Frame, n)
	var st DecodeStats
	peak := d.peakBound()
	for f := 0; f < n; f++ {
		fr := Frame{Samples: make([]int32, d.Channels)}
		silent := true
		for c := 0; c < d.Channels; c++ {
			off := f*frameBytes + c*bytesPerSample
			v := d.readSample(payload[off : off+bytesPerSample])
			fr.Samples[c] = v
			st.TotalSamples++
			if v != 0 {
				silent = false
			}
			if v >= peak-1 || v <= -peak {
				st.ClippedSamples++
			}
		}
		if silent {
			st.SilentFrames++
		}
		frames[f] = fr
	}
	return frames, st, nil
}

func (d *PCMDecoder) readSample(b []byte) int32 {
	switch d.BitDepth {
	case 16:
		return int32(int16(binary.LittleEndian.Uint16(b)))
	case 24:
		// 24-bit 有符号小端 → int32，符号扩展。
		v := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
		if v&0x800000 != 0 {
			v |= 0xFF000000
		}
		return int32(v)
	default:
		return 0
	}
}

func (d *PCMDecoder) peakBound() int32 {
	if d.BitDepth == 24 {
		return 1 << 23
	}
	return 1 << 15
}

// Encode 把帧序列编码回交错 16-bit 小端 PCM（再封装统一输出 16-bit）。
func Encode16(frames []Frame, channels int) []byte {
	out := make([]byte, 0, len(frames)*channels*2)
	var buf [2]byte
	for _, fr := range frames {
		for c := 0; c < channels; c++ {
			v := fr.Samples[c]
			// 限幅到 int16，防止重采样/混音后溢出。
			if v > 32767 {
				v = 32767
			} else if v < -32768 {
				v = -32768
			}
			binary.LittleEndian.PutUint16(buf[:], uint16(int16(v)))
			out = append(out, buf[0], buf[1])
		}
	}
	return out
}
