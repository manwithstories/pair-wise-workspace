package gen

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"math"
	"os"
)

// WAVSpec describes a fixture to encode. Extensible selects
// WAVE_FORMAT_EXTENSIBLE and requires a channel mask.
type WAVSpec struct {
	SampleRate  int
	Channels    int
	Bits        int
	Float       bool
	Extensible  bool
	ChannelMask uint32
	Seconds     float64
}

type SampleFunc func(frame, channel int) float64

// WriteWAV streams a synthetic signal to disk in bounded blocks.
func WriteWAV(path string, spec WAVSpec, sample SampleFunc) error {
	if spec.SampleRate <= 0 || spec.Channels <= 0 || spec.Seconds < 0 {
		return fmt.Errorf("invalid wav spec")
	}
	bytesPerSample := (spec.Bits + 7) / 8
	if bytesPerSample == 0 {
		return fmt.Errorf("invalid sample depth")
	}
	blockAlign := spec.Channels * bytesPerSample
	frames := int(math.Round(spec.Seconds * float64(spec.SampleRate)))
	dataBytes := int64(frames) * int64(blockAlign)
	fmtChunk := buildFmt(spec)
	riffSize := int64(4+8+len(fmtChunk)+8) + dataBytes + dataBytes%2
	if riffSize > math.MaxUint32 {
		return fmt.Errorf("wav too large")
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 128*1024)
	header := make([]byte, 0, 12+8+len(fmtChunk)+8)
	header = appendASCII(header, "RIFF")
	header = appendU32(header, uint32(riffSize))
	header = appendASCII(header, "WAVE")
	header = appendASCII(header, "fmt ")
	header = appendU32(header, uint32(len(fmtChunk)))
	header = append(header, fmtChunk...)
	header = appendASCII(header, "data")
	header = appendU32(header, uint32(dataBytes))
	if _, err := w.Write(header); err != nil {
		return err
	}

	const blockFrames = 4096
	raw := make([]byte, blockFrames*blockAlign)
	for start := 0; start < frames; start += blockFrames {
		count := frames - start
		if count > blockFrames {
			count = blockFrames
		}
		buf := raw[:count*blockAlign]
		for i := 0; i < count; i++ {
			for ch := 0; ch < spec.Channels; ch++ {
				v := sample(start+i, ch)
				encode(buf[(i*spec.Channels+ch)*bytesPerSample:], v, spec)
			}
		}
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}
	if dataBytes%2 == 1 {
		if err := w.WriteByte(0); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return f.Close()
}

func buildFmt(spec WAVSpec) []byte {
	bytesPerSample := (spec.Bits + 7) / 8
	blockAlign := spec.Channels * bytesPerSample
	byteRate := spec.SampleRate * blockAlign
	tag := uint16(1)
	if spec.Float {
		tag = 3
	}
	size := 16
	if spec.Extensible {
		size = 40
	}
	b := make([]byte, size)
	rawTag := tag
	if spec.Extensible {
		rawTag = 0xfffe
	}
	binary.LittleEndian.PutUint16(b[0:], rawTag)
	binary.LittleEndian.PutUint16(b[2:], uint16(spec.Channels))
	binary.LittleEndian.PutUint32(b[4:], uint32(spec.SampleRate))
	binary.LittleEndian.PutUint32(b[8:], uint32(byteRate))
	binary.LittleEndian.PutUint16(b[12:], uint16(blockAlign))
	binary.LittleEndian.PutUint16(b[14:], uint16(spec.Bits))
	if spec.Extensible {
		binary.LittleEndian.PutUint16(b[16:], 22)
		binary.LittleEndian.PutUint16(b[18:], uint16(spec.Bits))
		binary.LittleEndian.PutUint32(b[20:], spec.ChannelMask)
		copy(b[24:], []byte{
			byte(tag), byte(tag >> 8), 0x00, 0x00, 0x00, 0x00, 0x10, 0x00,
			0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71,
		})
	}
	return b
}

func encode(dst []byte, v float64, spec WAVSpec) {
	if spec.Float {
		if spec.Bits == 32 {
			binary.LittleEndian.PutUint32(dst, math.Float32bits(float32(v)))
		} else {
			binary.LittleEndian.PutUint64(dst, math.Float64bits(v))
		}
		return
	}
	switch spec.Bits {
	case 8:
		x := clampInt(int64(math.Round(v*128))+128, 0, 255)
		dst[0] = byte(x)
	case 16:
		x := clampInt(int64(math.Round(v*32768)), -32768, 32767)
		binary.LittleEndian.PutUint16(dst, uint16(int16(x)))
	case 24:
		x := clampInt(int64(math.Round(v*8388608)), -8388608, 8388607)
		u := uint32(x)
		dst[0], dst[1], dst[2] = byte(u), byte(u>>8), byte(u>>16)
	case 32:
		x := clampInt(int64(math.Round(v*2147483648)), -2147483648, 2147483647)
		binary.LittleEndian.PutUint32(dst, uint32(int32(x)))
	}
}

func clampInt(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func appendASCII(dst []byte, s string) []byte { return append(dst, s...) }
func appendU32(dst []byte, v uint32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	return append(dst, b[:]...)
}
