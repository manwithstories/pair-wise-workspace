package codec

import (
	"github.com/pion/rtp"

	"mediad/internal/clock"
)

// RTPMapper extends 16-bit sequence numbers and 32-bit RTP timestamps without
// allowing wire wraparound to move media time backward.
type RTPMapper struct {
	ClockRate     int64
	baseSequence  uint64
	lastSequence  uint64
	haveSequence  bool
	baseTimestamp uint64
	lastTimestamp uint64
	haveTimestamp bool
}

// MappedPacket is an unwrapped RTP sample with rational media time.
type MappedPacket struct {
	Packet        rtp.Packet
	ExtSequence   uint64
	ExtTimestamp  uint64
	MediaTime     clock.Rational
	DurationTicks int64
}

func NewRTPMapper(clockRate int64, baseSequence uint16, baseTimestamp uint32) *RTPMapper {
	m := &RTPMapper{ClockRate: clockRate}
	m.lastSequence = extend16(0, baseSequence)
	m.baseSequence = m.lastSequence
	m.haveSequence = true
	m.lastTimestamp = extend32(0, baseTimestamp)
	m.baseTimestamp = m.lastTimestamp
	m.haveTimestamp = true
	return m
}

// Map accepts wire fields and returns unwrapped values plus media time.
func (m *RTPMapper) Map(seq uint16, ts uint32, marker bool, payload []byte) (MappedPacket, error) {
	if m.ClockRate <= 0 {
		return MappedPacket{}, nil
	}
	if !m.haveSequence {
		m.lastSequence = uint64(seq)
		m.baseSequence = m.lastSequence
		m.haveSequence = true
	} else {
		m.lastSequence = ExtendSequence(m.lastSequence, seq)
	}
	if !m.haveTimestamp {
		m.lastTimestamp = uint64(ts)
		m.baseTimestamp = m.lastTimestamp
		m.haveTimestamp = true
	} else {
		m.lastTimestamp = ExtendTimestamp(m.lastTimestamp, ts)
	}
	relTicks := int64(m.lastTimestamp - m.baseTimestamp)
	mediaTime, err := clock.SamplesToTime(relTicks, m.ClockRate)
	if err != nil {
		return MappedPacket{}, err
	}
	pkt := rtp.Packet{
		Header: rtp.Header{
			Version: 2, PayloadType: 96, SequenceNumber: seq,
			Timestamp: ts, Marker: marker, SSRC: 0x4d454431,
		},
		Payload: payload,
	}
	return MappedPacket{
		Packet: pkt, ExtSequence: m.lastSequence, ExtTimestamp: m.lastTimestamp,
		MediaTime: mediaTime, DurationTicks: relTicks,
	}, nil
}

// ExtendSequence unwraps a 16-bit sequence relative to last extended value.
func ExtendSequence(last uint64, wire uint16) uint64 {
	return extendBits(last, uint64(wire), 16)
}

// ExtendTimestamp unwraps a 32-bit timestamp relative to last extended value.
func ExtendTimestamp(last uint64, wire uint32) uint64 {
	return extendBits(last, uint64(wire), 32)
}

func extend16(_ uint64, v uint16) uint64 { return uint64(v) }
func extend32(_ uint64, v uint32) uint64 { return uint64(v) }

func extendBits(last, wire uint64, bits uint) uint64 {
	mask := uint64((1 << bits) - 1)
	upper := last &^ mask
	candidate := upper | wire
	half := uint64(1) << (bits - 1)
	if candidate+half < last {
		candidate += uint64(1) << bits
	} else if candidate > last+half {
		candidate -= uint64(1) << bits
	}
	return candidate
}

// WireTime reconstructs rational time directly from an extended timestamp.
func WireTime(extTimestamp, base uint64, rate int64) (clock.Rational, error) {
	return clock.SamplesToTime(int64(extTimestamp-base), rate)
}
