// Package riff implements the RIFF/WAVE container subset used by the
// loudness tools. Chunk metadata is scanned first; sample data is read in
// bounded pieces so memory use is independent of the file duration.
package riff

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	formatPCM        = 0x0001
	formatFloat      = 0x0003
	formatExtensible = 0xfffe

	// A few encoders use this value while writing a non-seekable stream.
	streamingSize = 0xffffffff
)

var (
	ErrMalformed             = errors.New("malformed wav")
	ErrUnsupportedFormat     = errors.New("unsupported_format")
	ErrUnsupportedLayout     = errors.New("unsupported_layout")
	ErrUnsupportedSampleRate = errors.New("unsupported_sample_rate")
)

// Format is the parsed WAVEFORMATEX/WAVEFORMATEXTENSIBLE description.
type Format struct {
	SampleRate    int
	Channels      int
	BitsPerSample int
	BlockAlign    int
	ByteRate      int
	Tag           uint16
	RawTag        uint16
	Extensible    bool
	ChannelMask   uint32
	DataBytes     uint64
}

func (f Format) BytesPerSample() int { return (f.BitsPerSample + 7) / 8 }
func (f Format) IsFloat() bool       { return f.Tag == formatFloat }

// Decoder streams interleaved float64 samples from one WAV file.
type Decoder struct {
	file       *os.File
	format     Format
	dataOffset int64
	remaining  int64
	raw        []byte
	nans       uint64
}

