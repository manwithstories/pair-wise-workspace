// Package wav reads archive carrier WAV files (PCM 16/24/32 bit, 1–8
// channels, 8–192 kHz) and turns their payload into fixed-size sample
// blocks annotated with CRC, loss and truncation state. Parsing uses
// github.com/go-audio/wav; an archival side chunk ("rsid") carries the
// restore manifest written by the fixture generator.
package wav

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/go-audio/wav"

	"audiorestore/internal/model"
)

// ManifestID is the custom RIFF chunk id holding restore metadata.
var ManifestID = [4]byte{'r', 's', 'i', 'd'}

const (
	// DefaultBlockFrames is used when a carrier has no rsid manifest.
	DefaultBlockFrames = 256
	manifestMagic      = "RSID"
	manifestVersion    = 1
)

// Format describes the PCM geometry and source labels of a carrier.
type Format struct {
	SampleRate model.SampleRate
	Channels   int
	BitDepth   uint16
	BlockAlign int
	// Layout is the source channel-layout tag (e.g. "stereo_lr"). Empty
	// means the carrier carried no layout; the caller marks degradation.
	Layout string
	// Color is the source color/label metadata tag.
	Color string
}

// Manifest is the archival index embedded beside the PCM payload.
type Manifest struct {
	BlockFrames int
	// Missing lists block indices absent from the carrier, in order.
	Missing []int
	// CRC holds the original CRC32 (IEEE) of every block's PCM bytes.
	CRC []uint32
	Layout string
	Color  string
}

// FrameBlock is one fixed-height slice of the PCM payload. A Missing or
// Truncated block never carries fabricated samples: Data is nil whenever
// the block cannot be presented as intact PCM.
type FrameBlock struct {
	Index      int
	StartFrame int64
	Frames     int
	Data       []byte
	CRC        uint32
	WantCRC    uint32
	Missing    bool
	Corrupt    bool
	Truncated  bool
}

// Reader streams a carrier as frame blocks.
type Reader struct {
	dec      *wav.Decoder
	data     io.Reader
	format   Format
	manifest *Manifest
	pcmBytes int64
	read     int64
}

// Open parses headers and positions the reader on the PCM data chunk.
func Open(rs io.ReadSeeker) (*Reader, error) {
	dec := wav.NewDecoder(rs)
	dec.ReadInfo()
	if err := dec.Err(); err != nil {
		return nil, fmt.Errorf("wav header: %w", err)
	}
	r := &Reader{dec: dec}
	if err := r.collectChunks(); err != nil {
		return nil, err
	}
	if err := r.validate(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Reader) collectChunks() error {
	var foundData bool
	for {
		chunk, err := r.dec.NextChunk()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("wav chunk scan: %w", err)
		}
		switch chunk.ID {
		case ManifestID:
			raw := make([]byte, chunk.Size)
			if _, err := io.ReadFull(chunk, raw); err != nil {
				return fmt.Errorf("rsid chunk: %w", err)
			}
			m, err := parseManifest(raw)
			if err != nil {
				return err
			}
			r.manifest = m
		case [4]byte{'d', 'a', 't', 'a'}:
			r.data = chunk
			r.pcmBytes = int64(chunk.Size)
			foundData = true
		default:
			chunk.Drain()
		}
		if foundData {
			break
		}
	}
	if !foundData {
		return model.TrackedError{Kind: model.ErrNoFrames, Msg: "no data chunk"}
	}
	return nil
}

func (r *Reader) validate() error {
	d := r.dec
	if d.WavAudioFormat != 1 {
		return model.TrackedError{Kind: model.ErrUnsupportedFormat,
			Msg: fmt.Sprintf("only PCM (format 1) supported, got %d", d.WavAudioFormat)}
	}
	if d.NumChans < 1 || d.NumChans > 8 {
		return model.TrackedError{Kind: model.ErrUnsupportedFormat,
			Msg: fmt.Sprintf("channels must be 1..8, got %d", d.NumChans)}
	}
	switch d.BitDepth {
	case 16, 24, 32:
	default:
		return model.TrackedError{Kind: model.ErrUnsupportedFormat,
			Msg: fmt.Sprintf("bit depth must be 16/24/32, got %d", d.BitDepth)}
	}
	sr := model.SampleRate(d.SampleRate)
	if !sr.Valid() {
		return model.TrackedError{Kind: model.ErrUnsupportedFormat,
			Msg: fmt.Sprintf("sample rate %d outside 8000..192000", d.SampleRate)}
	}
	r.format = Format{
		SampleRate: sr,
		Channels:   int(d.NumChans),
		BitDepth:   d.BitDepth,
		BlockAlign: int(d.NumChans) * int(d.BitDepth) / 8,
	}
	if r.manifest != nil {
		r.format.Layout = r.manifest.Layout
		r.format.Color = r.manifest.Color
	}
	return nil
}

// Format returns the validated carrier format and source labels.
func (r *Reader) Format() Format { return r.format }

// Manifest returns the embedded restore manifest, or nil.
func (r *Reader) Manifest() *Manifest { return r.manifest }

// BlockFrames returns the configured block height in sample frames.
func (r *Reader) BlockFrames() int {
	if r.manifest != nil && r.manifest.BlockFrames > 0 {
		return r.manifest.BlockFrames
	}
	return DefaultBlockFrames
}

// TotalFrames returns the number of complete sample frames the declared
// PCM payload contains (a truncated tail's partial frame is excluded).
func (r *Reader) TotalFrames() int64 {
	return r.pcmBytes / int64(r.format.BlockAlign)
}

func parseManifest(raw []byte) (*Manifest, error) {
	if len(raw) < 12 || string(raw[:4]) != manifestMagic {
		return nil, model.TrackedError{Kind: model.ErrUnsupportedFormat, Msg: "bad rsid magic"}
	}
	if ver := binary.LittleEndian.Uint16(raw[4:6]); ver != manifestVersion {
		return nil, model.TrackedError{Kind: model.ErrUnsupportedFormat,
			Msg: fmt.Sprintf("rsid version %d unsupported", ver)}
	}
	m := &Manifest{BlockFrames: int(binary.LittleEndian.Uint32(raw[6:10]))}
	pos := 10
	ll := int(raw[pos])
	pos++
	m.Layout = string(raw[pos : pos+ll])
	pos += ll
	cl := int(raw[pos])
	pos++
	m.Color = string(raw[pos : pos+cl])
	pos += cl
	nMiss := int(binary.LittleEndian.Uint32(raw[pos : pos+4]))
	pos += 4
	m.Missing = make([]int, nMiss)
	for i := range m.Missing {
		m.Missing[i] = int(binary.LittleEndian.Uint32(raw[pos : pos+4]))
		pos += 4
	}
	nCRC := (len(raw) - pos) / 4
	m.CRC = make([]uint32, nCRC)
	for i := range m.CRC {
		m.CRC[i] = binary.LittleEndian.Uint32(raw[pos : pos+4])
		pos += 4
	}
	return m, nil
}
