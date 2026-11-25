// Package wav parses PCM RIFF/RF64 (WAVE) files, validates their chunk
// structure and frame alignment, and verifies per-32-frame CRC32C blocks.
package wav

import (
	"encoding/binary"
	"errors"
	"fmt"
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

func (rd *Reader) parseContainer() error {
	var tag [4]byte
	if err := readFull(rd.r, tag[:]); err != nil {
		return model.NewFault(model.KindInvalid, "", model.FrameRange{}, 0,
			"unreadable RIFF tag: "+err.Error())
	}
	switch string(tag[:]) {
	case "RIFF":
	case "RF64":
		rd.rf64 = true
	default:
		return model.NewFault(model.KindInvalid, "", model.FrameRange{}, 0,
			"not a RIFF/RF64 file: "+string(tag[:]))
	}
	var riffSize uint32
	if err := binary.Read(rd.r, binary.LittleEndian, &riffSize); err != nil {
		return err
	}
	var wave [4]byte
	if err := readFull(rd.r, wave[:]); err != nil {
		return err
	}
	if string(wave[:]) != "WAVE" {
		return model.NewFault(model.KindInvalid, "", model.FrameRange{}, 8,
			"missing WAVE form type")
	}
	// RF64 carries a ds64 chunk before all others with the 64-bit sizes.
	if rd.rf64 {
		if err := rd.readDS64(riffSize); err != nil {
			return err
		}
	}
	return rd.scanChunks(riffSize)
}

func (rd *Reader) readDS64(riffSize uint32) error {
	c, payload, err := rd.readChunkHeader()
	if err != nil {
		return err
	}
	if c.ID != "ds64" {
		return model.NewFault(model.KindInvalid, "", model.FrameRange{}, c.Offset,
			"RF64 requires a leading ds64 chunk")
	}
	if payload < 28 {
		return model.NewFault(model.KindInvalid, "", model.FrameRange{}, c.Offset,
			"ds64 chunk too short")
	}
	buf := make([]byte, 28)
	if _, err := io.ReadFull(rd.r, buf); err != nil {
		return err
	}
	// RIFF size (8), data size (8), sample count (8), table count (4).
	rd.ds64DataSize = uint32(binary.LittleEndian.Uint64(buf[8:16]))
	// Skip any remaining ds64 payload plus padding.
	return rd.skipChunk(c)
}

// readChunkHeader reads id+size and returns the chunk with its payload offset.
func (rd *Reader) readChunkHeader() (Chunk, int64, error) {
	var head [8]byte
	off, err := rd.r.Seek(0, io.SeekCurrent)
	if err != nil {
		return Chunk{}, 0, err
	}
	if err := readFull(rd.r, head[:]); err != nil {
		return Chunk{}, 0, err
	}
	id := string(head[:4])
	size := int64(binary.LittleEndian.Uint32(head[4:]))
	payloadOff := off + 8
	return Chunk{ID: id, Offset: payloadOff, Size: size}, size, nil
}

func (rd *Reader) scanChunks(riffSize uint32) error {
	var sawFmt, sawData bool
	for {
		c, size, err := rd.readChunkHeader()
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return err
		}
		// RF64: the 0xFFFFFFFF placeholder is replaced by the ds64 value.
		if rd.rf64 && size == 0xFFFFFFFF {
			if c.ID == "data" {
				size = int64(rd.ds64DataSize)
				c.Size = size
			}
		}
		// Validate declared length against remaining file before allocating.
		padded := size + size%2
		if size < 0 || c.Offset+size > rd.fileSize {
			return model.NewFault(model.KindTruncated, "",
				model.FrameRange{}, c.Offset,
				fmt.Sprintf("chunk %q declares %d bytes beyond file end", c.ID, size))
		}
		switch c.ID {
		case "fmt ":
			if err := rd.parseFmt(c, size); err != nil {
				return err
			}
			sawFmt = true
		case "data":
			rd.hdr.DataChunk = c
			sawData = true
		default:
			rd.chunks = append(rd.chunks, c)
		}
		if err := rd.skipTo(c.Offset + padded); err != nil {
			return err
		}
	}
	if !sawFmt {
		return model.NewFault(model.KindInvalid, "", model.FrameRange{}, 0, "missing fmt chunk")
	}
	if !sawData {
		return model.NewFault(model.KindInvalid, "", model.FrameRange{}, 0, "missing data chunk")
	}
	return rd.finalizeData(riffSize)
}

func (rd *Reader) parseFmt(c Chunk, size int64) error {
	if size < 16 {
		return model.NewFault(model.KindInvalid, "", model.FrameRange{}, c.Offset,
			"fmt chunk shorter than PCM header")
	}
	buf := make([]byte, 16)
	if _, err := rd.r.Seek(c.Offset, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.ReadFull(rd.r, buf); err != nil {
		return err
	}
	format := binary.LittleEndian.Uint16(buf[0:2])
	channels := binary.LittleEndian.Uint16(buf[2:4])
	rate := binary.LittleEndian.Uint32(buf[4:8])
	bits := binary.LittleEndian.Uint16(buf[14:16])
	if format != 1 { // PCM only
		return model.NewFault(model.KindInvalid, "", model.FrameRange{}, c.Offset,
			fmt.Sprintf("unsupported format tag %d (PCM=1 only)", format))
	}
	if channels == 0 || channels > MaxChannels {
		return model.NewFault(model.KindInvalid, "", model.FrameRange{}, c.Offset,
			fmt.Sprintf("channel count %d out of range 1..%d", channels, MaxChannels))
	}
	if bits != 16 && bits != 24 && bits != 32 {
		return model.NewFault(model.KindInvalid, "", model.FrameRange{}, c.Offset,
			fmt.Sprintf("unsupported bit depth %d (16/24/32 only)", bits))
	}
	if rate == 0 {
		return model.NewFault(model.KindInvalid, "", model.FrameRange{}, c.Offset,
			"zero sample rate")
	}
	rd.hdr.SampleRate = rate
	rd.hdr.Channels = channels
	rd.hdr.Bits = bits
	return nil
}

func (rd *Reader) finalizeData(riffSize uint32) error {
	h := &rd.hdr
	h.FrameBytes = int64(h.Channels) * int64(h.Bits) / 8
	dataSize := h.DataChunk.Size
	if dataSize > MaxDataBytes {
		return model.NewFault(model.KindInvalid, "", model.FrameRange{},
			h.DataChunk.Offset, "data payload exceeds configured limit")
	}
	if h.FrameBytes <= 0 {
		return model.NewFault(model.KindInvalid, "", model.FrameRange{},
			h.DataChunk.Offset, "non-positive frame size")
	}
	if dataSize%h.FrameBytes != 0 {
		// Frame misalignment: isolate the trailing residual rather than fail
		// the whole track. Full frames remain usable.
		h.FrameCount = dataSize / h.FrameBytes
		return model.NewFault(model.KindAlignment, "",
			model.FrameRange{First: model.Frame(h.FrameCount), Last: model.Frame(h.FrameCount + 1)},
			h.DataChunk.Offset+h.FrameCount*h.FrameBytes,
			fmt.Sprintf("data size %d not frame aligned (%d bytes/frame)", dataSize, h.FrameBytes))
	}
	h.FrameCount = dataSize / h.FrameBytes
	return nil
}

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
