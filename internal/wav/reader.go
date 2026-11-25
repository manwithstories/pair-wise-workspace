// Package wav parses PCM RIFF/RF64 (WAVE) files, validates their chunk
// structure and frame alignment, and verifies per-32-frame CRC32C blocks.
package wav

import (
	"errors"
	"io"
	"os"

	"mediaaudit/internal/model"
)

// Limits guard against malformed length fields causing huge allocations.
const (
	MaxChannels = 32
	MaxBits     = 32
	// MaxDataBytes caps a single WAV data payload the analyzer will accept.
	MaxDataBytes = 8 << 30 // 8 GiB
)

// Chunk describes one RIFF sub-chunk located inside the WAVE container.
type Chunk struct {
	ID     string
	Offset int64 // absolute source byte offset of the chunk payload
	Size   int64 // declared payload size (padded size is Size+Size%2)
}

// Header carries the decoded PCM parameters needed to address frames.
type Header struct {
	SampleRate uint32
	Channels   uint16
	Bits       uint16
	// DataChunk locates the PCM payload in the source file.
	DataChunk Chunk
	// FrameBytes is Channels * Bits/8; FrameCount is Data.Size/FrameBytes.
	FrameBytes int64
	FrameCount int64
}

// Rate returns the audio sample rate as a model time base.
func (h Header) Rate() model.Rational { return model.Hz(int64(h.SampleRate)) }

// Reader performs a single streaming pass and records every sub-chunk. It
// never buffers the whole PCM payload; the caller seeks to DataChunk.
type Reader struct {
	r        io.ReadSeeker
	fileSize int64
	rf64     bool
	hdr      Header
	chunks   []Chunk
	// ds64 real sizes, populated only for RF64.
	ds64DataSize uint32
	// tailFault records a recoverable short-tail truncation instead of
	// failing the whole track; the intact prefix remains fully usable.
	tailFault *model.Fault
}

// NewReader opens path and parses the container header (not the PCM payload).
func NewReader(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	rd := &Reader{r: f, fileSize: st.Size()}
	if err := rd.parseContainer(); err != nil {
		f.Close()
		return nil, err
	}
	return rd, nil
}

// Close releases the underlying file.
func (rd *Reader) Close() error {
	if c, ok := rd.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// Header returns the decoded PCM header.
func (rd *Reader) Header() Header { return rd.hdr }

// Chunks returns all located sub-chunks in file order.
func (rd *Reader) Chunks() []Chunk { return rd.chunks }


func (rd *Reader) skipChunk(c Chunk) error {
	return rd.skipTo(c.Offset + c.Size + c.Size%2)
}

func (rd *Reader) skipTo(abs int64) error {
	_, err := rd.r.Seek(abs, io.SeekStart)
	return err
}

// SeekData positions the underlying reader at the first PCM byte.
func (rd *Reader) SeekData() error {
	_, err := rd.r.Seek(rd.hdr.DataChunk.Offset, io.SeekStart)
	return err
}

// Source returns the underlying seeker (used by the inspector for streaming).
func (rd *Reader) Source() io.ReadSeeker { return rd.r }

func readFull(r io.Reader, b []byte) error {
	_, err := io.ReadFull(r, b)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
