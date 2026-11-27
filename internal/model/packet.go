package model

import (
	"fmt"
	"strings"

	"mediacheck/internal/clock"
)

// Codec identifies an elementary stream encoding.
type Codec uint8

const (
	CodecUnknown Codec = iota
	CodecOpus
	CodecAAC
	CodecH264
)

// String returns the FFmpeg short codec name.
func (c Codec) String() string {
	switch c {
	case CodecOpus:
		return "opus"
	case CodecAAC:
		return "aac"
	case CodecH264:
		return "h264"
	default:
		return "unknown"
	}
}

// ParseCodec accepts codec names used by command line flags.
func ParseCodec(s string) (Codec, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "opus":
		return CodecOpus, nil
	case "aac":
		return CodecAAC, nil
	case "h264", "avc":
		return CodecH264, nil
	default:
		return CodecUnknown, fmt.Errorf("model: unknown codec %q", s)
	}
}

// IsAudio reports whether the codec carries PCM-derived audio.
func (c Codec) IsAudio() bool { return c == CodecOpus || c == CodecAAC }

// StreamInfo describes one elementary stream, including the codec
// configuration a muxer needs.  Extradata is the codec-private header:
// OpusHead for Opus, AudioSpecificConfig for AAC, avcC for H.264.
type StreamInfo struct {
	Track      Track
	Codec      Codec
	Timebase   clock.Timebase
	SampleRate int
	Channels   int
	Width      int
	Height     int
	FrameRate  clock.Timebase // tick timebase of one frame; audio leaves zero
	Extradata  []byte
}

// Packet is the unified event passed between ingest, inspect, recover and
// remux.  PTS and DTS are in Timebase ticks; Slot is the same instant
// expressed in nanoseconds for fault merging across tracks.
type Packet struct {
	Track    Track
	Codec    Codec
	Sequence uint32
	PTS      int64
	DTS      int64
	Duration int64 // ticks
	Keyframe bool
	Data     []byte
	Timebase clock.Timebase
	Slot     Slot
	// Corrupt is set by inspect when the codec structure could not be
	// parsed.  Such a packet must not be muxed as-is: its slot is reported
	// as damaged, and for audio recover fills it instead.
	Corrupt bool
}

// End returns the exclusive end timestamp in ticks.
func (p Packet) End() int64 { return p.PTS + p.Duration }

// Clone returns a deep copy, used whenever a packet may be merged and then
// mutated independently of the source buffer.
func (p Packet) Clone() Packet {
	q := p
	q.Data = append([]byte(nil), p.Data...)
	return q
}

// Fixture is a complete deterministic sample stream: stream descriptors plus
// packets in playback order.
type Fixture struct {
	Streams []StreamInfo
	Packets []Packet
}

// TrackInfo returns the descriptor for track, if present.
func (f *Fixture) TrackInfo(t Track) (StreamInfo, bool) {
	for _, s := range f.Streams {
		if s.Track == t {
			return s, true
		}
	}
	return StreamInfo{}, false
}

// PacketBytes returns the total payload size of all packets.
func (f *Fixture) PacketBytes() int64 {
	var n int64
	for _, p := range f.Packets {
		n += int64(len(p.Data))
	}
	return n
}
