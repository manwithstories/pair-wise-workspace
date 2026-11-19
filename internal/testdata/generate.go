// Package testdata generates the self-contained RTP/RTCP fixtures used by the
// demo and the test suite. Every byte is derived from a fixed seed plus the
// packet index, so a given seed always produces identical datagrams and
// identical downstream hashes. No external files are referenced.
package testdata

import (
	"math"

	"github.com/pion/rtp"
)

// TrackKind distinguishes the two generated track types.
type TrackKind uint8

// Track kinds.
const (
	TrackAudio TrackKind = iota
	TrackVideo
)

// CorruptionKind enumerates the deterministic faults a fixture can embed.
type CorruptionKind uint8

// Supported corruptions, each located by sequence index.
const (
	// CorruptTruncated cuts the datagram short mid-payload.
	CorruptTruncated CorruptionKind = iota
	// CorruptBadVersion clears the RTP version bits.
	CorruptBadVersion
	// CorruptBadRTCP injects an unparseable RTCP datagram.
	CorruptBadRTCP
	// CorruptDuplicateConflict sends the same sequence twice, payloads differ.
	CorruptDuplicateConflict
)

// Corruption declares one embedded fault.
type Corruption struct {
	Track uint32
	// SeqIndex is the zero-based clean-packet index on that track; for
	// duplicate faults it marks the sequence that gets re-sent.
	SeqIndex uint64
	Kind     CorruptionKind
}

// TrackSpec configures one generated track.
type TrackSpec struct {
	SSRC        uint32
	Kind        TrackKind
	ClockRate   uint32
	Packets     int
	SeqStart    uint16  // first RTP sequence number; wraps naturally at 65535
	LossRate    float64 // fraction of clean packets omitted from the stream
	JitterSwaps int     // number of adjacent arrival-order swaps
}

// Spec fully describes a generated fixture.
type Spec struct {
	Seed          int64
	DurationNS    int64
	AudioSSRC     uint32
	VideoSSRC     uint32
	AudioSeqStart uint16
	VideoSeqStart uint16
	AudioPkts     int
	VideoPkts     int
	LossRate      float64
	JitterSwaps   int
	KeyframeEvery int // video packets between keyframe markers
	Corruptions   []Corruption
}

// Datagram is one raw RTP/RTCP message with its source offset and identity.
type Datagram struct {
	Track      uint32 // 0 marks RTCP/control datagrams
	IsRTCP     bool
	Raw        []byte
	Offset     int64
	Arrival    int64
	SeqIndex   uint64 // clean sequence index, valid for RTP
	Timestamp  uint32 // RTP timestamp, valid for RTP
	Corrupt    CorruptionKind
	HasCorrupt bool
}

// Fixture is the generated input set plus a manifest for assertions.
type Fixture struct {
	Datagrams   []Datagram
	AudioSpec   TrackSpec
	VideoSpec   TrackSpec
	TotalOffset int64
	// CleanAudio / CleanVideo count the uncorrupted RTP packets per track,
	// so tests can reason about what should survive processing.
	CleanAudio int
	CleanVideo int
}

// DefaultSpec builds the baseline spec used by the demo and verify commands.
func DefaultSpec(seed int64) Spec {
	return Spec{
		Seed:          seed,
		DurationNS:    4_000_000_000,
		AudioSSRC:     0xA1100001,
		VideoSSRC:     0x71DE0001,
		AudioPkts:     400, // 400 * 10 ms = 4 s
		VideoPkts:     100, // 100 * 40 ms = 4 s
		KeyframeEvery: 10,
	}
}

// marshalRTP builds one RTP datagram from explicit fields.
func marshalRTP(ssrc uint32, seq uint16, ts uint32, pt uint8, marker bool, payload []byte) []byte {
	pkt := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         marker,
			PayloadType:    pt,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           ssrc,
		},
		Payload: payload,
	}
	raw, err := pkt.Marshal()
	if err != nil {
		panic("testdata: rtp marshal: " + err.Error())
	}
	return raw
}

// audioPayload synthesizes a deterministic 10 ms 48 kHz mono tone block
// (float32 samples) from the packet index and seed.
func audioPayload(seed int64, idx uint64) []byte {
	const samples = 160 // 10 ms at 48 kHz
	buf := make([]byte, samples*4)
	for i := 0; i < samples; i++ {
		v := 0.25 * triWave(uint64(seed), idx, uint64(i))
		putFloat32(buf[i*4:], v)
	}
	return buf
}

// videoPayload synthesizes a deterministic H.264-like payload. The first byte
// encodes the frame type: 0x65 for a keyframe, 0x61 for an inter frame.
func videoPayload(seed int64, idx uint64, keyframe bool) []byte {
	size := 200 + int(hashMix(uint64(seed), idx)%120)
	buf := make([]byte, size)
	if keyframe {
		buf[0] = 0x65
	} else {
		buf[0] = 0x61
	}
	for i := 1; i < size; i++ {
		buf[i] = byte(hashMix(uint64(seed), idx*7+uint64(i)) % 256)
	}
	return buf
}

// triWave is a deterministic triangle in [-1, 1] over sample position.
func triWave(seed, pkt, sample uint64) float32 {
	const period uint64 = 64
	phase := (hashMix(seed, pkt)*13 + sample) % (2 * period)
	if phase < period {
		return float32(phase)/float32(period)*2 - 1
	}
	return float32(2*period-phase)/float32(period)*2 - 1
}

// hashMix is a splitmix-style 64-bit mix for deterministic payload content.
func hashMix(seed, x uint64) uint64 {
	z := x + seed + 0x9E3779B97F4A7C15
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func putFloat32(b []byte, v float32) {
	bits := math.Float32bits(v)
	b[0], b[1], b[2], b[3] = byte(bits), byte(bits>>8), byte(bits>>16), byte(bits>>24)
}
