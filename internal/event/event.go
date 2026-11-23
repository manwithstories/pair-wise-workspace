// Package event defines the media packet event model and the error model
// shared by all pipeline modules.
package event

import (
	"fmt"

	"mediarchive/internal/clock"
)

// CodecID identifies a supported codec family.
type CodecID int

const (
	CodecUnknown CodecID = iota
	CodecOpus
	CodecAAC
)

// String returns the canonical codec name.
func (c CodecID) String() string {
	switch c {
	case CodecOpus:
		return "opus"
	case CodecAAC:
		return "aac"
	default:
		return "unknown"
	}
}

// CodecConfig describes the encoding configuration of one RTP stream.
type CodecConfig struct {
	Codec     CodecID
	ClockRate uint32 // RTP clock rate (Opus: 48000; AAC: sample rate)
	Channels  uint8  // 1 = mono, 2 = stereo
}

// Validate reports whether the configuration is decodable, i.e. whether
// packet durations can be derived from it.
func (c CodecConfig) Validate() error {
	if c.Codec != CodecOpus && c.Codec != CodecAAC {
		return &Error{Op: "codec.validate", Reason: ReasonUndecodableConfig,
			Err: fmt.Errorf("unknown codec id %d", int(c.Codec))}
	}
	if c.ClockRate == 0 {
		return &Error{Op: "codec.validate", Reason: ReasonUndecodableConfig,
			Err: fmt.Errorf("zero clock rate")}
	}
	if c.Channels != 1 && c.Channels != 2 {
		return &Error{Op: "codec.validate", Reason: ReasonUndecodableConfig,
			Err: fmt.Errorf("unsupported channel count %d", c.Channels)}
	}
	return nil
}

// Flags annotate packets as they move through the pipeline.
type Flags uint16

const (
	FlagDuplicate Flags = 1 << iota
	FlagReordered
	FlagLate
	FlagRepairedTS // timestamp was repaired by the repairer
	FlagTruncated  // payload truncated; not a decodable boundary
)

// Packet is one media packet event on the unified time line.
type Packet struct {
	SSRC      uint32
	SeqExt    uint64 // extended 16-bit RTP sequence number
	TSExt     uint64 // extended 32-bit RTP timestamp (codec clock)
	MediaTime clock.Time
	Duration  clock.Time
	Payload   []byte
	Arrival   clock.Time
	Flags     Flags
}

// Reason classifies pipeline errors and quarantine causes.
type Reason string

const (
	ReasonUndecodableConfig Reason = "undecodable_codec_config"
	ReasonDuplicate         Reason = "duplicate"
	ReasonTooLate           Reason = "late_beyond_window"
	ReasonWrapAmbiguous     Reason = "wrap_ambiguous"
	ReasonTruncated         Reason = "truncated_input"
	ReasonNoBoundary        Reason = "no_decodable_boundary"
	ReasonPathTraversal     Reason = "path_traversal"
	ReasonRTPMalformed      Reason = "rtp_malformed"
)

// Error is the shared pipeline error model.
type Error struct {
	Op     string
	Reason Reason
	Err    error
}

// Error formats the error with its operation and reason.
func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s: %v", e.Op, e.Reason, e.Err)
}

// Unwrap returns the underlying cause.
func (e *Error) Unwrap() error { return e.Err }

// Quarantine records an event that could not be processed, with its cause.
type Quarantine struct {
	SSRC   uint32
	SeqExt uint64
	TSExt  uint64
	HasSeq bool // header fields valid (false when the RTP header itself failed)
	Reason Reason
	Detail string
}
