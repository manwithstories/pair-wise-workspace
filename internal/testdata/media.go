// Package testdata generates deterministic, fully local RTP/RTCP fixture
// streams. No external files or network captures are used: every payload,
// sequence and impairment derives from a seed so a run is byte-for-byte
// reproducible. The generator also emits ground-truth counters that the
// report metrics are scored against.
package testdata

import (
	"encoding/binary"

	"github.com/pion/rtp"
)

// TrackSpec describes one RTP track to synthesize.
type TrackSpec struct {
	SSRC       uint32
	ClockRate  uint32
	PayloadPT  byte
	Packets    int
	KeyEvery   int // a keyframe marker every N packets (<=1 disables)
	TSInterval uint32
	Kind       string
}

// DefaultTracks builds the canonical two-track (video+audio) specification.
func DefaultTracks(perTrack int) []TrackSpec {
	return []TrackSpec{
		{SSRC: 0xA110CE01, ClockRate: 90000, PayloadPT: 96, Packets: perTrack,
			KeyEvery: 25, TSInterval: 900, Kind: "video"},
		{SSRC: 0xA110CE02, ClockRate: 48000, PayloadPT: 97, Packets: perTrack,
			KeyEvery: 0, TSInterval: 480, Kind: "audio"},
	}
}

// payload returns a deterministic payload for a track/ordinal. The byte
// pattern makes round-trip hashing meaningful without external assets.
func payload(ssrc uint32, ordinal int64, size int) []byte {
	b := make([]byte, size)
	var x uint32 = ssrc ^ uint32(ordinal)*2654435761
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x >> 24)
	}
	return b
}

// BuildPacket marshals one RTP packet for a track ordinal.
func BuildPacket(spec TrackSpec, ordinal int64) ([]byte, modelPacketHint, error) {
	hdr := rtp.Header{
		Version:        2,
		PayloadType:    spec.PayloadPT,
		SequenceNumber: uint16(ordinal),
		Timestamp:      uint32(ordinal) * spec.TSInterval,
		SSRC:           spec.SSRC,
	}
	if spec.KeyEvery > 1 && ordinal%int64(spec.KeyEvery) == 0 {
		hdr.Marker = true
	}
	p := rtp.Packet{Header: hdr, Payload: payload(spec.SSRC, ordinal, 64)}
	raw, err := p.Marshal()
	if err != nil {
		return nil, modelPacketHint{}, err
	}
	hint := modelPacketHint{
		SSRC: spec.SSRC, Sequence: uint16(ordinal),
		Timestamp: hdr.Timestamp, Keyframe: hdr.Marker, Ordinal: ordinal,
	}
	return raw, hint, nil
}

// modelPacketHint is the generator-side ground truth for a packet, kept
// local so the package does not import the runtime model package.
type modelPacketHint struct {
	SSRC      uint32
	Sequence  uint16
	Timestamp uint32
	Keyframe  bool
	Ordinal   int64
}

// BuildSenderReport marshals an RTCP Sender Report for a track at a given
// RTP timestamp and NTP wall-clock instant.
func BuildSenderReport(ssrc uint32, rtpTime uint32, ntp uint64, packets, octets uint32) []byte {
	b := make([]byte, 28)
	b[0] = 2 << 6                         // version 2, no reception blocks
	b[1] = 200                            // PT = SR
	binary.BigEndian.PutUint16(b[2:4], 6) // length in 32-bit words minus 1
	binary.BigEndian.PutUint32(b[4:8], ssrc)
	binary.BigEndian.PutUint64(b[8:16], ntp)
	binary.BigEndian.PutUint32(b[16:20], rtpTime)
	binary.BigEndian.PutUint32(b[20:24], packets)
	binary.BigEndian.PutUint32(b[24:28], octets)
	return b
}

// ntpAt maps a synthetic running time in nanoseconds to an NTP timestamp
// anchored at 2024-01-01 (NTP seconds 3898444800).
func ntpAt(nanos int64) uint64 {
	const ntpEpoch2024 = 3898444800
	secs := ntpEpoch2024 + nanos/1_000_000_000
	frac := uint64(float64(nanos%1_000_000_000) / 1e9 * (1 << 32))
	return uint64(secs)<<32 | frac
}
