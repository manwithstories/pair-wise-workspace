// Package fixture generates every sample the pipeline consumes:
// valid/corrupt/recovery Matroska/WebM byte streams, RTP timing captures
// and preview MP4 containers. All samples are deterministic functions of
// a seed and are synthesised in-process; no external media is read.
package fixture

import (
	"encoding/binary"
	"math"
	"math/rand"

	"review/internal/codec"
)

// Matroska element IDs used by the builder (mirrors ebml.ID* constants;
// duplicated here so the generator stays independent of the parser).
const (
	idEBML           uint32 = 0x1A45DFA3
	idDocType        uint32 = 0x4282
	idSegment        uint32 = 0x18538067
	idInfo           uint32 = 0x1549A966
	idTimecodeScale  uint32 = 0x2AD7B1
	idMuxingApp      uint32 = 0x4D80
	idWritingApp     uint32 = 0x5741
	idDuration       uint32 = 0x4489
	idTracks         uint32 = 0x1654AE6B
	idTrackEntry     uint32 = 0xAE
	idTrackNumber    uint32 = 0xD7
	idTrackUID       uint32 = 0x73C5
	idTrackType      uint32 = 0x83
	idCodecID        uint32 = 0x86
	idDefaultDur     uint32 = 0x23E383
	idAudio          uint32 = 0xE1
	idChannels       uint32 = 0x9F
	idSamplingFreq   uint32 = 0xB5
	idBitDepth       uint32 = 0x6264
	idColour         uint32 = 0x55B0
	idMatrixCoeffs   uint32 = 0x55B1
	idTransferChar   uint32 = 0x55BA
	idPrimaries      uint32 = 0x55BB
	idCluster        uint32 = 0x1F43B675
	idTimecode       uint32 = 0xE7
	idSimpleBlock    uint32 = 0xA3
	idBlockGroup     uint32 = 0xA0
	idBlock          uint32 = 0xA1
)

// Track types per Matroska.
const (
	TrackTypeVideo = 1
	TrackTypeAudio = 2
)

// Supported codec identifiers.
const (
	CodecVP8  = "V_VP8"
	CodecVP9  = "V_VP9"
	CodecOpus = "A_OPUS"
	CodecPCM  = "A_PCM/INT/LIT"
)

// TrackSpec describes one generated track.
type TrackSpec struct {
	Number       uint8
	UID          uint32
	Type         uint8
	Codec        string
	Channels     uint32 // audio only; 0 means "absent/unknown layout"
	SampleRate   float64
	BitDepth     uint32
	DefaultDurNS uint64
	// Colour metadata; zero values mean the element is omitted entirely,
	// so missing values are never silently synthesised.
	Matrix   uint64
	Transfer uint64
	Primar   uint64
}

// BlockSpec describes one interleaved block in timeline order.
type BlockSpec struct {
	Track    uint8
	TimeMS   int64 // absolute block time in segment milliseconds
	Keyframe bool
	Payload  []byte
}

// Document is a fully specified Matroska/WebM sample.
type Document struct {
	Seed          int64
	TimecodeScale uint64 // nanoseconds per timecode unit
	DurationMS    float64
	Tracks        []TrackSpec
	Blocks        []BlockSpec
	// UnknownSegment emits the segment with an unknown-size VINT (it must
	// be the document's final master, ending at EOF).
	UnknownSegment bool
	// UnknownCluster emits the last cluster with unknown size.
	UnknownCluster bool
}

func mustElem(id uint32, payload []byte) []byte {
	b, err := codec.EncodeElement(id, payload)
	if err != nil {
		panic(err)
	}
	return b
}

func uintField(id uint32, v uint64) []byte {
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], v)
	i := 0
	for i < 7 && raw[i] == 0 {
		i++
	}
	return mustElem(id, raw[i:])
}

func floatField(id uint32, f float64) []byte {
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], math.Float64bits(f))
	return mustElem(id, raw[:])
}

func stringField(id uint32, s string) []byte {
	return mustElem(id, []byte(s))
}

// Build renders the document to a complete Matroska byte stream.
func (d Document) Build() []byte {
	out := d.buildEBMLHeader()
	out = append(out, d.buildSegment()...)
	return out
}

func (d Document) buildEBMLHeader() []byte {
	body := append(stringField(idDocType, "webm"),
		uintField(0x4287, 2)...) // EBMLMaxVersion 2
	return mustElem(idEBML, body)
}

