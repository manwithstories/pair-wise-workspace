// Package stream implements the custom AVPK sample container: a compact
// binary stream of track descriptors followed by CRC-protected media packets
// carrying PNG (video), PCM (audio) or transition payloads.
//
// Wire layout (little-endian):
//
//	magic "AVPK1\n", version u8, trackCount u16, packetCount u32, flags u32
//	track descriptor * trackCount
//	packet         * packetCount (or until clean EOF)
//
// Packet:
//
//	sync(0xA7 0x5B) track u32 seq u64 pts i64 duration u32 flags u16
//	payloadLen u32 crc32 u32 payload[payloadLen]
//
// The CRC is IEEE CRC-32 over the fixed packet header (track..payloadLen)
// concatenated with the payload.
package stream

import (
	"encoding/binary"
	"hash/crc32"
	"io"

	"avflow/internal/model"
)

// Magic and version identify an AVPK version-1 stream.
const (
	Magic   = "AVPK1\n"
	Version = byte(1)
	// MaxPayloadDefault bounds a single packet payload (8 MiB). Oversized
	// length fields are rejected before any allocation occurs.
	MaxPayloadDefault = 8 << 20
)

// Sync markers frame every packet so a parser can locate packet boundaries.
const (
	Sync0 byte = 0xA7
	Sync1 byte = 0x5B
)

// Packet flag bits.
const (
	FlagKeyframe uint16 = 1 << iota
)

// TrackKind selects the payload semantics of a track.
type TrackKind uint8

// Supported track kinds.
const (
	KindVideo TrackKind = iota
	KindAudio
	KindTransition
)

// Color tags carried by video tracks.
const (
	ColorSRGB    uint8 = 1
	ColorBT709   uint8 = 2
	ColorUnknown uint8 = 0xFF
)

// PCM channel layouts, identified by speaker count.
const (
	LayoutMono   uint8 = 1
	LayoutStereo uint8 = 2
	Layout51     uint8 = 6
	Layout71     uint8 = 8
)

// TrackDesc is one entry of the container's track directory.
type TrackDesc struct {
	ID                 uint32
	Kind               TrackKind
	TimeBase           model.Rational
	ColorTag           uint8
	Width              uint16
	Height             uint16
	Channels           uint8
	ChannelLayout      uint8
	SampleRate         uint32
	MaxKeyIntervalNS   int64
	Name               string
}

// PacketSpec is one encodable packet. CorruptCRC instructs the writer to emit
// an intentionally wrong checksum, used by the fault-injection fixtures.
// ColorTag is 0 for non-video packets; otherwise it carries the per-packet
// sRGB/BT.709/unknown label independently of the payload.
type PacketSpec struct {
	Track      uint32
	Seq        uint64
	PTS        int64
	Duration   uint32
	Keyframe   bool
	ColorTag   uint8
	Payload    []byte
	CorruptCRC bool
}

// Encode writes a complete AVPK stream. It is the single serializer used by
// the fixture generator and by the stream tests.
func Encode(w io.Writer, tracks []TrackDesc, packets []PacketSpec) error {
	bw := newByteWriter(w)
	bw.bytes([]byte(Magic))
	bw.u8(Version)
	bw.u16(uint16(len(tracks)))
	bw.u32(uint32(len(packets)))
	bw.u32(0) // reserved container flags
	for _, t := range tracks {
		bw.u32(t.ID)
		bw.u8(byte(t.Kind))
		bw.u32(uint32(t.TimeBase.Num))
		bw.u32(uint32(t.TimeBase.Den))
		bw.u8(t.ColorTag)
		bw.u16(t.Width)
		bw.u16(t.Height)
		bw.u8(t.Channels)
		bw.u8(t.ChannelLayout)
		bw.u32(t.SampleRate)
		bw.u64(uint64(t.MaxKeyIntervalNS))
		bw.u8(byte(len(t.Name)))
		bw.bytes([]byte(t.Name))
	}
	for _, p := range packets {
		if err := encodePacket(bw, p); err != nil {
			return err
		}
	}
	return bw.err
}

func encodePacket(bw *byteWriter, p PacketSpec) error {
	hdr := make([]byte, 37) // 2 sync + 4+8+8+4+2 flags +1 tag +4 len +4 crc
	o := 0
	hdr[0], hdr[1] = Sync0, Sync1
	o += 2
	binary.LittleEndian.PutUint32(hdr[o:], p.Track)
	o += 4
	binary.LittleEndian.PutUint64(hdr[o:], p.Seq)
	o += 8
	binary.LittleEndian.PutUint64(hdr[o:], uint64(p.PTS))
	o += 8
	binary.LittleEndian.PutUint32(hdr[o:], uint32(p.Duration))
	o += 4
	var flags uint16
	if p.Keyframe {
		flags |= FlagKeyframe
	}
	binary.LittleEndian.PutUint16(hdr[o:], flags)
	o += 2
	hdr[o] = p.ColorTag
	o++
	binary.LittleEndian.PutUint32(hdr[o:], uint32(len(p.Payload)))
	o += 4
	sum := crc32.ChecksumIEEE(hdr[2:o])
	sum = crc32.Update(sum, crc32.IEEETable, p.Payload)
	if p.CorruptCRC {
		sum ^= 0xDEADBEEF
	}
	binary.LittleEndian.PutUint32(hdr[o:], sum)
	bw.bytes(hdr)
	bw.bytes(p.Payload)
	return bw.err
}

type byteWriter struct {
	w   io.Writer
	err error
}

func newByteWriter(w io.Writer) *byteWriter { return &byteWriter{w: w} }

func (b *byteWriter) write(p []byte) {
	if b.err != nil {
		return
	}
	_, b.err = b.w.Write(p)
}

func (b *byteWriter) bytes(p []byte) { b.write(p) }
func (b *byteWriter) u8(v uint8)     { b.write([]byte{v}) }
func (b *byteWriter) u16(v uint16)    { b.write(binary.LittleEndian.AppendUint16(nil, v)) }
func (b *byteWriter) u32(v uint32)    { b.write(binary.LittleEndian.AppendUint32(nil, v)) }
func (b *byteWriter) u64(v uint64)    { b.write(binary.LittleEndian.AppendUint64(nil, v)) }
