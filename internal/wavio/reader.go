package wavio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

const (
	FormatPCM        = 1
	FormatFloat      = 3
	FormatExtensible = 0xFFFE
)

type Header struct {
	SampleRate    uint32
	Channels      uint16
	BitsPerSample uint16
	FormatTag     uint16
	ChannelMask   uint32
	DataOffset    int64
	DataLength    int64
}

type TruncatedDataError struct {
	DataOffset  int64
	Declared    int64
	ActuallyGot int64
}

func (e *TruncatedDataError) Error() string {
	return fmt.Sprintf("data chunk truncated: payload byte offset %d, declared %d bytes, actually got %d bytes",
		e.DataOffset, e.Declared, e.ActuallyGot)
}

func IsTruncated(err error) bool {
	var target *TruncatedDataError
	return errors.As(err, &target)
}

type Reader struct {
	r         io.Reader
	pos       int64
	hdr       Header
	remaining int64
	got       int64
	dataOff   int64
	declared  int64
}

func NewReader(r io.Reader) (*Reader, error) {
	reader := &Reader{r: r}
	if err := reader.parse(); err != nil {
		return nil, err
	}
	return reader, nil
}

func (r *Reader) Header() Header { return r.hdr }

func (r *Reader) FrameSize() int {
	return int(r.hdr.BitsPerSample) / 8 * int(r.hdr.Channels)
}

func (r *Reader) read(p []byte) error {
	n, err := io.ReadFull(r.r, p)
	r.pos += int64(n)
	return err
}

func (r *Reader) parse() error {
	var head [12]byte
	if err := r.read(head[:]); err != nil {
		return fmt.Errorf("invalid WAV: %w", err)
	}
	if string(head[0:4]) != "RIFF" || string(head[8:12]) != "WAVE" {
		return errors.New("not a RIFF/WAVE file")
	}
	for {
		var chunkHead [8]byte
		if err := r.read(chunkHead[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return errors.New("WAV missing data chunk")
			}
			return err
		}
		id := string(chunkHead[0:4])
		size := int64(binary.LittleEndian.Uint32(chunkHead[4:8]))
		bodyStart := r.pos
		switch id {
		case "fmt ":
			body := make([]byte, size)
			if err := r.read(body); err != nil {
				return err
			}
			if err := r.parseFmt(body); err != nil {
				return err
			}
		case "data":
			r.dataOff = bodyStart
			r.declared = size
			r.remaining = size
			r.hdr.DataOffset = bodyStart
			r.hdr.DataLength = size
			return nil
		default:
			skip := size + size%2
			if _, err := io.CopyN(io.Discard, r.r, skip); err != nil {
				return err
			}
			r.pos += skip
		}
	}
}

func (r *Reader) parseFmt(body []byte) error {
	if len(body) < 16 {
		return errors.New("fmt chunk too small")
	}
	tag := binary.LittleEndian.Uint16(body[0:2])
	channels := binary.LittleEndian.Uint16(body[2:4])
	sampleRate := binary.LittleEndian.Uint32(body[4:8])
	bits := binary.LittleEndian.Uint16(body[14:16])
	mask := uint32(0)
	if tag == FormatExtensible && len(body) >= 40 {
		mask = binary.LittleEndian.Uint32(body[20:24])
		tag = binary.LittleEndian.Uint16(body[24:26])
	}
	if channels == 0 || sampleRate == 0 {
		return errors.New("invalid fmt chunk: zero channels or sample rate")
	}
	switch tag {
	case FormatPCM:
		if bits != 16 && bits != 24 && bits != 32 {
			return fmt.Errorf("unsupported PCM bit depth %d", bits)
		}
	case FormatFloat:
		if bits != 32 && bits != 64 {
			return fmt.Errorf("unsupported float bit depth %d", bits)
		}
	default:
		return fmt.Errorf("unsupported WAV format tag %d", tag)
	}
	r.hdr.FormatTag = tag
	r.hdr.Channels = channels
	r.hdr.SampleRate = sampleRate
	r.hdr.BitsPerSample = bits
	r.hdr.ChannelMask = mask
	return nil
}

func (r *Reader) ReadFrames(maxFrames int) ([][]float64, error) {
	if r.remaining <= 0 {
		return nil, io.EOF
	}
	channels := int(r.hdr.Channels)
	frameBytes := r.FrameSize()
	frameCount := maxFrames
	if int64(frameCount*frameBytes) > r.remaining {
		frameCount = int(r.remaining / int64(frameBytes))
	}
	if frameCount == 0 {
		return nil, &TruncatedDataError{r.dataOff, r.declared, r.got}
	}
	buf := make([]byte, frameCount*frameBytes)
	n, err := io.ReadFull(r.r, buf)
	r.pos += int64(n)
	r.got += int64(n)
	r.remaining -= int64(n)
	available := n / frameBytes
	planar := make([][]float64, channels)
	for ch := range planar {
		planar[ch] = make([]float64, available)
	}
	bytesPerSample := int(r.hdr.BitsPerSample) / 8
	for i := 0; i < available; i++ {
		for ch := 0; ch < channels; ch++ {
			off := (i*channels + ch) * bytesPerSample
			planar[ch][i] = r.decodeSample(buf[off : off+bytesPerSample])
		}
	}
	if err != nil || n%frameBytes != 0 {
		return planar, &TruncatedDataError{r.dataOff, r.declared, r.got}
	}
	return planar, nil
}

func (r *Reader) decodeSample(b []byte) float64 {
	if r.hdr.FormatTag == FormatFloat {
		if r.hdr.BitsPerSample == 32 {
			return float64(math.Float32frombits(binary.LittleEndian.Uint32(b)))
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(b))
	}
	switch r.hdr.BitsPerSample {
	case 16:
		return float64(int16(binary.LittleEndian.Uint16(b))) / 32768.0
	case 24:
		value := int32(b[0]) | int32(b[1])<<8 | int32(b[2])<<16
		if value&0x00800000 != 0 {
			value |= ^0x00FFFFFF
		}
		return float64(value) / 8388608.0
	case 32:
		return float64(int32(binary.LittleEndian.Uint32(b))) / 2147483648.0
	default:
		return 0
	}
}
