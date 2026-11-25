// Package fixture generates deterministic MP4/MXF containers, JPEG frames and
// RTP packet streams from a seed. Nothing is read from outside the repository.
package fixture

import (
	"bytes"
	"encoding/binary"
)

// Default geometry and counts. These constants are part of the demo contract:
// 120 frames split into 6 segments of 20.
const (
	DefaultFrames   = 120
	DefaultWidth    = 64
	DefaultHeight   = 48
	DefaultSegments = 6
	// VideoTimescale is the MP4 media timescale of the video track.
	VideoTimescale = 30000
	// RTPClock is the 90 kHz RTP timestamp clock.
	RTPClock = 90000
	// MXFEditRate is the MXF edit rate of the video track (90 kHz).
	MXFEditRate = 90000
	// MXFFirstFrame is the first global frame carried by the MXF clip.
	MXFFirstFrame = 24
	// MP4KeyInterval and MXFKeyInterval determine the 48 keyframes.
	MP4KeyInterval = 5
	MXFKeyInterval = 4
	// DriftPPM is the deliberate clock skew of the RTP senders.
	DriftPPM = 180
	// UnalignedPPM exceeds the 500 ppm alignment bound.
	UnalignedPPM = 800
)

// FrameDelta returns the duration of global frame i in the 30000 Hz video
// timescale. The repeating pattern has three distinct durations, which is what
// keeps the timeline genuinely variable frame rate.
func FrameDelta(i int) int64 {
	switch i % 4 {
	case 0, 1:
		return 1001
	case 2:
		return 1501
	default:
		return 2002
	}
}

// FrameDeltas returns the durations of frames [0,n).
func FrameDeltas(n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = FrameDelta(i)
	}
	return out
}

// FrameTime90k returns the start time of global frame i in 90 kHz ticks.
func FrameTime90k(i int) int64 {
	var t int64
	for j := 0; j < i; j++ {
		t += FrameDelta(j) * 3
	}
	return t
}

// frameDurations90k returns frame durations in 90 kHz ticks.
func frameDurations90k(n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = FrameDelta(i) * 3
	}
	return out
}

// box assembles an ISO BMFF box from payload parts.
func box(typ string, parts ...[]byte) []byte {
	var body bytes.Buffer
	for _, p := range parts {
		body.Write(p)
	}
	out := make([]byte, 8, 8+body.Len())
	binary.BigEndian.PutUint32(out[0:4], uint32(8+body.Len()))
	copy(out[4:8], typ)
	return append(out, body.Bytes()...)
}

// fullBox assembles a FullBox with version and flags.
func fullBox(typ string, version byte, flags uint32, parts ...[]byte) []byte {
	hdr := make([]byte, 4)
	hdr[0] = version
	hdr[1] = byte(flags >> 16)
	hdr[2] = byte(flags >> 8)
	hdr[3] = byte(flags)
	return box(typ, append([][]byte{hdr}, parts...)...)
}

// u8/u16/u32/u64 encode big-endian integers for box payloads.
func u8(v uint8) []byte   { return []byte{v} }
func u16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
func u32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func u64(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }

func cat(parts ...[]byte) []byte {
	var b bytes.Buffer
	for _, p := range parts {
		b.Write(p)
	}
	return b.Bytes()
}

// berLen encodes a BER length using the minimal number of bytes.
func berLen(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	var tmp [8]byte
	i := 8
	for v := n; v > 0; v >>= 8 {
		i--
		tmp[i] = byte(v)
	}
	out := []byte{0x80 | byte(8-i)}
	return append(out, tmp[i:]...)
}

// klv assembles a key-length-value triplet.
func klv(key, value []byte) []byte {
	return cat(key, berLen(len(value)), value)
}
