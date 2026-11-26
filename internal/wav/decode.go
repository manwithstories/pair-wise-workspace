package wav

import (
	"encoding/binary"
	"fmt"

	"audiorestore/internal/model"
)

// DecodeBlock de-interleaves one block's PCM bytes into per-channel sample
// slices of length Frames. A missing, corrupt or truncated block returns
// (nil, nil): the caller keeps the gap instead of inventing samples.
func DecodeBlock(b FrameBlock, f Format) ([][]int, error) {
	if b.Missing || b.Corrupt || b.Truncated || len(b.Data) == 0 {
		return nil, nil
	}
	chans := f.Channels
	out := make([][]int, chans)
	for c := range out {
		out[c] = make([]int, b.Frames)
	}
	bytesPerSample := int(f.BitDepth) / 8
	stride := f.BlockAlign
	if len(b.Data) < b.Frames*stride {
		return nil, fmt.Errorf("block %d: data shorter than declared frames", b.Index)
	}
	for frame := 0; frame < b.Frames; frame++ {
		base := frame*stride
		for c := 0; c < chans; c++ {
			off := base + c*bytesPerSample
			v, err := decodeSample(b.Data[off:off+bytesPerSample], f.BitDepth)
			if err != nil {
				return nil, fmt.Errorf("block %d frame %d ch %d: %w", b.Index, frame, c, err)
			}
			out[c][frame] = v
		}
	}
	return out, nil
}

func decodeSample(p []byte, bits uint16) (int, error) {
	switch bits {
	case 16:
		return int(int16(binary.LittleEndian.Uint16(p))), nil
	case 24:
		// Signed 24-bit little endian, sign-extended into int32.
		u := uint32(p[0]) | uint32(p[1])<<8 | uint32(p[2])<<16
		if u&0x800000 != 0 {
			u |= 0xff000000
		}
		return int(int32(u)), nil
	case 32:
		return int(int32(binary.LittleEndian.Uint32(p))), nil
	default:
		return 0, model.TrackedError{Kind: model.ErrUnsupportedFormat,
			Msg: fmt.Sprintf("bit depth %d", bits)}
	}
}

// EncodeSample writes one signed PCM sample in the carrier's bit depth.
// Values are saturated to the representable range so a generated tone
// cannot wrap into a click.
func EncodeSample(p []byte, v int, bits uint16) {
	switch bits {
	case 16:
		binary.LittleEndian.PutUint16(p, uint16(saturate(v, 1<<15)))
	case 24:
		s := uint32(saturate(v, 1<<23))
		p[0] = byte(s)
		p[1] = byte(s >> 8)
		p[2] = byte(s >> 16)
	case 32:
		binary.LittleEndian.PutUint32(p, uint32(saturate(v, 1<<31)))
	}
}

func saturate(v, maxNeg int) int {
	if v < -maxNeg {
		return -maxNeg
	}
	if v > maxNeg-1 {
		return maxNeg - 1
	}
	return v
}
