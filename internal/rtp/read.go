// Package rtp reads length-prefixed RTP/RTCP datagrams, reorders the RTP
// sequence space (including 16-bit wraparound, duplicates and late packets),
// and surfaces ordered media frames plus sender-report anchors.
//
// Wire framing used by the archive fixtures (no network sockets): every
// datagram is preceded by a big-endian uint16 payload length. RTP carries
// RFC 2250 MPEG-TS; RTCP packets are distinguished by their distinct version
// and payload-type range per RFC 5761.
package rtp

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/archive/tsrestore/internal/timeline"
)

const (
	// HeaderLen is the minimum RTP fixed header size in bytes.
	HeaderLen = 12
	// PayloadTypeRTCPMin bounds the RFC 5761 RTCP dynamic payload range.
	PayloadTypeRTCPMin = 64
	PayloadTypeRTCPMax = 95
	// SRType is the RTCP packet type for sender reports.
	SRType = 200
)

// Packet is one parsed RTP datagram in receive order.
type Packet struct {
	Version    uint8
	Padding    bool
	Extension  bool
	Marker     bool
	PayloadType uint8
	Sequence   uint16
	Timestamp  uint32
	SSRC       uint32
	Payload    []byte
}

// IsRTP reports whether a raw datagram begins with an RTP header (V2, no RTCP
// marker in the PT field) using the RFC 5761 demultiplexing rule.
func IsRTP(b []byte) bool {
	if len(b) < 2 {
		return false
	}
	if b[0]>>6 != 2 {
		return false
	}
	pt := b[1] & 0x7F
	return pt < PayloadTypeRTCPMin || pt > PayloadTypeRTCPMax
}

// ParsePacket parses one RTP datagram. Truncated or malformed input is
// reported as a classified Corrupt error rather than a panic.
func ParsePacket(b []byte) (*Packet, error) {
	if len(b) < HeaderLen {
		return nil, timeline.NewError(timeline.ClassCorrupt, "rtp.parse",
			"datagram shorter than RTP fixed header", nil)
	}
	if b[0]>>6 != 2 {
		return nil, timeline.NewError(timeline.ClassCorrupt, "rtp.parse",
			"unsupported RTP version", nil)
	}
	p := &Packet{
		Version:     b[0] >> 6,
		Padding:     b[0]&0x20 != 0,
		Extension:   b[0]&0x10 != 0,
		Marker:      b[1]&0x80 != 0,
		PayloadType: b[1] & 0x7F,
		Sequence:    binary.BigEndian.Uint16(b[2:4]),
		Timestamp:   binary.BigEndian.Uint32(b[4:8]),
		SSRC:        binary.BigEndian.Uint32(b[8:12]),
	}
	csrcCount := int(b[0] & 0x0F)
	off := HeaderLen + 4*csrcCount
	if off > len(b) {
		return nil, timeline.NewError(timeline.ClassCorrupt, "rtp.parse",
			"CSRC list runs past datagram", nil)
	}
	if p.Extension {
		if off+4 > len(b) {
			return nil, timeline.NewError(timeline.ClassCorrupt, "rtp.parse",
				"RTP header extension truncated", nil)
		}
		extWords := int(binary.BigEndian.Uint16(b[off+2 : off+4]))
		off += 4 + 4*extWords
		if off > len(b) {
			return nil, timeline.NewError(timeline.ClassCorrupt, "rtp.parse",
				"RTP extension data runs past datagram", nil)
		}
	}
	end := len(b)
	if p.Padding {
		if off >= end {
			return nil, timeline.NewError(timeline.ClassCorrupt, "rtp.parse",
				"padding-only RTP datagram", nil)
		}
		pad := int(b[end-1])
		if pad == 0 || pad > end-off {
			return nil, timeline.NewError(timeline.ClassCorrupt, "rtp.parse",
				"illegal RTP padding length", nil)
		}
		end -= pad
	}
	if off > end {
		return nil, timeline.NewError(timeline.ClassCorrupt, "rtp.parse",
			"RTP payload offset invalid", nil)
	}
	p.Payload = b[off:end]
	return p, nil
}

// FrameReader pulls length-prefixed datagrams from an underlying reader. It
// does not reorder or interpret RTCP beyond detection; callers feed RTP
// datagrams to the reorder buffer and handle RTCP separately.
type FrameReader struct {
	r   io.Reader
	buf [65535]byte
}

// NewFrameReader wraps a stream of uint16-length-prefixed datagrams.
func NewFrameReader(r io.Reader) *FrameReader { return &FrameReader{r: r} }

// ReadDatagram returns the next datagram in wire order. io.EOF is returned
// unchanged at a clean stream end; truncated framing is a Corrupt error.
func (f *FrameReader) ReadDatagram() ([]byte, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(f.r, lenBuf[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, timeline.NewError(timeline.ClassCorrupt, "rtp.frame",
			"cannot read length prefix", err)
	}
	n := int(binary.BigEndian.Uint16(lenBuf[:]))
	if n == 0 {
		return nil, timeline.NewError(timeline.ClassCorrupt, "rtp.frame",
			"zero-length datagram", nil)
	}
	if _, err := io.ReadFull(f.r, f.buf[:n]); err != nil {
		return nil, timeline.NewError(timeline.ClassCorrupt, "rtp.frame",
			"datagram body truncated", err)
	}
	out := make([]byte, n)
	copy(out, f.buf[:n])
	return out, nil
}
