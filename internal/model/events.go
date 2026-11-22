package model

import (
	"mediakernel/internal/clock"
)

// Kind discriminates media events carried on the shared timeline.
type Kind uint8

const (
	KindUnknown Kind = iota
	KindAudio
	KindVideo
)

// Event is one timestamped media item as produced by the WAV decoder or the
// RTP parser and consumed by the jitter window.
type Event struct {
	Kind   Kind
	Stream uint32
	Seq    uint16
	// EventTime is the media time mapped onto the unified clock.
	EventTime clock.Ticks
	// Arrival is the simulated network receive time, also on the unified
	// clock; ordering on the wire follows Arrival, ordering for playback
	// follows EventTime.
	Arrival clock.Ticks
	// PayloadHash identifies identical payloads for duplicate detection.
	PayloadHash uint64
	// AudioPCM holds interleaved little-endian 16-bit samples for audio.
	AudioPCM []byte
	// VideoFrame carries the generated video frame descriptor.
	VideoFrame *VideoFrame
}

// Origin marks the provenance of a recovered audio sample.
type Origin uint8

const (
	OriginReal Origin = iota
	OriginRecovered
	OriginUnrecoverable
)

// String renders an origin for reports.
func (o Origin) String() string {
	switch o {
	case OriginReal:
		return "real"
	case OriginRecovered:
		return "recovered"
	case OriginUnrecoverable:
		return "unrecoverable"
	default:
		return "unknown"
	}
}

// VideoFrame is the interactive-side media frame carried inside an event.
type VideoFrame struct {
	FrameID uint32
	// PTS is the presentation time on the unified clock.
	PTS clock.Ticks
	// ContentHash deterministically identifies the frame body.
	ContentHash uint64
}

// AudioBlock is one aligned chunk of PCM produced after loss recovery.
type AudioBlock struct {
	Stream     uint32
	Start      clock.Ticks
	Samples    []int16
	Origins    []Origin
	Channels   int
	SampleRate int
}

// SampleCount returns the number of interleaved sample values in the block.
func (b *AudioBlock) SampleCount() int { return len(b.Samples) }

// AlignedFrame pairs a video frame with the audio block that should be heard
// while it is presented, along with the measured drift in milliseconds.
type AlignedFrame struct {
	Frame    VideoFrame
	Audio    *AudioBlock
	DriftMS  int64
	Degraded bool
	Reason   string
	// OutputOrder is the deterministic replay/playback ordinal.
	OutputOrder int
}
