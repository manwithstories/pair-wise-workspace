// Package fixture generates deterministic, self-contained Matroska/WebM
// test material: valid streams, out-of-order multi-track blocks,
// unknown-length nested truncations, timestamp wrap, late events and
// slow-consumer scenarios. No external files are ever referenced.
package fixture

import (
	"bytes"
	"encoding/binary"

	"mediapack/internal/ebml"
)

// Matroska element IDs mirrored from the parser package's vocabulary.
const (
	idEBML          uint64 = 0x1A45DFA3
	idDocType       uint64 = 0x4282
	idSegment       uint64 = 0x18538067
	idInfo          uint64 = 0x1549A966
	idTimeScale     uint64 = 0x2AD7B1
	idMuxingApp     uint64 = 0x4D80
	idTracks        uint64 = 0x1654AE6B
	idTrackEntry    uint64 = 0xAE
	idTrackNumber   uint64 = 0xD7
	idTrackType     uint64 = 0x83
	idCodecID       uint64 = 0x86
	idCluster       uint64 = 0x1F43B675
	idTimecode      uint64 = 0xE7
	idSimpleBlock   uint64 = 0xA3
)

// Builder accumulates a Matroska byte stream in memory. Fixtures are
// small by design; the production parser itself remains fully streaming.
type Builder struct {
	buf       bytes.Buffer
	scale     uint64
	tracks    []TrackSpec
	clusters  []clusterSpec
	unknown   bool // unknown-length Segment
	cutTail   int  // drop this many final bytes (truncation)
}

// TrackSpec describes one track entry.
type TrackSpec struct {
	Number  uint64
	Type    byte // 1=video,2=audio,17=subtitle
	Codec   string
}

// BlockSpec is one SimpleBlock placement.
type BlockSpec struct {
	Track      uint64
	RelativeTC int16
	Keyframe   bool
	Payload    []byte
}

type clusterSpec struct {
	timecode uint64
	blocks   []BlockSpec
	unknown  bool
}

// NewBuilder starts a builder with the default 1ms TimestampScale.
func NewBuilder() *Builder {
	return &Builder{scale: 1_000_000}
}

// WithTimestampScale overrides the nanoseconds-per-tick scale.
func (b *Builder) WithTimestampScale(scale uint64) *Builder {
	b.scale = scale
	return b
}

// UnknownSegment marks the Segment element as unknown-length.
func (b *Builder) UnknownSegment() *Builder { b.unknown = true; return b }

// TruncateTail drops the final n bytes of the serialized output.
func (b *Builder) TruncateTail(n int) *Builder { b.cutTail = n; return b }

// AddTrack registers a track.
func (b *Builder) AddTrack(t TrackSpec) *Builder {
	b.tracks = append(b.tracks, t)
	return b
}

// AddCluster appends a cluster at the given absolute cluster timecode
// (in ticks) containing the given blocks in stated order.
func (b *Builder) AddCluster(timecode uint64, blocks ...BlockSpec) *Builder {
	b.clusters = append(b.clusters, clusterSpec{timecode: timecode, blocks: blocks})
	return b
}

// AddUnknownCluster appends an unknown-length cluster (live style).
func (b *Builder) AddUnknownCluster(timecode uint64, blocks ...BlockSpec) *Builder {
	b.clusters = append(b.clusters, clusterSpec{timecode: timecode, blocks: blocks, unknown: true})
	return b
}

func element(id uint64, body []byte) []byte {
	out := append(ebml.EncodeElementID(id), ebml.EncodeVINT(uint64(len(body)))...)
	return append(out, body...)
}

func unknownElement(id uint64, width int, body []byte) []byte {
	out := append(ebml.EncodeElementID(id), ebml.EncodeUnknownSize(width)...)
	return append(out, body...)
}

func uintElement(id uint64, v uint64, width int) []byte {
	body := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		body[i] = byte(v & 0xff)
		v >>= 8
	}
	return element(id, body)
}

func stringElement(id uint64, s string) []byte {
	return element(id, []byte(s))
}

func simpleBlock(bl BlockSpec) []byte {
	var body bytes.Buffer
	body.Write(ebml.EncodeVINT(bl.Track)) // size VINT: marker stripped
	var tc [2]byte
	binary.BigEndian.PutUint16(tc[:], uint16(bl.RelativeTC))
	body.Write(tc[:])
	var flags byte
	if bl.Keyframe {
		flags |= 0x80
	}
	body.WriteByte(flags)
	body.Write(bl.Payload)
	return element(idSimpleBlock, body.Bytes())
}

// Build serializes the whole fixture and returns the raw bytes.
func (b *Builder) Build() []byte {
	var out bytes.Buffer
	// EBML header with WebM doc type.
	ebmlBody := stringElement(idDocType, "webm")
	out.Write(element(idEBML, ebmlBody))

	// Segment content: Info + Tracks + Clusters.
	var segBody bytes.Buffer
	info := append(uintElement(idTimeScale, b.scale, 4),
		stringElement(idMuxingApp, "mediapack-fixture")...)
	segBody.Write(element(idInfo, info))

	var tracks bytes.Buffer
	for _, t := range b.tracks {
		entry := append(uintElement(idTrackNumber, t.Number, 1),
			uintElement(idTrackType, uint64(t.Type), 1)...)
		entry = append(entry, stringElement(idCodecID, t.Codec)...)
		tracks.Write(element(idTrackEntry, entry))
	}
	segBody.Write(element(idTracks, tracks.Bytes()))

	for _, c := range b.clusters {
		var cb bytes.Buffer
		cb.Write(uintElement(idTimecode, c.timecode, 4))
		for _, bl := range c.blocks {
			cb.Write(simpleBlock(bl))
		}
		if c.unknown {
			segBody.Write(unknownElement(idCluster, 4, cb.Bytes()))
		} else {
			segBody.Write(element(idCluster, cb.Bytes()))
		}
	}

	if b.unknown {
		out.Write(unknownElement(idSegment, 4, segBody.Bytes()))
	} else {
		out.Write(element(idSegment, segBody.Bytes()))
	}
	raw := out.Bytes()
	if b.cutTail > 0 && b.cutTail < len(raw) {
		raw = raw[:len(raw)-b.cutTail]
	}
	return raw
}