// Open parses the RIFF and fmt/data chunks. It does not read sample data.
func Open(path string) (*Decoder, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	d, err := openFile(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return d, nil
}

func openFile(f *os.File) (*Decoder, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	if size < 12 {
		return nil, fmt.Errorf("%w: file too small", ErrMalformed)
	}
	head := make([]byte, 12)
	if _, err := f.ReadAt(head, 0); err != nil {
		return nil, fmt.Errorf("%w: header: %v", ErrMalformed, err)
	}
	if string(head[0:4]) != "RIFF" || string(head[8:12]) != "WAVE" {
		return nil, fmt.Errorf("%w: not a RIFF/WAVE file", ErrUnsupportedFormat)
	}

	var fmtBytes []byte
	var dataOffset, dataBytes int64 = -1, 0
	for off := int64(12); off+8 <= size; {
		h := make([]byte, 8)
		if _, err := f.ReadAt(h, off); err != nil {
			break
		}
		id := string(h[0:4])
		chunkSize := int64(binary.LittleEndian.Uint32(h[4:8]))
		// A streaming writer can leave the size at FFFFFFFF.
		streaming := uint32(chunkSize) == streamingSize
		body := off + 8
		end := body + chunkSize
		if chunkSize < 0 {
			return nil, fmt.Errorf("%w: chunk size overflow", ErrMalformed)
		}
		if id == "fmt " {
			want := chunkSize
			if want > 64 {
				want = 64
			}
			if want < 16 || body+want > size {
				return nil, fmt.Errorf("%w: bad fmt chunk", ErrMalformed)
			}
			fmtBytes = make([]byte, want)
			if _, err := f.ReadAt(fmtBytes, body); err != nil {
				return nil, fmt.Errorf("%w: fmt: %v", ErrMalformed, err)
			}
		}
		if id == "data" && dataOffset < 0 {
			avail := end
			if streaming || avail > size {
				avail = size
			}
			if avail < body {
				avail = body
			}
			dataOffset, dataBytes = body, avail-body
			if streaming || end > size {
				break
			}
		}
		if end > size {
			break
		}
		off = end
		if chunkSize&1 == 1 {
			off++
		}
		if off <= body {
			return nil, fmt.Errorf("%w: non-advancing chunk", ErrMalformed)
		}
	}
	if len(fmtBytes) == 0 {
		return nil, fmt.Errorf("%w: missing fmt chunk", ErrMalformed)
	}
	if dataOffset < 0 {
		return nil, fmt.Errorf("%w: missing data chunk", ErrMalformed)
	}
	parsed, err := parseFormat(fmtBytes)
	if err != nil {
		return nil, err
	}
	parsed.DataBytes = uint64(dataBytes)
	if parsed.BlockAlign > 0 {
		parsed.DataBytes -= parsed.DataBytes % uint64(parsed.BlockAlign)
	}
	if _, err := f.Seek(dataOffset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("%w: seek data: %v", ErrMalformed, err)
	}
	return &Decoder{file: f, format: parsed, dataOffset: dataOffset, remaining: int64(parsed.DataBytes)}, nil
}

func parseFormat(b []byte) (Format, error) {
	if len(b) < 16 {
		return Format{}, fmt.Errorf("%w: short fmt chunk", ErrMalformed)
	}
	rawTag := binary.LittleEndian.Uint16(b[0:2])
	channels := int(binary.LittleEndian.Uint16(b[2:4]))
	rate := int(binary.LittleEndian.Uint32(b[4:8]))
	byteRate := int(binary.LittleEndian.Uint32(b[8:12]))
	blockAlign := int(binary.LittleEndian.Uint16(b[12:14]))
	bits := int(binary.LittleEndian.Uint16(b[14:16]))
	f := Format{
		SampleRate: rate, Channels: channels, BitsPerSample: bits,
		BlockAlign: blockAlign, ByteRate: byteRate, Tag: rawTag, RawTag: rawTag,
	}
	if rawTag == formatExtensible {
		if len(b) < 40 || binary.LittleEndian.Uint16(b[16:18]) < 22 {
			return Format{}, fmt.Errorf("%w: short extensible fmt chunk", ErrMalformed)
		}
		f.Extensible = true
		f.ChannelMask = binary.LittleEndian.Uint32(b[20:24])
		f.Tag = binary.LittleEndian.Uint16(b[24:26])
		guid := []byte{byte(f.Tag), byte(f.Tag >> 8), 0x00, 0x00, 0x00, 0x00, 0x10, 0x00,
			0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}
		guidOK := string(b[24:40]) == string(guid)
		if !guidOK {
			return Format{}, fmt.Errorf("%w: unknown extensible subformat", ErrUnsupportedFormat)
		}
	}
	if channels <= 0 || rate <= 0 || blockAlign <= 0 {
		return Format{}, fmt.Errorf("%w: invalid channel/rate/alignment", ErrMalformed)
	}
	if f.Tag != formatPCM && f.Tag != formatFloat {
		return Format{}, fmt.Errorf("%w: format tag 0x%04x", ErrUnsupportedFormat, f.Tag)
	}
	if f.Tag == formatPCM {
		switch bits {
		case 8, 16, 24, 32:
		default:
			return Format{}, fmt.Errorf("%w: PCM depth %d", ErrUnsupportedFormat, bits)
		}
	} else {
		switch bits {
		case 32, 64:
		default:
			return Format{}, fmt.Errorf("%w: float depth %d", ErrUnsupportedFormat, bits)
		}
	}
	if want := channels * f.BytesPerSample(); blockAlign != want {
		return Format{}, fmt.Errorf("%w: block align %d, want %d", ErrMalformed, blockAlign, want)
	}
	return f, nil
}

// ChannelGains returns BS.1770 channel weights for the supported layouts.
func (f Format) ChannelGains() ([]float64, error) {
	switch f.Channels {
	case 1:
		if f.Extensible && f.ChannelMask != 0x1 && f.ChannelMask != 0x4 {
			return nil, fmt.Errorf("%w: mono mask 0x%x", ErrUnsupportedLayout, f.ChannelMask)
		}
		return []float64{1}, nil
	case 2:
		if f.Extensible && f.ChannelMask != 0 && f.ChannelMask != 0x3 {
			return nil, fmt.Errorf("%w: stereo mask 0x%x", ErrUnsupportedLayout, f.ChannelMask)
		}
		return []float64{1, 1}, nil
	case 6:
		if f.ChannelMask != 0 && f.ChannelMask != 0x3f && f.ChannelMask != 0x60f {
			return nil, fmt.Errorf("%w: 5.1 mask 0x%x", ErrUnsupportedLayout, f.ChannelMask)
		}
		// FL FR FC LFE BL/BR or SL/SR.
		return []float64{1, 1, 1, 0, 1.41, 1.41}, nil
	default:
		return nil, fmt.Errorf("%w: %d channels", ErrUnsupportedLayout, f.Channels)
	}
}

// Format returns a copy of the container description.
func (d *Decoder) Format() Format { return d.format }

// NaNs reports non-finite float samples replaced with zero during decoding.
func (d *Decoder) NaNs() uint64 { return d.nans }

// ReadFrames reads at most maxFrames into dst (interleaved). It returns
// io.EOF after the final frame.
func (d *Decoder) ReadFrames(dst []float64, maxFrames int) (int, error) {
	if maxFrames <= 0 {
		return 0, fmt.Errorf("read frame limit must be positive")
	}
	if d.remaining <= 0 {
		return 0, io.EOF
	}
	frameBytes := d.format.BlockAlign
	wantFrames := maxFrames
	if int64(wantFrames*frameBytes) > d.remaining {
		wantFrames = int(d.remaining / int64(frameBytes))
	}
	if wantFrames == 0 {
		return 0, io.EOF
	}
	wantBytes := wantFrames * frameBytes
	if cap(d.raw) < wantBytes {
		d.raw = make([]byte, wantBytes)
	}
	buf := d.raw[:wantBytes]
	n, err := io.ReadFull(d.file, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return 0, err
	}
	frames := n / frameBytes
	if frames == 0 {
		d.remaining = 0
		return 0, io.EOF
	}
	buf = buf[:frames*frameBytes]
	nanCount, err := ConvertSamples(buf, d.format, dst[:frames*d.format.Channels])
	if err != nil {
		return 0, err
	}
	d.nans += uint64(nanCount)
	d.remaining -= int64(frames * frameBytes)
	if d.remaining <= 0 {
		return frames, io.EOF
	}
	return frames, nil
}

// DurationSeconds returns the sample duration represented by data bytes.
func (d *Decoder) DurationSeconds() float64 {
	return float64(d.format.DataBytes) / float64(d.format.ByteRate)
}

func (d *Decoder) Close() error { return d.file.Close() }
