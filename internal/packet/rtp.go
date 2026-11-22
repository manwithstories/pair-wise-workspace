// Package packet reads RTP packet timestamps and rebuilds a
// keyframe-anchored random-access index.
package packet

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/pion/rtp"
)

// Limits guarding the packet pipeline.
const (
	MaxPacketSize        = 2048
	MaxPacketsPerSegment = 1 << 20
)

// ErrTruncated marks a frame whose declared length exceeds available bytes.
var ErrTruncated = errors.New("packet: truncated frame")

// ErrCorrupt marks a frame that fails RTP parsing.
var ErrCorrupt = errors.New("packet: corrupt rtp packet")

// EncodeFrame prefixes raw RTP bytes with a uint16 length.
func EncodeFrame(raw []byte) []byte {
	out := make([]byte, 2+len(raw))
	binary.BigEndian.PutUint16(out, uint16(len(raw)))
	copy(out[2:], raw)
	return out
}

// ReadPacket reads one length-prefixed RTP packet from r. A short frame
// yields ErrTruncated; a frame that fails RTP parsing yields ErrCorrupt.
func ReadPacket(r io.Reader) (*rtp.Packet, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n > MaxPacketSize {
		return nil, fmt.Errorf("packet: frame of %d bytes exceeds limit", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("%w: declared %d bytes", ErrTruncated, n)
	}
	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(buf); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return pkt, nil
}
