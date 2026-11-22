// Package codec parses H.264/H.265/AV1 elementary bitstreams into access
// units. It only frames and classifies NAL/OBU units; it never decodes or
// re-encodes media payloads.
package codec

import "mediapack/internal/clock"

// Codec identifies one of the three supported elementary stream formats.
type Codec int

const (
	H264 Codec = iota
	H265
	AV1
)

func (c Codec) String() string {
	switch c {
	case H264:
		return "h264"
	case H265:
		return "h265"
	case AV1:
		return "av1"
	default:
		return "unknown"
	}
}

// MaxNALBytes bounds a single framed unit so a missing start code can never
// grow the parser buffer without limit.
const MaxNALBytes = 8 << 20

// NAL is one framed unit. Raw is the unit payload (header included, start
// code excluded).
type NAL struct {
	Type   uint8
	Offset int64 // absolute input offset of the first unit byte
	Raw    []byte
	Key    bool
	Config bool // SPS/PPS/VPS or AV1 sequence header
}

// AccessUnit is one timestamped frame, the parser's output unit.
type AccessUnit struct {
	Codec   Codec
	TrackID uint32
	Seq     int64
	PTS     int64 // ticks in clock.RTPClock (90kHz), assigned by the parser
	Key     bool
	NALs    []NAL
	// ByteOffset/ByteLen locate the AU (start code included) in the input.
	ByteOffset int64
	ByteLen    int
}

// Timebase is the unified clock stamped onto every access unit.
func (AccessUnit) Timebase() clock.Rational { return clock.RTPClock() }

// PayloadLen sums the framed byte counts of all units.
func (a AccessUnit) PayloadLen() int {
	n := 0
	for _, u := range a.NALs {
		n += len(u.Raw)
	}
	return n
}
