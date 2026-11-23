// Package rtp parses RTP packets (via pion/rtp) and maps their clock
// timestamps onto the unified timeline.
//
// RTP timestamps are 32-bit and wrap at 2^32 ticks. A stream keeps the
// rollover counter (ROC) so packet order can be reconstructed and every
// timestamp becomes a full 64-bit extended tick count before conversion to
// the rational millisecond base. The parser validates fixed-header bounds and
// rejects timestamp jumps past the configured horizon instead of trusting
// unbounded input.
package rtp

import (
	"github.com/pion/rtp"

	"mediad/diag"
	"mediad/timeline"
)

// wrapMod is the RTP timestamp modulus.
const wrapMod int64 = 1 << 32

// Stream tracks the state of one synchronized RTP source.
type Stream struct {
	track      string
	clockRate  uint32
	baseTicks  uint32 // timestamp of the first packet
	haveBase   bool
	lastExt    int64  // extended tick count of the previous packet
	roc        uint32 // rollover counter
	packets    int64
	maxJumpMS  int64
	limits     diag.Limits
}

// NewStream opens a stream for a track at a fixed RTP clock rate (e.g. 90000
// for video). The rate is immutable for the stream's lifetime.
func NewStream(track string, clockRate uint32, limits diag.Limits) (*Stream, error) {
	if clockRate == 0 {
		return nil, diag.New(diag.EParam, track, "RTP clock rate must be non-zero")
	}
	return &Stream{track: track, clockRate: clockRate, maxJumpMS: limits.MaxTimestampMS, limits: limits}, nil
}

// Packet is the parsed result mapped onto the unified time base.
type Packet struct {
	// ExtTicks is the 64-bit rollover-extended RTP timestamp.
	ExtTicks int64
	// Seq is the raw 16-bit sequence number.
	Seq uint16
	// Marker is the RTP marker bit.
	Marker bool
	// Keyframe is inferred from a marker on video payloads (H.264 IDR/key
	// frames typically set it); callers may refine with payload inspection.
	Keyframe bool
	// Time is the packet timestamp on the unified rational time base,
	// measured relative to the stream's first packet.
	Time timeline.Rat
	// Payload holds the RTP payload bytes (not retained by the stream).
	Payload []byte
}

// Parse unmarshals one UDP-style RTP datagram and maps it onto the timeline.
// Malformed packets return ERTP with the track set; no partial state is kept.
func (s *Stream) Parse(raw []byte) (Packet, error) {
	if len(raw) < 12 || raw[0]>>6 != 2 {
		return Packet{}, diag.At(diag.ERTP, s.track, -1,
			"malformed RTP packet: short buffer or RTP version != 2")
	}
	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(raw); err != nil {
		return Packet{}, diag.At(diag.ERTP, s.track, -1, "malformed RTP packet: %v", err)
	}
	if !s.haveBase {
		s.baseTicks = pkt.Timestamp
		s.haveBase = true
	}
	ext := s.extend(pkt.Timestamp)
	rel := ext - s.firstExt()
	if rel < 0 {
		// A packet older than the stream base after ROC accounting is
		// evidence of a reordering wider than one wrap; reject deterministically.
		return Packet{}, diag.At(diag.ERTP, s.track, -1,
			"packet timestamp %d predates stream base", pkt.Timestamp)
	}
	t := timeline.NewRat(rel, int64(s.clockRate))
	ms := t.Millis()
	if err := s.limits.CheckTimestampMS(s.track, ms); err != nil {
		return Packet{}, err
	}
	if s.packets > 0 {
		jump := t.Sub(s.lastTime()).Millis()
		if jump > s.maxJumpMS {
			return Packet{}, diag.At(diag.ERTP, s.track, -1,
				"timestamp jump %dms exceeds limit %dms", jump, s.maxJumpMS)
		}
	}
	s.lastExt = ext
	s.packets++
	return Packet{
		ExtTicks: ext,
		Seq:      pkt.SequenceNumber,
		Marker:   pkt.Marker,
		Keyframe: pkt.Marker,
		Time:     t,
		Payload:  pkt.Payload,
	}, nil
}

// extend applies rollover counting to a raw 32-bit timestamp. Forward and
// backward wraparound are both handled by comparing against the last value:
// a large forward decrease means a wrap happened, a large backward increase
// means a reordered packet from before the wrap.
func (s *Stream) extend(ts uint32) int64 {
	if s.packets == 0 {
		return int64(ts)
	}
	prev := uint32(s.lastExt)
	delta := int32(ts - prev) // signed modular difference
	ext := s.lastExt + int64(delta)
	// Forward wrap: the raw 32-bit value decreased while the modular delta is
	// positive (time advanced through zero). A negative delta is ordinary
	// reordering and must not touch the rollover counter.
	if delta > 0 && ts < prev {
		s.roc++
	}
	return ext
}

func (s *Stream) firstExt() int64 { return int64(s.baseTicks) }

func (s *Stream) lastTime() timeline.Rat {
	return timeline.NewRat(s.lastExt-s.firstExt(), int64(s.clockRate))
}

// PacketsSeen reports how many packets were accepted.
func (s *Stream) PacketsSeen() int64 { return s.packets }

// ROC exposes the current rollover counter (test/telemetry).
func (s *Stream) ROC() uint32 { return s.roc }

// BuildPacket constructs a raw RTP datagram for fixtures and tests without
// extensions or CSRC entries.
func BuildPacket(ssrc uint32, seq uint16, ts uint32, marker bool, payload []byte) []byte {
	hdr := rtp.Header{
		Version:        2,
		PayloadType:    96,
		SequenceNumber: seq,
		Timestamp:      ts,
		SSRC:           ssrc,
		Marker:         marker,
	}
	pkt := rtp.Packet{Header: hdr, Payload: payload}
	raw, err := pkt.Marshal()
	if err != nil {
		panic(err) // only reached with impossible header state in fixtures
	}
	return raw
}

// MaxRTPTimestampMS is the sanity ceiling applied to mapped packet times.
func MaxRTPTimestampMS(limits diag.Limits) int64 { return limits.MaxTimestampMS }
