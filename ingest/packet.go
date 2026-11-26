// Package ingest parses transport packet headers and normalizes arrival order.
package ingest

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"strings"
)

const (
	PacketMagic     = "MCPK"
	PacketHeaderLen = 40
	Version         = 1
)

var (
	ErrTruncatedPacket = errors.New("ingest: truncated packet header")
	ErrBadMagic        = errors.New("ingest: bad packet magic")
	ErrBadPacketCRC    = errors.New("ingest: packet header CRC mismatch")
)

// Header is the bounded transport header. Camera and Stream are short labels.
type Header struct {
	Camera     string `json:"camera"`
	Stream     string `json:"stream"`
	Seq        uint64 `json:"seq"`
	PayloadLen uint32 `json:"payload_len"`
	Flags      uint8  `json:"flags"`
	CRC        uint32 `json:"crc"`
	CRCValid   bool   `json:"crc_valid"`
}

// Packet combines a header with optional in-memory fixture payload. Production
// readers may discard Payload after header validation.
type Packet struct {
	Header
	Payload []byte `json:"-"`
}

func putFixed(dst []byte, value string) {
	for i := 0; i < len(dst); i++ {
		dst[i] = 0
	}
	copy(dst, value)
}

func fixedString(src []byte) string {
	return strings.TrimRight(string(src), "\x00")
}

// EncodeHeader serializes a fixed 40-byte packet header.
func EncodeHeader(h Header) []byte {
	b := make([]byte, PacketHeaderLen)
	copy(b[0:4], PacketMagic)
	b[4] = Version
	b[5] = h.Flags
	putFixed(b[8:16], h.Camera)
	putFixed(b[16:24], h.Stream)
	binary.LittleEndian.PutUint64(b[24:32], h.Seq)
	binary.LittleEndian.PutUint32(b[32:36], h.PayloadLen)
	binary.LittleEndian.PutUint32(b[36:40], crc32.ChecksumIEEE(b[:36]))
	return b
}

// EncodePacket appends payload bytes to the header.
func EncodePacket(p Packet) []byte {
	h := p.Header
	h.PayloadLen = uint32(len(p.Payload))
	out := EncodeHeader(h)
	return append(out, p.Payload...)
}

// DecodeHeader parses and validates a fixed packet header.
func DecodeHeader(b []byte) (Header, error) {
	if len(b) < PacketHeaderLen {
		return Header{}, fmt.Errorf("%w: got %d", ErrTruncatedPacket, len(b))
	}
	if string(b[0:4]) != PacketMagic {
		return Header{}, ErrBadMagic
	}
	h := Header{
		Flags:      b[5],
		Camera:     fixedString(b[8:16]),
		Stream:     fixedString(b[16:24]),
		Seq:        binary.LittleEndian.Uint64(b[24:32]),
		PayloadLen: binary.LittleEndian.Uint32(b[32:36]),
		CRC:        binary.LittleEndian.Uint32(b[36:40]),
	}
	want := crc32.ChecksumIEEE(b[:36])
	h.CRCValid = want == h.CRC
	if !h.CRCValid {
		return h, fmt.Errorf("%w: seq=%d stored=%08x calculated=%08x", ErrBadPacketCRC, h.Seq, h.CRC, want)
	}
	return h, nil
}

// ParsePacket parses one complete encoded packet and slices its payload.
func ParsePacket(b []byte) (Packet, error) {
	h, err := DecodeHeader(b)
	if err != nil && !errors.Is(err, ErrBadPacketCRC) {
		return Packet{}, err
	}
	end := PacketHeaderLen + int(h.PayloadLen)
	var payload []byte
	if end <= len(b) {
		payload = b[PacketHeaderLen:end]
	}
	return Packet{Header: h, Payload: payload}, err
}
