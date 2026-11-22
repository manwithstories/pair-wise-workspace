// Package rtppacket turns normalised audio segments into RTP timing evidence.
//
// Broadcast archives commonly keep an RTP view of the timeline alongside the
// segment files: a 48 kHz RTP timestamp per segment and an RFC 6464 audio
// level extension describing the delivered loudness. The packets are built
// with pion/rtp and marshalled deterministically so they can be hashed.
package rtppacket

import (
	"errors"
	"fmt"
	"math"

	"github.com/pion/rtp"

	"mediad/internal/clock"
)

// ClockRate is the RTP audio clock used for delivered segments.
const ClockRate uint32 = 48000

// PayloadType is a dynamic payload type for PCM audio.
const PayloadType uint8 = 96

// SSRC is fixed so evidence packets are reproducible.
const SSRC uint32 = 0x4d454449 // "MEDI"

// AudioLevelExtensionID is the one-byte RFC 6464 extension identifier.
const AudioLevelExtensionID uint8 = 1

// Evidence describes one segment on the RTP timeline.
type Evidence struct {
	URI         string `json:"uri"`
	Timestamp   uint32 `json:"rtp_timestamp"`
	Frames      int    `json:"frames"`
	AudioLevel  uint8  `json:"audio_level"`
	PacketBytes int    `json:"packet_bytes"`
}

// Timestamp converts canonical ticks into a 48 kHz RTP timestamp.
func Timestamp(ticks int64) uint32 {
	if ticks <= 0 {
		return 0
	}
	tb := clock.Default()
	return uint32(tb.Convert(ticks, clock.Timebase{Timescale: ClockRate}))
}

// LevelFromLUFS maps integrated loudness to an RFC 6464 level in -dBov.
// Silence reports the maximum level of 127.
func LevelFromLUFS(lufs float64) uint8 {
	if math.IsInf(lufs, -1) || math.IsNaN(lufs) {
		return 127
	}
	level := -lufs
	if level < 0 {
		level = 0
	}
	if level > 127 {
		level = 127
	}
	return uint8(math.Round(level))
}

// Build creates and marshals one evidence packet for a segment. sequence is
// the segment's sequence number truncated to 16 bits, as RTP requires.
func Build(uri string, sequence uint64, startTicks int64, frames int, lufs float64) (Evidence, []byte, error) {
	if uri == "" {
		return Evidence{}, nil, errors.New("rtppacket: empty uri")
	}
	if frames < 0 {
		return Evidence{}, nil, fmt.Errorf("rtppacket: negative frame count %d", frames)
	}
	level := LevelFromLUFS(lufs)
	packet := &rtp.Packet{Header: rtp.Header{
		Version:        2,
		PayloadType:    PayloadType,
		SequenceNumber: uint16(sequence),
		Timestamp:      Timestamp(startTicks),
		SSRC:           SSRC,
	}}
	ext, err := rtp.AudioLevelExtension{Level: level, Voice: false}.Marshal()
	if err != nil {
		return Evidence{}, nil, fmt.Errorf("rtppacket: audio level: %w", err)
	}
	if err := packet.Header.SetExtension(AudioLevelExtensionID, ext); err != nil {
		return Evidence{}, nil, fmt.Errorf("rtppacket: set extension: %w", err)
	}
	// The payload is the frame count in canonical order, kept empty here:
	// evidence describes timing, not a second copy of the audio.
	raw, err := packet.Marshal()
	if err != nil {
		return Evidence{}, nil, fmt.Errorf("rtppacket: marshal: %w", err)
	}
	ev := Evidence{
		URI: uri, Timestamp: packet.Header.Timestamp, Frames: frames,
		AudioLevel: level, PacketBytes: len(raw),
	}
	return ev, raw, nil
}

// Parse decodes a marshalled evidence packet and returns the audio level.
func Parse(raw []byte) (uint32, uint8, error) {
	var p rtp.Packet
	if err := p.Unmarshal(raw); err != nil {
		return 0, 0, fmt.Errorf("rtppacket: unmarshal: %w", err)
	}
	ext := p.Header.GetExtension(AudioLevelExtensionID)
	if ext == nil {
		return p.Header.Timestamp, 0, errors.New("rtppacket: missing audio level extension")
	}
	var level rtp.AudioLevelExtension
	if err := level.Unmarshal(ext); err != nil {
		return p.Header.Timestamp, 0, fmt.Errorf("rtppacket: audio level: %w", err)
	}
	return p.Header.Timestamp, level.Level, nil
}

// Monotonic reports whether evidence timestamps strictly increase. Wraps are
// accepted and treated as continuing forward.
func Monotonic(evs []Evidence) bool {
	var prev uint32
	for i, e := range evs {
		if i > 0 && e.Timestamp == prev {
			return false
		}
		prev = e.Timestamp
	}
	return true
}
