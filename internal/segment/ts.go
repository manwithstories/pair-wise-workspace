// Package segment turns ordered, possibly damaged RTP MPEG-TS payloads back
// into HLS segments. It resynchronizes on the 0x47 sync byte after loss,
// detects video frame boundaries and IDR access units, cuts six-second
// segments on keyframes aligned to a shared time grid, and flags
// discontinuities when a safe cut point cannot be honored.
package segment

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/archive/tsrestore/internal/sample"
)

const (
	tsSize     = sample.TSPacketSize // 188
	syncByte   = sample.SyncByte     // 0x47
	vidPID     = sample.PIDVideo
	targetSecs = sample.SegmentSeconds
	idrNAL     = sample.NALIDR
)

// Frame is one decoded video access unit boundary observed in the TS stream.
type Frame struct {
	RTPTime uint32 // raw RTP timestamp of the frame's first packet
	Tick    uint64 // extended 90 kHz tick via the shared clock
	IsIDR   bool
	// StartOffset is the byte offset in the accumulated TS byte stream where
	// this frame's first TS packet begins.
	StartOffset int
}

// Segment is one cut MPEG-TS segment and its audit metadata.
type Segment struct {
	ID            string // rendition-scoped stable id, e.g. "v1-000007"
	Index         int
	Rendition     string
	Data          []byte
	StartTick     uint64
	EndTick       uint64
	DurationSecs  float64
	Discontinuity bool
	SHA256        string // lowercase hex, 64 chars
	// KeyAligned records that the cut point was an actual IDR access unit.
	KeyAligned bool
}

// ResyncStats counts recovery events while scanning one rendition.
type ResyncStats struct {
	SyncLoss     int // times a sync byte was missing at an expected boundary
	Resynced     int // successful recoveries via sync-byte scanning
	Frames       int
	IDRFrames    int
	Discontinuities int
}

// hash returns the lowercase hex SHA-256 of b.
func hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// resyncFrom scans data from pos for the next valid 188-byte TS sync boundary:
// a 0x47 byte followed by another 0x47 exactly tsSize bytes later. It returns
// the absolute offset of the confirmed sync byte, or -1 if none is found.
func resyncFrom(data []byte, pos int) int {
	for i := pos; i+tsSize < len(data); i++ {
		if data[i] == syncByte && data[i+tsSize] == syncByte {
			return i
		}
	}
	return -1
}

// parseFrameHeader inspects one TS packet. When it is a video PID packet with
// the payload unit start indicator set, it reports whether the embedded NAL
// unit is an IDR. Non-PUSI or non-video packets return ok=false.
func parseFrameHeader(tp []byte) (isIDR, ok bool) {
	if len(tp) < tsSize || tp[0] != syncByte {
		return false, false
	}
	pid := uint16(tp[1]&0x1F)<<8 | uint16(tp[2])
	if pid != vidPID || tp[1]&0x40 == 0 {
		return false, false
	}
	start := 4
	if tp[3]&0x20 != 0 { // adaptation field present
		al := int(tp[4])
		start = 5 + al
		if start >= tsSize {
			return false, false
		}
	}
	// PES: 00 00 01 E0 ... NAL header at PES offset 17 (see sample.putPES).
	if start+17 >= tsSize {
		return false, false
	}
	if tp[start] != 0 || tp[start+1] != 0 || tp[start+2] != 1 || tp[start+3] != 0xE0 {
		return false, false
	}
	return tp[start+17]&0x1F == idrNAL, true
}
