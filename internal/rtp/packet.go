// Package rtp parses RTP/RTCP packets, reorders out-of-order streams,
// identifies sequence gaps and calibrates track clocks against Sender
// Reports. All packet builders in this package also generate the local
// fixtures used by the demo; no external captures are ever read.
package rtp

import (
	"encoding/binary"
	"errors"

	"mediamix/internal/clock"
)

// Version is the only supported RTP version (RFC 3550).
const Version = 2

// Header constants.
const (
	minHeaderLen   = 12
	extHeaderLen   = 4
	csrcLen        = 4
	maxPayloadType = 127
)

// Packet is a parsed RTP packet.
type Packet struct {
	Version        uint8
	Padding        bool
	Extension      bool
	CC             uint8
	Marker         bool
	PayloadType    uint8
	Seq            uint16
	Timestamp      uint32
	SSRC           uint32
	CSRC           []uint32
	ExtProfile     uint16
	ExtData        []byte
	Payload        []byte
	PaddingBytes   int
	RawLen         int
	ArrivalTick     clock.Tick
}

// ErrMalformed is the sentinel wrapped by parse failures.
var ErrMalformed = errors.New("malformed RTP packet")

func malformed(seq uint16, detail string) *clock.MediaError {
	return clock.NewError(clock.StageIngest, clock.ErrInvalidPacket, seq, false, detail)
}

// Parse validates and parses one RTP datagram. Invalid packet lengths are
// rejected with a non-recoverable clock.MediaError so the caller can map
// them to exit code 2.
func Parse(data []byte) (*Packet, error) {
	n := len(data)
	if n < minHeaderLen {
		return nil, malformed(0, "packet shorter than 12 byte header")
	}
	b0, b1 := data[0], data[1]
	p := &Packet{RawLen: n}
	p.Version = b0 >> 6
	if p.Version != Version {
		return nil, malformed(0, "unsupported RTP version")
	}
	p.Padding = b0&0x20 != 0
	p.Extension = b0&0x10 != 0
	p.CC = b0 & 0x0F
	p.Marker = b1&0x80 != 0
	p.PayloadType = b1 & 0x7F
	if p.PayloadType > maxPayloadType {
		return nil, malformed(0, "payload type out of range")
	}
	p.Seq = binary.BigEndian.Uint16(data[2:4])
	p.Timestamp = binary.BigEndian.Uint32(data[4:8])
	p.SSRC = binary.BigEndian.Uint32(data[8:12])

	off := minHeaderLen
	need := off + int(p.CC)*csrcLen
	if n < need {
		return nil, malformed(p.Seq, "truncated CSRC list")
	}
	if p.CC > 0 {
		p.CSRC = make([]uint32, p.CC)
		for i := range p.CSRC {
			p.CSRC[i] = binary.BigEndian.Uint32(data[off:])
			off += csrcLen
		}
	}
	if p.Extension {
		if n < off+extHeaderLen {
			return nil, malformed(p.Seq, "truncated extension header")
		}
		p.ExtProfile = binary.BigEndian.Uint16(data[off:])
		words := int(binary.BigEndian.Uint16(data[off+2:]))
		off += extHeaderLen
		if n < off+words*4 {
			return nil, malformed(p.Seq, "truncated extension data")
		}
		p.ExtData = append([]byte(nil), data[off:off+words*4]...)
		off += words * 4
	}
	if p.Padding {
		pad := int(data[n-1])
		if pad == 0 || pad > n-off {
			return nil, malformed(p.Seq, "invalid padding length")
		}
		p.PaddingBytes = pad
	}
	if n-off-p.PaddingBytes < 0 {
		return nil, malformed(p.Seq, "padding exceeds packet size")
	}
	p.Payload = append([]byte(nil), data[off:n-p.PaddingBytes]...)
	return p, nil
}

// Marshal serialises the packet back to wire format. It is used by the
// fixture generator and round-trip tests.
func (p *Packet) Marshal() []byte {
	csrc := len(p.CSRC)
	size := minHeaderLen + csrc*csrcLen + len(p.Payload) + p.PaddingBytes
	if p.Extension {
		size += extHeaderLen + len(p.ExtData)
	}
	buf := make([]byte, 0, size)
	var b0 uint8 = Version << 6
	if p.Padding {
		b0 |= 0x20
	}
	if p.Extension {
		b0 |= 0x10
	}
	b0 |= uint8(csrc) & 0x0F
	b1 := p.PayloadType & 0x7F
	if p.Marker {
		b1 |= 0x80
	}
	buf = append(buf, b0, b1)
	buf = binary.BigEndian.AppendUint16(buf, p.Seq)
	buf = binary.BigEndian.AppendUint32(buf, p.Timestamp)
	buf = binary.BigEndian.AppendUint32(buf, p.SSRC)
	for _, c := range p.CSRC {
		buf = binary.BigEndian.AppendUint32(buf, c)
	}
	if p.Extension {
		buf = binary.BigEndian.AppendUint16(buf, p.ExtProfile)
		buf = binary.BigEndian.AppendUint16(buf, uint16(len(p.ExtData)/4))
		buf = append(buf, p.ExtData...)
	}
	buf = append(buf, p.Payload...)
	for i := 0; i < p.PaddingBytes; i++ {
		buf = append(buf, 0)
	}
	if p.Padding {
		buf[len(buf)-1] = byte(p.PaddingBytes)
	}
	return buf
}
