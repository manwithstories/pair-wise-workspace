// Package wav implements a streaming RIFF/WAVE reader supporting
// uncompressed PCM integer (8/16/24/32-bit) and IEEE float (32-bit),
// mono through 8 channels. Compressed formats, corrupt chunks and
// inconsistent interleave sizes are rejected.
package wav

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// Format describes the decoded audio stream.
type Format struct {
	SampleRate int
	Channels   int
	BitDepth   int
	Float      bool
	DataBytes  uint64 // payload size of the data chunk
	StartFrame int64  // BWF time reference, 0 when absent
}

// Frames returns the total number of sample frames in the data chunk.
func (f Format) Frames() int64 {
	return int64(f.DataBytes) / int64(f.BlockAlign())
}

// BlockAlign returns bytes per interleaved frame.
func (f Format) BlockAlign() int { return f.Channels * (f.BitDepth / 8) }

// Limits for sanity checks.
const (
	MaxChannels   = 8
	MaxDataBytes  = 1 << 34 // 16 GiB guard against absurd headers
	minSampleRate = 1000
	maxSampleRate = 768000
)

var (
	ErrCompressed   = errors.New("wav: compressed or unsupported encoding (only PCM integer and IEEE float)")
	ErrCorrupt      = errors.New("wav: corrupt RIFF structure")
	ErrTooManyCh    = errors.New("wav: more than 8 channels")
	ErrInterleave   = errors.New("wav: data chunk size is not a multiple of the frame size")
	ErrTooLarge     = errors.New("wav: data chunk exceeds size limit")
	ErrBadRate      = errors.New("wav: unsupported sample rate")
	ErrNonFinite    = errors.New("wav: NaN or Inf sample in float data")
	ErrShortRead    = errors.New("wav: unexpected end of file inside data chunk")
)

// Reader streams decoded float64 samples from a WAV file.
type Reader struct {
	f      *os.File
	format Format
	left   uint64 // bytes remaining in data chunk
	raw    []byte // scratch buffer, reused across ReadFrames calls
}

