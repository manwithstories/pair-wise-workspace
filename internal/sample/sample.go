// Package sample defines the compressed-sample container exchanged between
// subsystems and the deterministic framing used inside mdat. Frames are never
// decoded: their compressed bytes are preserved verbatim.
package sample

import (
	"encoding/binary"
	"hash/crc32"

	"mediacore/internal/clock"
)

// FrameMagic marks every self-describing sample frame inside mdat.
var FrameMagic = [4]byte{'M', 'C', '0', '1'}

// FrameHeaderLen is magic(4)+length(4)+track(4)+crc(4) = 16 bytes.
const FrameHeaderLen = 16

// Codec identifiers supported by the pipeline.
type Codec string

const (
	CodecH264 Codec = "h264"
	CodecAAC  Codec = "aac"
	CodecOpus Codec = "opus"
)

// Sample is one compressed sample in unified 90 kHz time.
type Sample struct {
	TrackIndex int        // 0-based source trak index
	TrackID    uint32     // mvhd track ID
	Codec      Codec      // codec tag after mapping
	Seq        int        // 0-based sample sequence within its source track
	PTS        clock.Tick // presentation time in unified ticks
	Duration   clock.Tick // duration in unified ticks
	Offset     int64      // absolute file offset of the frame header
	Data       []byte     // compressed sample bytes (copied, retained)
	Keyframe   bool       // sync sample flag
}

// Frame wraps sample bytes with the on-disk framing header.
func Frame(trackID uint32, data []byte) []byte {
	buf := make([]byte, FrameHeaderLen+len(data))
	copy(buf[0:4], FrameMagic[:])
	binary.BigEndian.PutUint32(buf[4:8], uint32(len(data)))
	binary.BigEndian.PutUint32(buf[8:12], trackID)
	binary.BigEndian.PutUint32(buf[12:16], crc32.ChecksumIEEE(data))
	copy(buf[FrameHeaderLen:], data)
	return buf
}

// ParseFrame validates a frame header at the start of b and returns the
// declared track ID and the payload slice. A mismatch yields ok=false and the
// relative offset of the first confirmable abnormal byte within b.
func ParseFrame(b []byte) (trackID uint32, payload []byte, badOffset int, ok bool) {
	if len(b) < FrameHeaderLen {
		return 0, nil, len(b), false
	}
	if [4]byte{b[0], b[1], b[2], b[3]} != FrameMagic {
		return 0, nil, 0, false
	}
	length := binary.BigEndian.Uint32(b[4:8])
	trackID = binary.BigEndian.Uint32(b[8:12])
	sum := binary.BigEndian.Uint32(b[12:16])
	if int(FrameHeaderLen)+int(length) > len(b) {
		return 0, nil, 4, false // length field itself is the bad claim
	}
	payload = b[FrameHeaderLen : FrameHeaderLen+int(length)]
	if crc32.ChecksumIEEE(payload) != sum {
		return 0, nil, 12, false // stored CRC is the first byte proving corruption
	}
	return trackID, payload, 0, true
}
