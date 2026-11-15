package wavio

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

type WriteHeader struct {
	SampleRate    uint32
	Channels      uint16
	BitsPerSample uint16
	IsFloat       bool
	ChannelMask   uint32
	UseExtensible bool
}

type Writer struct {
	w           io.WriteCloser
	hdr         WriteHeader
	dataLength  int64
	closed      bool
}

func NewWriter(w io.WriteCloser, hdr WriteHeader) (*Writer, error) {
	if hdr.Channels == 0 || hdr.SampleRate == 0 {
		return nil, fmt.Errorf("invalid write header")
	}
	writer := &Writer{w: w, hdr: hdr}
	if err := writer.writeChunks(); err != nil {
		return nil, err
	}
	return writer, nil
}

func (w *Writer) writeChunks() error {
	ext := w.hdr.UseExtensible
	blockAlign := int(w.hdr.Channels) * int(w.hdr.BitsPerSample) / 8
	avgBytes := int(w.hdr.SampleRate) * blockAlign
	fmtSize := uint32(16)
	if ext {
		fmtSize = 40
	}
	riffSize := uint32(4) + (8 + fmtSize) + 8
	buf := make([]byte, 0, 12+8+int(fmtSize)+8)
	buf = append(buf, 'R', 'I', 'F', 'F')
	buf = binary.LittleEndian.AppendUint32(buf, riffSize)
	buf = append(buf, 'W', 'A', 'V', 'E')
	buf = append(buf, 'f', 'm', 't', ' ')
	buf = binary.LittleEndian.AppendUint32(buf, fmtSize)
	formatTag := uint16(FormatPCM)
	if w.hdr.IsFloat {
		formatTag = FormatFloat
	}
	if ext {
		formatTag = FormatExtensible
	}
	buf = binary.LittleEndian.AppendUint16(buf, formatTag)
	buf = binary.LittleEndian.AppendUint16(buf, w.hdr.Channels)
	buf = binary.LittleEndian.AppendUint32(buf, w.hdr.SampleRate)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(avgBytes))
	buf = binary.LittleEndian.AppendUint16(buf, uint16(blockAlign))
	buf = binary.LittleEndian.AppendUint16(buf, w.hdr.BitsPerSample)
	if ext {
		buf = binary.LittleEndian.AppendUint16(buf, 22)
		buf = binary.LittleEndian.AppendUint16(buf, w.hdr.BitsPerSample)
		buf = binary.LittleEndian.AppendUint32(buf, w.hdr.ChannelMask)
		subformat := FormatPCM
		if w.hdr.IsFloat {
			subformat = FormatFloat
		}
		buf = binary.LittleEndian.AppendUint16(buf, uint16(subformat))
		buf = append(buf, make([]byte, 14)...)
	}
	buf = append(buf, 'd', 'a', 't', 'a')
	buf = binary.LittleEndian.AppendUint32(buf, 0)
	_, err := w.w.Write(buf)
	return err
}

func (w *Writer) WriteFrames(planar [][]float64) error {
	if w.closed {
		return fmt.Errorf("writer already closed")
	}
	channels := len(planar)
	if channels != int(w.hdr.Channels) {
		return fmt.Errorf("channel mismatch: got %d want %d", channels, w.hdr.Channels)
	}
	frames := len(planar[0])
	bytesPerSample := int(w.hdr.BitsPerSample) / 8
	buf := make([]byte, 0, frames*channels*bytesPerSample)
	for i := 0; i < frames; i++ {
		for ch := 0; ch < channels; ch++ {
			if len(planar[ch]) != frames {
				return fmt.Errorf("channel %d has %d frames, want %d", ch, len(planar[ch]), frames)
			}
			buf = w.encodeSample(buf, planar[ch][i])
		}
	}
	n, err := w.w.Write(buf)
	w.dataLength += int64(n)
	return err
}

func (w *Writer) encodeSample(buf []byte, sample float64) []byte {
	if w.hdr.IsFloat {
		if w.hdr.BitsPerSample == 32 {
			return binary.LittleEndian.AppendUint32(buf, math.Float32bits(float32(sample)))
		}
		return binary.LittleEndian.AppendUint64(buf, math.Float64bits(sample))
	}
	switch w.hdr.BitsPerSample {
	case 16:
		return binary.LittleEndian.AppendUint16(buf, floatToUint16(sample))
	case 24:
		clamped := clampInt(sample, -1, 1)
		value := int32(math.Round(clamped * 8388608.0))
		if value == 8388608 {
			value = 8388607
		}
		return append(buf, byte(value), byte(value>>8), byte(value>>16))
	default:
		clamped := clampInt(sample, -1, 1)
		value := int32(math.Round(clamped * (1 << 31)))
		if value == int32(-1<<31) {
			value = 2147483647
		}
		return binary.LittleEndian.AppendUint32(buf, uint32(value))
	}
}

func floatToUint16(sample float64) uint16 {
	clamped := clampInt(sample, -1, 1)
	value := int32(math.Round(clamped * 32768.0))
	if value >= 32768 {
		value = 32767
	}
	if value < -32768 {
		value = -32768
	}
	return uint16(value)
}

func clampInt(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

type patchSeeker interface {
	io.WriteCloser
	Seek(offset int64, whence int) (int64, error)
}

func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if seeker, ok := w.w.(patchSeeker); ok {
		dataSizePos := int64(12 + 8 + 16 + 4)
		if w.hdr.UseExtensible {
			dataSizePos = int64(12 + 8 + 40 + 4)
		}
		var sizeBytes [4]byte
		binary.LittleEndian.PutUint32(sizeBytes[:], uint32(w.dataLength))
		if _, err := seeker.Seek(dataSizePos, io.SeekStart); err != nil {
			return err
		}
		if _, err := seeker.Write(sizeBytes[:]); err != nil {
			return err
		}
		riffSize := uint32(uint64(4) + uint64(8) + uint64(dataSizePos-12) + uint64(8) + uint64(w.dataLength))
		binary.LittleEndian.PutUint32(sizeBytes[:], riffSize)
		if _, err := seeker.Seek(4, io.SeekStart); err != nil {
			return err
		}
		if _, err := seeker.Write(sizeBytes[:]); err != nil {
			return err
		}
	}
	return w.w.Close()
}

func WriteWAV(path string, hdr WriteHeader, planar [][]float64) error {
	file, err := createFile(path)
	if err != nil {
		return err
	}
	writer, err := NewWriter(file, hdr)
	if err != nil {
		file.Close()
		return err
	}
	if err := writer.WriteFrames(planar); err != nil {
		writer.Close()
		return err
	}
	return writer.Close()
}
