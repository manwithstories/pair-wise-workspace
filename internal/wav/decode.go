// Package wav parses RIFF/WAVE containers and emits aligned PCM sample blocks.
// Truncated headers and half frames produce position-bearing MediaError values
// instead of being silently discarded, and every size field is validated
// before allocation so that a malformed file cannot trigger large allocations.
package wav

import (
	"bytes"
	"encoding/binary"
	"io"

	"mediakernel/internal/clock"
	"mediakernel/internal/model"
)

// Supported bit depths.
const (
	BitsPCM16 = 16
)

// Format describes the PCM layout of an opened WAV stream.
type Format struct {
	SampleRate    uint32
	Channels      uint16
	BitsPerSample uint16
	// DataBytes is the size advertised by the data chunk.
	DataBytes uint32
}

// BlockAlign returns the byte size of one interleaved frame.
func (f Format) BlockAlign() int { return int(f.Channels) * int(f.BitsPerSample) / 8 }

// FrameCount returns the number of whole frames advertised by the data chunk.
func (f Format) FrameCount() int { return int(f.DataBytes) / f.BlockAlign() }

// MaxWAVBytes bounds how much header/aux data the parser will accept before
// declaring the input malformed. PCM payload itself is streamed in blocks.
const MaxWAVBytes = 64 << 20

// Decoder streams PCM blocks out of a WAV reader.
type Decoder struct {
	r      io.Reader
	Format Format
	// remaining is the number of data bytes advertised but not yet read.
	remaining uint32
	hdrDone   bool
}

type riffChunk struct {
	ID   [4]byte
	Size uint32
}

// dataID marks the chunk whose body is streamed rather than buffered whole.
var dataID = [4]byte{'d', 'a', 't', 'a'}

// NewDecoder reads and validates the WAVE header (up to the data chunk) and
// returns a streaming decoder. Half frames at EOF are reported as
// CatTruncatedInput errors by NextBlock.
func NewDecoder(r io.Reader) (*Decoder, error) {
	d := &Decoder{r: r}
	if err := d.readHeader(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *Decoder) readHeader() error {
	const hdrSize = 12
	riff := make([]byte, hdrSize)
	if _, err := io.ReadFull(d.r, riff); err != nil {
		return truncated("RIFF header", 0, err)
	}
	if !bytes.Equal(riff[0:4], []byte("RIFF")) || !bytes.Equal(riff[8:12], []byte("WAVE")) {
		return &model.MediaError{
			Category: model.CatAlignment,
			Detail:   "not a RIFF/WAVE container",
			Decision: model.DecRejected,
		}
	}
	var sawFmt, sawData bool
	for !sawData {
		var c riffChunk
		if err := binary.Read(d.r, binary.LittleEndian, &c); err != nil {
			return truncated("chunk header", 12, err)
		}
		if c.Size > MaxWAVBytes {
			return &model.MediaError{
				Category: model.CatAlignment,
				Detail:   "chunk length exceeds safety bound",
				Decision: model.DecRejected,
			}
		}
		body := make([]byte, c.Size)
		if c.ID != dataID {
			if _, err := io.ReadFull(d.r, body); err != nil {
				return truncated(string(c.ID[:])+" chunk body", 0, err)
			}
		}
		// Chunks are word-aligned; consume the padding byte when present.
		if c.Size%2 == 1 {
			if _, err := d.r.Read(make([]byte, 1)); err != nil && err != io.EOF {
				return truncated("chunk padding", 0, err)
			}
		}
		switch string(c.ID[:]) {
		case "fmt ":
			if err := d.parseFmt(body); err != nil {
				return err
			}
			sawFmt = true
		case "data":
			if !sawFmt {
				return &model.MediaError{
					Category: model.CatAlignment,
					Detail:   "data chunk appears before fmt chunk",
					Decision: model.DecRejected,
				}
			}
			d.Format.DataBytes = c.Size
			d.remaining = c.Size
			sawData = true
		}
	}
	d.hdrDone = true
	return nil
}

func (d *Decoder) parseFmt(body []byte) error {
	if len(body) < 16 {
		return &model.MediaError{
			Category: model.CatTruncatedInput,
			Detail:   "fmt chunk shorter than 16 bytes",
			Decision: model.DecRejected,
		}
	}
	formatTag := binary.LittleEndian.Uint16(body[0:2])
	channels := binary.LittleEndian.Uint16(body[2:4])
	sampleRate := binary.LittleEndian.Uint32(body[4:8])
	bits := binary.LittleEndian.Uint16(body[14:16])
	if formatTag != 1 {
		return &model.MediaError{
			Category: model.CatAlignment,
			Detail:   "only uncompressed PCM (format tag 1) is supported",
			Decision: model.DecRejected,
		}
	}
	if channels == 0 || channels > 8 {
		return &model.MediaError{
			Category: model.CatAlignment,
			Detail:   "channel count out of supported range 1..8",
			Decision: model.DecRejected,
		}
	}
	if sampleRate == 0 || sampleRate > 384000 {
		return &model.MediaError{
			Category: model.CatAlignment,
			Detail:   "sample rate out of supported range",
			Decision: model.DecRejected,
		}
	}
	if bits != BitsPCM16 {
		return &model.MediaError{
			Category: model.CatAlignment,
			Detail:   "only 16-bit PCM is supported",
			Decision: model.DecRejected,
		}
	}
	d.Format.Channels = channels
	d.Format.SampleRate = sampleRate
	d.Format.BitsPerSample = bits
	return nil
}

// NextBlock reads exactly blockFrames interleaved frames. A short final read
// is an error: the caller always gets whole, block-aligned data.
func (d *Decoder) NextBlock(blockFrames int) ([]byte, error) {
	if !d.hdrDone {
		return nil, &model.MediaError{Category: model.CatAlignment, Detail: "decoder not initialized", Decision: model.DecRejected}
	}
	if blockFrames <= 0 {
		return nil, &model.MediaError{Category: model.CatAlignment, Detail: "block frame count must be positive", Decision: model.DecRejected}
	}
	align := d.Format.BlockAlign()
	want := blockFrames * align
	if int(d.remaining) < want {
		want = int(d.remaining)
	}
	if want == 0 {
		return nil, io.EOF
	}
	buf := make([]byte, want)
	n, err := io.ReadFull(d.r, buf)
	d.remaining -= uint32(n)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		if n%align != 0 {
			return buf[:n], &model.MediaError{
				Category: model.CatTruncatedInput,
				Detail:   "half frame at end of PCM data",
				Decision: model.DecRejected,
			}
		}
		if n == 0 {
			return nil, io.EOF
		}
		return buf[:n], nil
	}
	if err != nil {
		return buf[:n], truncated("PCM payload", 0, err)
	}
	if d.remaining == 0 && n%align != 0 {
		return buf, &model.MediaError{
			Category: model.CatTruncatedInput,
			Detail:   "half frame at end of PCM data",
			Decision: model.DecRejected,
		}
	}
	return buf, nil
}

func truncated(where string, offset int, cause error) error {
	detail := "truncated " + where
	if offset > 0 {
		detail += " near byte offset"
	}
	if cause != nil {
		detail += ": " + cause.Error()
	}
	return &model.MediaError{
		Category: model.CatTruncatedInput,
		At:       clock.Ticks(offset),
		Detail:   detail,
		Decision: model.DecRejected,
	}
}
