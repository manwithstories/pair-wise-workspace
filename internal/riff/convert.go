package riff

import (
	"encoding/binary"
	"fmt"
	"math"
)

// ConvertSamples converts interleaved WAV sample bytes into normalized
// float64 values. Non-finite float samples are counted and replaced by zero
// so a single corrupt sample cannot poison an entire analysis.
func ConvertSamples(raw []byte, f Format, dst []float64) (nanCount int, err error) {
	if len(dst) < len(raw)/f.BytesPerSample() {
		return 0, fmt.Errorf("sample destination too small")
	}
	out := dst[:len(raw)/f.BytesPerSample()]
	switch f.Tag {
	case formatPCM:
		switch f.BitsPerSample {
		case 8:
			for i, v := range raw {
				out[i] = (float64(v) - 128) / 128
			}
		case 16:
			for i := 0; i < len(raw); i += 2 {
				v := int16(binary.LittleEndian.Uint16(raw[i : i+2]))
				out[i/2] = float64(v) / 32768
			}
		case 24:
			for i := 0; i < len(raw); i += 3 {
				u := uint32(raw[i]) | uint32(raw[i+1])<<8 | uint32(raw[i+2])<<16
				if u&0x800000 != 0 {
					u |= 0xff000000
				}
				out[i/3] = float64(int32(u)) / 8388608
			}
		case 32:
			for i := 0; i < len(raw); i += 4 {
				v := int32(binary.LittleEndian.Uint32(raw[i : i+4]))
				out[i/4] = float64(v) / 2147483648
			}
		default:
			return 0, fmt.Errorf("%w: PCM depth %d", ErrUnsupportedFormat, f.BitsPerSample)
		}
	case formatFloat:
		switch f.BitsPerSample {
		case 32:
			for i := 0; i < len(raw); i += 4 {
				v := float64(math.Float32frombits(binary.LittleEndian.Uint32(raw[i : i+4])))
				if !isFinite(v) {
					nanCount++
					v = 0
				}
				out[i/4] = v
			}
		case 64:
			for i := 0; i < len(raw); i += 8 {
				v := math.Float64frombits(binary.LittleEndian.Uint64(raw[i : i+8]))
				if !isFinite(v) {
					nanCount++
					v = 0
				}
				out[i/8] = v
			}
		default:
			return 0, fmt.Errorf("%w: float depth %d", ErrUnsupportedFormat, f.BitsPerSample)
		}
	default:
		return 0, fmt.Errorf("%w: tag 0x%04x", ErrUnsupportedFormat, f.Tag)
	}
	return nanCount, nil
}

func isFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
