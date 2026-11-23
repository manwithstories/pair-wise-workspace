package silence

import (
	"encoding/binary"

	"media-pipeline/internal/fault"
	"media-pipeline/internal/media"
)

// decodeFrame 把一帧交织的整数 PCM 字节解码为带符号采样点。
// 仅支持整数 PCM：8bit 无符号偏移，16/24/32bit 二补码小端。
func decodeFrame(raw []byte, bits uint16, channels uint16) []int32 {
	out := make([]int32, channels)
	bytesPer := int(bits / 8)
	for c := uint16(0); c < channels; c++ {
		off := int(c) * bytesPer
		switch bits {
		case 8: // WAV 8bit PCM 为无符号，零点 128
			out[c] = int32(raw[off]) - 128
		case 16:
			out[c] = int32(int16(binary.LittleEndian.Uint16(raw[off:])))
		case 24:
			v := uint32(raw[off]) | uint32(raw[off+1])<<8 | uint32(raw[off+2])<<16
			if v&0x800000 != 0 {
				v |= 0xFF000000 // 符号扩展
			}
			out[c] = int32(v)
		case 32:
			out[c] = int32(binary.LittleEndian.Uint32(raw[off:]))
		}
	}
	return out
}

// Analyze 是便捷入口：对单个块跑检测并返回静音区间。
func Analyze(d *Detector, b media.Block) ([]Interval, error) {
	if d == nil {
		return nil, fault.New(fault.KindOther, b.ID, -1, -1, false,
			"nil silence detector").WithOp("silence.analyze")
	}
	return d.Push(b)
}
