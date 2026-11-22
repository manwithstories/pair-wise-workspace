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

// Frame flags carried in the capture-node framing layer.
const (
	FlagTruncated uint8 = 1 << 0 // capture node marked the write incomplete
	FlagKeyframe  uint8 = 1 << 1 // payload starts a keyframe
)

// ErrTruncated marks a frame the capture node flagged as incomplete, or one
// whose declared length exceeds the available bytes.
var ErrTruncated = errors.New("packet: truncated frame")

// ErrCorrupt marks a frame that fails RTP parsing.
var ErrCorrupt = errors.New("packet: corrupt rtp packet")

// EncodeFrame frames raw RTP bytes as [uint16 len][uint8 flags][payload].
func EncodeFrame(raw []byte, flags uint8) []byte {
	out := make([]byte, 3+len(raw))
	binary.BigEndian.PutUint16(out, uint16(len(raw)))
	out[2] = flags
	copy(out[3:], raw)
	return out
}

// ReadPacket reads one framed RTP packet from r, returning the packet and
// the frame flags. A frame flagged truncated by the capture node, or one
// cut short mid-payload, yields ErrTruncated; a frame that fails RTP
// parsing yields ErrCorrupt. The stream stays framed: a truncated frame's
// payload is consumed before the error is returned.
func ReadPacket(r io.Reader) (*rtp.Packet, uint8, error) {
	var hdr [3]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, 0, err
	}
	n := int(binary.BigEndian.Uint16(hdr[:2]))
	flags := hdr[2]
	if n > MaxPacketSize {
		return nil, flags, fmt.Errorf("packet: frame of %d bytes exceeds limit", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, flags, fmt.Errorf("%w: declared %d bytes", ErrTruncated, n)
	}
	if flags&FlagTruncated != 0 {
		return nil, flags, fmt.Errorf("%w: flagged by capture node", ErrTruncated)
	}
	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(buf); err != nil {
		return nil, flags, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return pkt, flags, nil
}