// Open opens path and parses the RIFF header, positioning at the data chunk.
func Open(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r := &Reader{f: f}
	if err := r.parseHeader(); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// Format returns the parsed stream format.
func (r *Reader) Format() Format { return r.format }

// Close closes the underlying file.
func (r *Reader) Close() error { return r.f.Close() }

func (r *Reader) parseHeader() error {
	var riff [12]byte
	if _, err := io.ReadFull(r.f, riff[:]); err != nil {
		return ErrCorrupt
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return ErrCorrupt
	}
	var haveFmt, haveData bool
	for !haveData {
		var hdr [8]byte
		if _, err := io.ReadFull(r.f, hdr[:]); err != nil {
			return ErrCorrupt
		}
		size := uint64(binary.LittleEndian.Uint32(hdr[4:8]))
		switch string(hdr[0:4]) {
		case "fmt ":
			if err := r.parseFmt(size); err != nil {
				return err
			}
			haveFmt = true
		case "bext":
			r.parseBext(size)
		case "data":
			if !haveFmt {
				return ErrCorrupt
			}
			if size > MaxDataBytes {
				return ErrTooLarge
			}
			if size%uint64(r.format.BlockAlign()) != 0 {
				return ErrInterleave
			}
			if err := r.checkDataFits(size); err != nil {
				return err
			}
			r.format.DataBytes = size
			r.left = size
			haveData = true
		}
		if !haveData {
			// skip chunk payload (bext already consumed) + pad byte
			skip := int64(size)
			if string(hdr[0:4]) == "bext" {
				skip = 0
			}
			if size%2 == 1 {
				skip++
			}
			if _, err := r.f.Seek(skip, io.SeekCurrent); err != nil {
				return ErrCorrupt
			}
		}
	}
	if !haveFmt {
		return ErrCorrupt
	}
	return nil
}

func (r *Reader) parseFmt(size uint64) error {
	if size < 16 {
		return ErrCorrupt
	}
	buf := make([]byte, 16)
	if _, err := io.ReadFull(r.f, buf); err != nil {
		return ErrCorrupt
	}
	enc := binary.LittleEndian.Uint16(buf[0:2])
	ch := int(binary.LittleEndian.Uint16(buf[2:4]))
	rate := int(binary.LittleEndian.Uint32(buf[4:8]))
	align := int(binary.LittleEndian.Uint16(buf[12:14]))
	bits := int(binary.LittleEndian.Uint16(buf[14:16]))
	// skip any extension bytes
	if _, err := r.f.Seek(int64(size)-16+int64(size%2), io.SeekCurrent); err != nil {
		return ErrCorrupt
	}
	var isFloat bool
	switch enc {
	case 1:
		isFloat = false
	case 3:
		isFloat = true
	default:
		return ErrCompressed
	}
	if ch < 1 {
		return ErrCorrupt
	}
	if ch > MaxChannels {
		return ErrTooManyCh
	}
	if rate < minSampleRate || rate > maxSampleRate {
		return ErrBadRate
	}
	if isFloat {
		if bits != 32 {
			return ErrCompressed
		}
	} else if bits != 8 && bits != 16 && bits != 24 && bits != 32 {
		return ErrCompressed
	}
	if align != ch*(bits/8) {
		return ErrInterleave
	}
	r.format = Format{SampleRate: rate, Channels: ch, BitDepth: bits, Float: isFloat}
	return nil
}

// parseBext extracts the BWF TimeReference (sample offset since midnight).
func (r *Reader) parseBext(size uint64) {
	const timeRefOff = 256 + 32 + 32 + 10 + 8
	if size < timeRefOff+8 {
		r.f.Seek(int64(size), io.SeekCurrent)
		return
	}
	if _, err := r.f.Seek(timeRefOff, io.SeekCurrent); err != nil {
		return
	}
	var b [8]byte
	if _, err := io.ReadFull(r.f, b[:]); err == nil {
		r.format.StartFrame = int64(binary.LittleEndian.Uint64(b[:]))
	}
	r.f.Seek(int64(size)-timeRefOff-8, io.SeekCurrent)
}

func (r *Reader) checkDataFits(size uint64) error {
	pos, err := r.f.Seek(0, io.SeekCurrent)
	if err != nil {
		return ErrCorrupt
	}
	st, err := r.f.Stat()
	if err != nil {
		return ErrCorrupt
	}
	if uint64(pos)+size > uint64(st.Size()) {
		return ErrCorrupt
	}
	return nil
}

// ReadFrames decodes up to len(dst)/Channels interleaved frames into dst,
// normalized to [-1, 1]. Returns the number of frames decoded; io.EOF is
// returned only when no frame could be decoded.
func (r *Reader) ReadFrames(dst []float64) (int, error) {
	ch := r.format.Channels
	frames := len(dst) / ch
	if frames == 0 {
		return 0, nil
	}
	if r.left == 0 {
		return 0, io.EOF
	}
	want := uint64(frames * r.format.BlockAlign())
	if want > r.left {
		want = r.left
		frames = int(want) / r.format.BlockAlign()
		want = uint64(frames * r.format.BlockAlign())
	}
	if uint64(cap(r.raw)) < want {
		r.raw = make([]byte, want)
	}
	raw := r.raw[:want]
	if _, err := io.ReadFull(r.f, raw); err != nil {
		return 0, ErrShortRead
	}
	r.left -= want
	if err := r.decode(raw, dst[:frames*ch]); err != nil {
		return 0, err
	}
	return frames, nil
}

func (r *Reader) decode(raw []byte, dst []float64) error {
	f := r.format
	switch {
	case f.Float && f.BitDepth == 32:
		for i := range dst {
			v := float64(math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:])))
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return ErrNonFinite
			}
			dst[i] = v
		}
	case f.BitDepth == 8:
		for i := range dst {
			dst[i] = (float64(raw[i]) - 128) / 128
		}
	case f.BitDepth == 16:
		for i := range dst {
			dst[i] = float64(int16(binary.LittleEndian.Uint16(raw[i*2:]))) / 32768
		}
	case f.BitDepth == 24:
		for i := range dst {
			b := raw[i*3:]
			v := int32(b[0]) | int32(b[1])<<8 | int32(b[2])<<16
			if v&0x800000 != 0 {
				v |= ^0xFFFFFF // sign-extend
			}
			dst[i] = float64(v) / 8388608
		}
	case f.BitDepth == 32:
		for i := range dst {
			dst[i] = float64(int32(binary.LittleEndian.Uint32(raw[i*4:]))) / 2147483648
		}
	}
	return nil
}
