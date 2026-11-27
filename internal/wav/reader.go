// Package wav splits RIFF/WAVE PCM input into fixed-size, timestamped sample
// blocks. github.com/go-audio/wav parses the container; this package reads the
// raw PCM payload itself so that truncation at the data chunk is detected to
// the byte. Only PCM 16-bit and 24-bit integer data is accepted.
package wav

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/go-audio/audio"
	"github.com/go-audio/wav"

	"avpipe/internal/media"
)

// Limits bounds the memory a single input may touch.
type Limits struct {
	MaxBlockFrames int
	MaxTotalBytes  int64
	MaxTracks      int
}

// DefaultLimits are the demo defaults: 4096 frames per block, 64 MiB of PCM,
// and eight tracks.
func DefaultLimits() Limits {
	return Limits{MaxBlockFrames: 4096, MaxTotalBytes: 64 << 20, MaxTracks: 8}
}

// Info describes a parsed WAV header plus the bytes actually present.
type Info struct {
	Rate       media.Rate
	Channels   int
	BitDepth   int
	Frames     int64
	DataLen    int64
	Available  int64
	Truncated  int64
	FrameBytes int
}

// Reader streams PCM frames from one WAV input.
type Reader struct {
	info   Info
	limits Limits
	file   *os.File
	dec    *wav.Decoder
	src    io.Reader
	read   int64 // bytes consumed from the data chunk
	block  uint64
	tail   bool
}

// Open validates the RIFF header and returns a reader positioned at the first
// sample frame. The caller owns Close.
func Open(path string, limits Limits) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, media.NewError(media.PhaseIO, media.ExitInternal, path, 0, false, "open", err)
	}
	r, err := NewReader(f, limits)
	if err != nil {
		f.Close()
		return nil, err
	}
	r.file = f
	return r, nil
}

// NewReader wraps an already-open stream.
func NewReader(rs io.ReadSeeker, limits Limits) (*Reader, error) {
	dec := wav.NewDecoder(rs)
	dec.ReadInfo()
	if err := dec.Err(); err != nil {
		return nil, media.NewError(media.PhaseInput, media.ExitInput, "", 0, false, "header", err)
	}
	if dec.NumChans < 1 {
		return nil, media.NewError(media.PhaseInput, media.ExitInput, "", 0, false, "header",
			fmt.Errorf("no channels"))
	}
	if dec.WavAudioFormat != 1 {
		return nil, media.NewError(media.PhaseInput, media.ExitInput, "", 0, false, "header",
			fmt.Errorf("unsupported WAV format %d; PCM only", dec.WavAudioFormat))
	}
	if dec.BitDepth != 16 && dec.BitDepth != 24 {
		return nil, media.NewError(media.PhaseInput, media.ExitInput, "", 0, false, "header",
			fmt.Errorf("unsupported bit depth %d; 16 or 24 only", dec.BitDepth))
	}
	if limits.MaxTracks > 0 && int(dec.NumChans) > limits.MaxTracks {
		return nil, media.NewError(media.PhaseInput, media.ExitInput, "", 0, false, "header",
			fmt.Errorf("channel count %d exceeds track limit %d", dec.NumChans, limits.MaxTracks))
	}
	rate := media.Rate{Num: int64(dec.SampleRate), Den: 1}
	if !rate.Valid() {
		return nil, media.NewError(media.PhaseInput, media.ExitInput, "", 0, false, "header",
			fmt.Errorf("invalid sample rate %d", dec.SampleRate))
	}
	if err := dec.FwdToPCM(); err != nil {
		return nil, media.NewError(media.PhaseInput, media.ExitInput, "", 0, false, "header",
			fmt.Errorf("no PCM data chunk: %w", err))
	}
	declared := dec.PCMLen()
	frameBytes := int64(dec.NumChans) * int64(dec.BitDepth) / 8
	if declared < frameBytes {
		return nil, media.NewError(media.PhaseInput, media.ExitInput, "", 0, false, "header",
			fmt.Errorf("PCM data %d bytes shorter than one frame (%d)", declared, frameBytes))
	}
	if limits.MaxTotalBytes > 0 && declared > limits.MaxTotalBytes {
		return nil, media.NewError(media.PhaseInput, media.ExitInput, "", 0, false, "header",
			fmt.Errorf("PCM bytes %d exceed limit %d", declared, limits.MaxTotalBytes))
	}
	// Measure the bytes actually present so a truncated tail is known before
	// any sample is decoded, not discovered as an ambiguous short read.
	start, err := rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, media.NewError(media.PhaseIO, media.ExitInternal, "", 0, false, "seek", err)
	}
	end, err := rs.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, media.NewError(media.PhaseIO, media.ExitInternal, "", 0, false, "seek", err)
	}
	if _, err := rs.Seek(start, io.SeekStart); err != nil {
		return nil, media.NewError(media.PhaseIO, media.ExitInternal, "", 0, false, "seek", err)
	}
	available := end - start
	if available < 0 {
		available = 0
	}
	truncated := int64(0)
	if available < declared {
		truncated = declared - available
	} else {
		available = declared
	}
	r := &Reader{
		limits: limits,
		dec:    dec,
		src:    dec.PCMChunk,
	}
	r.info = Info{
		Rate:       rate,
		Channels:   int(dec.NumChans),
		BitDepth:   int(dec.BitDepth),
		Frames:     available / frameBytes,
		DataLen:    declared,
		Available:  available,
		Truncated:  truncated,
		FrameBytes: int(frameBytes),
	}
	return r, nil
}

