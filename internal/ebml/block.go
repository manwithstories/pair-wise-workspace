package ebml

import (
	"io"

	"mediapack/internal/clock"
)

// Matroska element identifiers used by the block layer (full EBML ID
// values including the VINT marker bits).
const (
	IDEBMLElement   uint64 = 0x1A45DFA3
	IDSegment       uint64 = 0x18538067
	IDInfo          uint64 = 0x1549A966
	IDTracks        uint64 = 0x1654AE6B
	IDTrackEntry    uint64 = 0xAE
	IDTrackNumber   uint64 = 0xD7
	IDTimestampScale uint64 = 0x2AD7B1
	IDCluster       uint64 = 0x1F43B675
	IDTimecode      uint64 = 0xE7
	IDSimpleBlock   uint64 = 0xA3
	IDBlock         uint64 = 0xA1
	IDBlockGroup    uint64 = 0xA0
)

// Block is a decoded Matroska SimpleBlock/Block header plus a bounded
// reference to its payload. Payload is only retained up to the parser's
// block byte budget; the offsets always point at the raw input.
type Block struct {
	Track        uint64
	TrackWidth   int // on-wire VINT width of the track field
	RelativeTC   int16 // signed 16-bit block timecode relative to cluster
	Flags        byte
	Keyframe     bool
	HeaderOff    int64 // absolute offset of the enclosing element ID
	PayloadOff   int64 // absolute offset of the first payload byte
	PayloadLen   int64
	Payload      []byte // nil when streamed rather than buffered
	Simple       bool
}

// DecodeBlock parses the block header from body (the full element body
// bytes). headerOff is the enclosing element's ID offset and simple
// distinguishes SimpleBlock (flags carry keyframe/lacing) from Block.
func DecodeBlock(body []byte, headerOff int64, simple bool) (*Block, error) {
	// Track VINT, then two-byte signed timecode, then one flags byte.
	if len(body) < 4 {
		return nil, clock.AtError(clock.CatBadBlock, headerOff,
			"block body too short: %d bytes", len(body))
	}
	// Track number is a size-style VINT: the marker bit is not part of
	// the value, so decode with keepMarker=false.
	track, width, _, err := ReadVINT(bytesReader(body), false)
	if err != nil {
		return nil, clock.AtError(clock.CatBadBlock, headerOff, "bad track VINT: %v", err)
	}
	if width > len(body)-3 {
		return nil, clock.AtError(clock.CatBadBlock, headerOff,
			"track VINT width %d overruns block", width)
	}
	rel := int16(uint16(body[width])<<8 | uint16(body[width+1]))
	flags := body[width+2]
	b := &Block{
		Track:      track,
		TrackWidth: width,
		RelativeTC: rel,
		Flags:      flags,
		Keyframe:   simple && flags&0x80 != 0,
		HeaderOff:  headerOff,
		PayloadOff: -1, // caller sets via SetPayloadOff using Element.BodyOff
		PayloadLen: int64(len(body)) - int64(width) - 3,
		Simple:     simple,
	}
	if b.PayloadLen < 0 {
		return nil, clock.AtError(clock.CatBadBlock, headerOff, "block has no payload room")
	}
	b.Payload = body[width+3:]
	return b, nil
}

// SetPayloadOff lets the container walk correct the payload offset once
// the element's BodyOff is known.
func (b *Block) SetPayloadOff(bodyOff int64, _ int) {
	b.PayloadOff = bodyOff + int64(b.TrackWidth) + 3
}

// Lacing reports the lacing bits (0=none,1=Xiph,2=EBML,3=fixed-size).
func (b *Block) Lacing() byte { return (b.Flags >> 1) & 0x03 }

// Discardable reports the discardable flag (SimpleBlock only).
func (b *Block) Discardable() bool { return b.Flags&0x01 != 0 }

type byteReader struct {
	buf []byte
	pos int
}

func bytesReader(b []byte) *byteReader { return &byteReader{buf: b} }

func (r *byteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.buf) {
		return 0, io.EOF
	}
	n := copy(p, r.buf[r.pos:])
	r.pos += n
	return n, nil
}