func (d Document) buildSegment() []byte {
	body := d.buildInfo()
	body = append(body, d.buildTracks()...)
	body = append(body, d.buildClusters()...)
	if d.UnknownSegment {
		marker, err := codec.UnknownSizeVInt(1)
		if err != nil {
			panic(err)
		}
		idBytes, err := codec.EncodeID(idSegment)
		if err != nil {
			panic(err)
		}
		return append(append(idBytes, marker...), body...)
	}
	return mustElem(idSegment, body)
}

func (d Document) buildInfo() []byte {
	body := uintField(idTimecodeScale, d.TimecodeScale)
	body = append(body, floatField(idDuration, d.DurationMS)...)
	body = append(body, stringField(idMuxingApp, "review-fixture")...)
	body = append(body, stringField(idWritingApp, "review-fixture")...)
	return mustElem(idInfo, body)
}

func (d Document) buildTracks() []byte {
	var body []byte
	for _, tr := range d.Tracks {
		body = append(body, d.buildTrackEntry(tr)...)
	}
	return mustElem(idTracks, body)
}

func (d Document) buildTrackEntry(tr TrackSpec) []byte {
	body := uintField(idTrackNumber, uint64(tr.Number))
	body = append(body, uintField(idTrackUID, uint64(tr.UID))...)
	body = append(body, uintField(idTrackType, uint64(tr.Type))...)
	body = append(body, stringField(idCodecID, tr.Codec)...)
	if tr.DefaultDurNS > 0 {
		body = append(body, uintField(idDefaultDur, tr.DefaultDurNS)...)
	}
	if tr.Type == TrackTypeAudio {
		var audio []byte
		// Omit Channels entirely when the layout is unknown.
		if tr.Channels > 0 {
			audio = append(audio, uintField(idChannels, uint64(tr.Channels))...)
		}
		if tr.SampleRate > 0 {
			audio = append(audio, floatField(idSamplingFreq, tr.SampleRate)...)
		}
		if tr.BitDepth > 0 {
			audio = append(audio, uintField(idBitDepth, uint64(tr.BitDepth))...)
		}
		body = append(body, mustElem(idAudio, audio)...)
	}
	if tr.Matrix > 0 || tr.Transfer > 0 || tr.Primar > 0 {
		var colour []byte
		if tr.Matrix > 0 {
			colour = append(colour, uintField(idMatrixCoeffs, tr.Matrix)...)
		}
		if tr.Transfer > 0 {
			colour = append(colour, uintField(idTransferChar, tr.Transfer)...)
		}
		if tr.Primar > 0 {
			colour = append(colour, uintField(idPrimaries, tr.Primar)...)
		}
		body = append(body, mustElem(idColour, colour)...)
	}
	return mustElem(idTrackEntry, body)
}

// buildClusters groups blocks into 100 ms clusters and renders them.
func (d Document) buildClusters() []byte {
	if len(d.Blocks) == 0 {
		return nil
	}
	var out []byte
	clusterStart := d.Blocks[0].TimeMS / 100 * 100
	var pending []BlockSpec
	flush := func(startMS int64, blocks []BlockSpec) {
		out = append(out, d.buildCluster(startMS, blocks)...)
	}
	for _, blk := range d.Blocks {
		start := blk.TimeMS / 100 * 100
		if start != clusterStart {
			flush(clusterStart, pending)
			clusterStart = start
			pending = nil
		}
		pending = append(pending, blk)
	}
	if len(pending) > 0 {
		flush(clusterStart, pending)
	}
	return out
}

func (d Document) buildCluster(startMS int64, blocks []BlockSpec) []byte {
	body := uintField(idTimecode, uint64(startMS))
	last := len(blocks) - 1
	for i, blk := range blocks {
		rel := int16(blk.TimeMS - startMS)
		var flags byte
		if blk.Keyframe {
			flags = 0x80
		}
		header := codec.EncodeBlockHeader(blk.Track, rel, flags)
		raw := append(header, blk.Payload...)
		// The final block of the final cluster may use an unknown-size
		// block group to exercise parent-bounded parsing.
		if d.UnknownCluster && i == last {
			group := mustElem(idBlock, raw)
			marker, _ := codec.UnknownSizeVInt(1)
			idBytes, _ := codec.EncodeID(idBlockGroup)
			body = append(body, append(append(idBytes, marker...), group...)...)
		} else {
			body = append(body, mustElem(idSimpleBlock, raw)...)
		}
	}
	if d.UnknownCluster {
		marker, _ := codec.UnknownSizeVInt(1)
		idBytes, _ := codec.EncodeID(idCluster)
		return append(append(idBytes, marker...), body...)
	}
	return mustElem(idCluster, body)
}

// randPayload returns n deterministic pseudo-random bytes for a seed.
func randPayload(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.Intn(256))
	}
	return b
}