// Info returns the parsed header metadata.
func (r *Reader) Info() Info { return r.info }

// Close releases the underlying file, if any.
func (r *Reader) Close() error {
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}

// Chunk is one decoded block: interleaved samples normalized to [-1, 1].
type Chunk struct {
	Block   media.Block
	Partial bool
	RawLen  int64
	// DroppedBytes is the truncated tail reported with the final chunk.
	DroppedBytes int64
	// FragmentBytes is the sub-frame remainder of the available data.
	FragmentBytes int64
}

// ReadBlock decodes up to frames sample frames. A short final block is either
// rejected (strict) or flagged Partial; a truncated data chunk is reported as
// corrupt input in strict mode and as DroppedBytes in recovery mode.
func (r *Reader) ReadBlock(track string, frames int, strict bool) (Chunk, error) {
	if frames <= 0 {
		return Chunk{}, media.NewError(media.PhaseInput, media.ExitInput, track, 0, false, "block",
			fmt.Errorf("block size must be positive"))
	}
	if r.limits.MaxBlockFrames > 0 && frames > r.limits.MaxBlockFrames {
		return Chunk{}, media.NewError(media.PhaseInput, media.ExitInput, track, r.block, false, "block",
			fmt.Errorf("block frames %d exceed limit %d", frames, r.limits.MaxBlockFrames))
	}
	if r.read >= r.info.Available {
		return Chunk{}, io.EOF
	}
	block := media.Block{
		Track:    track,
		Seq:      r.block,
		Start:    r.read / int64(r.info.FrameBytes),
		Rate:     r.info.Rate,
		Channels: r.info.Channels,
	}
	wantBytes := frames * r.info.FrameBytes
	if left := r.info.Available - r.read; int64(wantBytes) > left {
		wantBytes = int(left)
	}
	raw := make([]byte, wantBytes)
	n, err := io.ReadFull(r.src, raw)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return Chunk{}, media.NewError(media.PhaseIO, media.ExitInternal, track, block.Seq, false, "read", err)
	}
	raw = raw[:n]
	r.read += int64(n)
	samples := n / r.info.FrameBytes

	last := r.read >= r.info.Available
	fragment := r.info.Available % int64(r.info.FrameBytes)
	corruptTail := last && (r.info.Truncated > 0 || fragment != 0)

	if corruptTail && strict {
		return Chunk{}, media.NewError(media.PhaseInput, media.ExitInput, track, block.Seq, true, "read",
			fmt.Errorf("%w: truncated tail of %d bytes", media.ErrCorruptInput, r.droppedTail()))
	}
	if samples == 0 && last {
		return Chunk{}, io.EOF
	}
	block.Samples = make([]float64, samples*r.info.Channels)
	decodePCM(raw, r.info.BitDepth, block.Samples)
	r.block++

	chunk := Chunk{Block: block, RawLen: int64(n)}
	if corruptTail {
		chunk.Partial = true
		chunk.DroppedBytes = r.droppedTail()
		chunk.FragmentBytes = fragment
	}
	return chunk, nil
}

