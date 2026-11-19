package audio

import (
	"encoding/binary"
	"math"

	"avpipe/internal/clock"
	"avpipe/internal/fault"
)

// Block is one decoded multichannel PCM chunk. Data is interleaved in the
// block layout's channel order and normalized to [-1,1) floats so downstream
// stages never depend on the on-disk integer width.
type Block struct {
	StartFrame int64      `json:"start_frame"`
	StartTick  clock.Tick `json:"start_tick"`
	SampleRate int        `json:"sample_rate"`
	Layout     Layout     `json:"layout"`
	Frames     int        `json:"frames"`
	Data       []float64  `json:"-"`
}

// EndTick is the exclusive end of the block on the 90 kHz timeline.
func (b Block) EndTick() clock.Tick {
	t, _ := clock.SamplesToTicks(b.StartFrame+int64(b.Frames), b.SampleRate)
	return t
}

// Bytes is the memory charged to a bounded queue, as 16-bit PCM equivalents.
func (b Block) Bytes() int64 { return int64(b.Frames * len(b.Layout.Channels) * 2) }

// Decode splits parsed PCM into fixed-size blocks. Each block carries the
// 90 kHz start tick derived from its absolute frame index, so block
// boundaries can never drift relative to the sample clock.
func Decode(src []byte, info Info, blockFrames int) ([]Block, error) {
	if blockFrames <= 0 {
		return nil, fault.New(fault.CodeInvalidArgument, "audio.wav.decode",
			"block size must be positive, got %d", blockFrames)
	}
	bytesPer := info.Format.BitsPerSample / 8
	align := info.Format.BlockAlign()
	total := info.BlockFrames
	blocks := make([]Block, 0, (total+blockFrames-1)/blockFrames)
	for start := 0; start < total; start += blockFrames {
		n := blockFrames
		if start+n > total {
			n = total - start
		}
		data := make([]float64, n*info.Format.Channels)
		base := info.DataOffset + int64(start*align)
		for f := 0; f < n; f++ {
			for c := 0; c < info.Format.Channels; c++ {
				off := base + int64(f*align+c*bytesPer)
				data[f*info.Format.Channels+c] = readSample(src, off, bytesPer)
			}
		}
		tick, _ := clock.SamplesToTicks(int64(start), info.Format.SampleRate)
		blocks = append(blocks, Block{
			StartFrame: int64(start), StartTick: tick,
			SampleRate: info.Format.SampleRate, Layout: info.Format.Layout,
			Frames: n, Data: data,
		})
	}
	return blocks, nil
}

func readSample(src []byte, off int64, width int) float64 {
	switch width {
	case 1:
		return (float64(src[off]) - 128) / 128
	case 2:
		return float64(int16(binary.LittleEndian.Uint16(src[off:off+2]))) / 32768
	case 3:
		u := uint32(src[off]) | uint32(src[off+1])<<8 | uint32(src[off+2])<<16
		v := int32(u<<8) >> 8
		return float64(v) / 8388608
	default:
		v := int32(binary.LittleEndian.Uint32(src[off : off+4]))
		return float64(v) / 2147483648
	}
}

// Encode builds a canonical RIFF/WAVE image from normalized samples.
func Encode(f Format, samples []float64, frames int) ([]byte, error) {
	if err := f.Layout.Validate(); err != nil {
		return nil, err
	}
	align := f.BlockAlign()
	if align <= 0 || frames < 0 || len(samples) != frames*f.Channels {
		return nil, fault.New(fault.CodeInvalidArgument, "audio.wav.encode",
			"sample buffer has %d values, want %d frames x %d channels",
			len(samples), frames, f.Channels)
	}
	dataBytes := frames * align
	extensible := f.ChannelMask != 0
	fmtSize := 16
	if extensible {
		fmtSize = 40
	}
	out := make([]byte, 20+fmtSize+8+dataBytes)
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)-8))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], uint32(fmtSize))
	tag := uint16(wavFormatPCM)
	if extensible {
		tag = wavFormatExtensible
	}
	binary.LittleEndian.PutUint16(out[20:22], tag)
	binary.LittleEndian.PutUint16(out[22:24], uint16(f.Channels))
	binary.LittleEndian.PutUint32(out[24:28], uint32(f.SampleRate))
	binary.LittleEndian.PutUint32(out[28:32], uint32(f.SampleRate*align))
	binary.LittleEndian.PutUint16(out[32:34], uint16(align))
	binary.LittleEndian.PutUint16(out[34:36], uint16(f.BitsPerSample))
	if extensible {
		binary.LittleEndian.PutUint16(out[36:38], 22)
		binary.LittleEndian.PutUint16(out[38:40], uint16(f.BitsPerSample))
		binary.LittleEndian.PutUint32(out[40:44], f.ChannelMask)
		copy(out[44:60], []byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00,
			0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71})
	}
	off := 20 + fmtSize
	copy(out[off:off+4], "data")
	binary.LittleEndian.PutUint32(out[off+4:off+8], uint32(dataBytes))
	off += 8
	bytesPer := f.BitsPerSample / 8
	for i, v := range samples {
		writeSample(out[off+i*bytesPer:], v, bytesPer)
	}
	return out, nil
}

func writeSample(dst []byte, v float64, width int) {
	if v > 0.999999 {
		v = 0.999999
	}
	if v < -1 {
		v = -1
	}
	switch width {
	case 1:
		dst[0] = byte(int(math.Round(v*127)) + 128)
	case 2:
		binary.LittleEndian.PutUint16(dst, uint16(int16(math.Round(v*32767))))
	case 3:
		u := int32(math.Round(v * 8388607))
		dst[0], dst[1], dst[2] = byte(u), byte(u>>8), byte(u>>16)
	default:
		binary.LittleEndian.PutUint32(dst, uint32(int32(math.Round(v*2147483647))))
	}
}
