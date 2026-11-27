// Package ingest produces the packet event stream that the rest of the
// pipeline consumes.  Events are generated locally from a fixed seed; no
// external media is referenced.  Packet sizes and durations are chosen so
// every sample is reproducible byte for byte.
package ingest

import (
	"fmt"
	"time"

	"mediacheck/internal/model"
)

// Hard bounds applied to every generated or read stream.  They are exported
// so tests and the command line can assert on the same numbers.
const (
	// MinPacketBytes and MaxPacketBytes bound a single payload.
	MinPacketBytes = 4
	MaxPacketBytes = 1500

	// MaxPackets bounds the total event count of one run.
	MaxPackets = 200_000

	// MaxDuration bounds the media time a run may describe.
	MaxDuration = 30 * time.Minute

	// MaxFrameBytes bounds an H.264 frame assembled from slices.
	MaxFrameBytes = 8 * 1024 * 1024

	// AudioSampleRate and channel count of the generated Opus stream.
	AudioSampleRate = 48000
	AudioChannels   = 2

	// VideoWidth, VideoHeight and frame rate of the generated H.264 stream.
	VideoWidth  = 320
	VideoHeight = 240
	VideoFPSNum = 25
	VideoFPSDen = 1
)

// Config describes a generated fixture stream.
type Config struct {
	// Seed drives every pseudo-random decision.  Equal seeds and configs
	// must produce identical packet bytes.
	Seed int64

	// VideoFrames is the number of video access units to emit.  Audio
	// packets continue until they cover the same media span.
	VideoFrames int

	// OpusDurationProfile selects per-packet audio durations.  Empty means
	// a steady 20 ms.  "mixed" alternates 20 ms, 10 ms and 60 ms packets.
	OpusDurationProfile string

	// KeyframeInterval is the GOP length in frames; frames at indices
	// divisible by it are marked as sync samples.
	KeyframeInterval int

	// MaxPacketBytes caps generated payloads and is clamped to
	// [MinPacketBytes, MaxPacketBytes].
	MaxPacketBytes int

	// Codec selects the generated audio codec.  The zero value generates
	// Opus; CodecAAC generates ADTS-framed AAC-LC.
	Codec model.Codec
}

// DefaultConfig is the fixture used by the demo command.
func DefaultConfig(seed int64) Config {
	return Config{
		Seed:             seed,
		VideoFrames:      600,
		KeyframeInterval: 25,
		MaxPacketBytes:   MaxPacketBytes,
	}
}

// Validate clamps and checks the configuration, returning a copy safe to use.
func (c Config) Validate() (Config, error) {
	if c.VideoFrames <= 0 {
		return c, fmt.Errorf("ingest: VideoFrames must be positive: %w", model.ErrLimitExceeded)
	}
	audio, _ := c.AudioPacketCount()
	total := int64(c.VideoFrames) + audio
	if total > MaxPackets {
		return c, fmt.Errorf("ingest: %d packets exceeds limit %d: %w", total, MaxPackets, model.ErrLimitExceeded)
	}
	if c.KeyframeInterval <= 0 {
		c.KeyframeInterval = 25
	}
	if c.MaxPacketBytes <= 0 {
		c.MaxPacketBytes = MaxPacketBytes
	}
	if c.MaxPacketBytes < MinPacketBytes {
		c.MaxPacketBytes = MinPacketBytes
	}
	if c.MaxPacketBytes > MaxPacketBytes {
		c.MaxPacketBytes = MaxPacketBytes
	}
	switch c.OpusDurationProfile {
	case "", "steady", "mixed":
	default:
		return c, fmt.Errorf("ingest: unknown OpusDurationProfile %q", c.OpusDurationProfile)
	}
	if c.Codec == model.CodecUnknown {
		c.Codec = model.CodecOpus
	}
	if c.Codec != model.CodecOpus && c.Codec != model.CodecAAC {
		return c, fmt.Errorf("ingest: unsupported fixture codec %q", c.Codec)
	}
	if c.OpusDurationProfile == "steady" {
		c.OpusDurationProfile = ""
	}
	span := c.Span()
	if span > MaxDuration {
		return c, fmt.Errorf("ingest: duration %s exceeds limit %s: %w", span, MaxDuration, model.ErrLimitExceeded)
	}
	return c, nil
}

// AudioPacketCount estimates how many audio packets are needed to cover the
// configured video span.  Mixed durations average 25 ms per packet.
func (c Config) AudioPacketCount() (int64, time.Duration) {
	span := c.Span()
	per := int64(20 * time.Millisecond)
	if c.OpusDurationProfile == "mixed" {
		per = int64(25 * time.Millisecond)
	}
	n := int64(span) / per
	if n < 1 {
		n = 1
	}
	return n, span
}

// Span is the media duration described by the configured video frame count.
func (c Config) Span() time.Duration {
	return time.Duration(c.VideoFrames) * time.Second * time.Duration(VideoFPSDen) / time.Duration(VideoFPSNum)
}