// droppedTail reports the bytes of the data chunk that were declared but are
// not decodable: the physically missing tail plus any sub-frame fragment.
func (r *Reader) droppedTail() int64 {
	return r.info.Truncated
}

// EOF reports whether every available sample frame has been read.
func (r *Reader) EOF() bool { return r.read >= r.info.Available }

// DroppedBytes returns the truncation size of the data chunk.
func (r *Reader) DroppedBytes() int64 { return r.info.Truncated }

func decodePCM(raw []byte, bitDepth int, dst []float64) {
	switch bitDepth {
	case 24:
		const scale = 8388608
		for i := 0; i < len(dst); i++ {
			b := raw[i*3 : i*3+3]
			v := int32(b[0]) | int32(b[1])<<8 | int32(b[2])<<16
			if v&0x800000 != 0 {
				v |= ^0xffffff
			}
			dst[i] = float64(v) / scale
		}
	default:
		const scale = 32768
		for i := 0; i < len(dst); i++ {
			v := int16(uint16(raw[i*2]) | uint16(raw[i*2+1])<<8)
			dst[i] = float64(v) / scale
		}
	}
}

// WritePCM16 writes normalized samples as a 16-bit PCM WAV.
func WritePCM16(w io.WriteSeeker, rate media.Rate, channels int, samples []float64) error {
	if !rate.Valid() || channels <= 0 {
		return media.NewError(media.PhaseConfig, media.ExitUsage, "", 0, false, "write",
			fmt.Errorf("invalid output format rate=%s channels=%d", rate.String(), channels))
	}
	enc := wav.NewEncoder(w, int(rate.Float()), 16, channels, 1)
	buf := &audio.IntBuffer{
		Data:   make([]int, len(samples)),
		Format: &audio.Format{NumChannels: channels, SampleRate: int(rate.Float())},
	}
	for i, s := range samples {
		if s > 1 {
			s = 1
		}
		if s < -1 {
			s = -1
		}
		buf.Data[i] = clampPCM16(s)
	}
	if err := enc.Write(buf); err != nil {
		return media.NewError(media.PhaseIO, media.ExitInternal, "", 0, false, "encode", err)
	}
	if err := enc.Close(); err != nil {
		return media.NewError(media.PhaseIO, media.ExitInternal, "", 0, false, "close", err)
	}
	return nil
}

func clampPCM16(s float64) int {
	v := int(s * 32767)
	if v > 32767 {
		v = 32767
	}
	if v < -32768 {
		v = -32768
	}
	return v
}

// EncodePCM16Bytes renders normalized samples as 16-bit PCM WAV into memory.
func EncodePCM16Bytes(rate media.Rate, channels int, samples []float64) ([]byte, error) {
	bs := &memWriteSeeker{}
	if err := WritePCM16(bs, rate, channels, samples); err != nil {
		return nil, err
	}
	return bs.buf, nil
}

type memWriteSeeker struct {
	buf []byte
	pos int64
}

func (m *memWriteSeeker) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if end > int64(len(m.buf)) {
		m.buf = append(m.buf, make([]byte, end-int64(len(m.buf)))...)
	}
	copy(m.buf[m.pos:], p)
	m.pos = end
	return len(p), nil
}

func (m *memWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = m.pos + offset
	case io.SeekEnd:
		abs = int64(len(m.buf)) + offset
	default:
		return 0, fmt.Errorf("invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("negative seek")
	}
	if abs > int64(len(m.buf)) {
		m.buf = append(m.buf, make([]byte, abs-int64(len(m.buf)))...)
	}
	m.pos = abs
	return abs, nil
}
