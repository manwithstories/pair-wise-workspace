// Package export writes the reconstructed timeline to local artifacts:
// fixed-size frame sequences, an MP4 container with track timing, a
// per-frame provenance manifest, and the checkpoint that lets an
// interrupted run resume from its last valid segment.
package export

import (
	"encoding/binary"

	"medpipe/internal/merge"
)

// Frame layout constants. Every frame is exactly FrameSize bytes so the
// sequence can be reviewed and re-sliced without any windowing logic.
const (
	FrameSize      = 256
	FrameMagic     = 0x4D504652 // "MPFR"
	payloadSample  = FrameSize - 92
	FramesPerSeg   = 1024
	MaxOutputBytes = 1 << 30
)

// EncodeFrame serializes one timeline event into a fixed-size frame.
// The encoding is a pure function of the event and its index, so
// identical input always produces identical bytes.
//
// Layout (big-endian):
//
//	0   magic          4
//	4   frame index    8
//	12  track id       4
//	16  ext sequence   8
//	24  shared num     8
//	32  shared den     8
//	40  rtp timestamp  4
//	44  payload hash   32 (SHA-256)
//	76  payload len    4
//	80  missing before 4
//	84  source offset  8
//	92  payload sample up to 164 bytes, zero-padded
func EncodeFrame(index uint64, ev *merge.Event) [FrameSize]byte {
	var f [FrameSize]byte
	binary.BigEndian.PutUint32(f[0:4], FrameMagic)
	binary.BigEndian.PutUint64(f[4:12], index)
	binary.BigEndian.PutUint32(f[12:16], ev.Track)
	binary.BigEndian.PutUint64(f[16:24], ev.ExtSeq)
	binary.BigEndian.PutUint64(f[24:32], uint64(ev.Shared.N))
	binary.BigEndian.PutUint64(f[32:40], uint64(ev.Shared.D))
	binary.BigEndian.PutUint32(f[40:44], ev.RTPTimestamp)
	copy(f[44:76], ev.Hash[:])
	binary.BigEndian.PutUint32(f[76:80], uint32(len(ev.Payload)))
	binary.BigEndian.PutUint32(f[80:84], ev.MissingBefore)
	binary.BigEndian.PutUint64(f[84:92], uint64(ev.Offset))
	n := len(ev.Payload)
	if n > payloadSample {
		n = payloadSample
	}
	copy(f[92:92+n], ev.Payload[:n])
	return f
}

// DecodeFrameHeader reads the fields verify needs back out of a frame.
type FrameHeader struct {
	Index   uint64
	Track   uint32
	ExtSeq  uint64
	Missing uint32
}

// ParseFrameHeader validates the magic and extracts the header.
func ParseFrameHeader(f []byte) (FrameHeader, bool) {
	if len(f) != FrameSize || binary.BigEndian.Uint32(f[0:4]) != FrameMagic {
		return FrameHeader{}, false
	}
	return FrameHeader{
		Index:   binary.BigEndian.Uint64(f[4:12]),
		Track:   binary.BigEndian.Uint32(f[12:16]),
		ExtSeq:  binary.BigEndian.Uint64(f[16:24]),
		Missing: binary.BigEndian.Uint32(f[80:84]),
	}, true
}
