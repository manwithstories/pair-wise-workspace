// Package testdata builds deterministic synthetic MP4/RTP fixtures. All data
// is generated from an integer seed: no external files are ever read, and the
// same parameters always yield byte-identical boxes, fingerprints and
// manifests. Fixture errors carry only offsets/timestamps, never payload.
package testdata

import (
	"time"

	"mediacq/internal/clock"
)

// Kind identifies a media track or a timed-metadata family.
type Kind string

const (
	KindVideo    Kind = "video"
	KindAudio    Kind = "audio"
	KindSubtitle Kind = "subtitle" // WebVTT-style cue
	KindChapter  Kind = "chapter"  // chapter marker
	KindID3      Kind = "id3"      // ID3v2-style tag event
	KindEmsg     Kind = "emsg"     // DASH event message
	KindSEI      Kind = "sei"      // H.264 SEI user data
)

// Sample is one generated media sample. Payload is synthetic, deterministic
// content; Keyframe marks random-access points on the video track.
type Sample struct {
	DurationTicks uint32 // duration in the track timescale
	Keyframe      bool
	Payload       []byte
}

// TimedEvent is one normalized metadata occurrence before serialization.
type TimedEvent struct {
	Kind       Kind
	Time       clock.Time // position on the unified timeline
	Duration   time.Duration
	ID         uint32
	Timebase   clock.Rate // originating clock, deliberately varied per family
	RawTicks   int64      // timestamp expressed in Timebase
	PayloadLen int
}

// TrackSpec fully describes one media track to generate.
type TrackSpec struct {
	Kind       Kind
	TrackID    uint32
	Timescale  uint32
	Samples    []Sample
	Rate       clock.Rate // rational rate used by RTP-style event conversion
	IsPCM      bool
	Channels   uint16
	SampleRate uint32
}

// DurationTicks sums sample durations.
func (t *TrackSpec) DurationTicks() uint64 {
	var n uint64
	for _, s := range t.Samples {
		n += uint64(s.DurationTicks)
	}
	return n
}

// Params controls deterministic fixture generation.
type Params struct {
	Seed         int64
	VideoTS      uint32 // video timescale (Hz)
	AudioTS      uint32 // audio timescale (Hz)
	VideoFrames  int    // number of video samples
	KeyframeEvery int   // <=1 means every frame is a keyframe
	KeyframeGap  int    // when >0, force this many consecutive non-keyframes
	VFR          bool   // variable frame rate via deterministic jitter
	TargetFPS    int
	AudioSamples int
	SilenceEvery int   // insert a silent audio span every N samples, 0=off
	SilenceLen   int   // span length in audio samples
	PCMBytes     int   // bytes per PCM frame per channel
	DriftPPM     float64
	WrapAt       uint32 // emit RTP wrap behavior once raw ts reaches this value
	// Corruption switches, consumed by the MP4 writer and RTP stream builder.
	CorruptBox   bool
	Truncate     bool
	MissingKey   bool
}

// DefaultParams returns the fixture sizing shared by tests and the demo.
func DefaultParams(seed int64) Params {
	return Params{
		Seed:          seed,
		VideoTS:       30000,
		AudioTS:       48000,
		VideoFrames:   90,  // ~3 s at 30 fps
		KeyframeEvery: 15,  // GOP of 0.5 s
		TargetFPS:     30,
		AudioSamples:  144, // ~3 s at 48 frames-per-packet cadence
		SilenceEvery:  40,
		SilenceLen:    12,
		PCMBytes:      4,
	}
}

// Fixture is a fully generated, self-describing input set.
type Fixture struct {
	Params Params
	Video  TrackSpec
	Audio  TrackSpec
	Events []TimedEvent
	// MP4 is the serialized container (may be corrupted per Params).
	MP4 []byte
	// RTP holds packetized RTP timestamps (raw 32-bit) in emission order.
	RTP []RTPPacket
	// ExpectedSilences are the ground-truth silent spans in unified time.
	ExpectedSilences []clock.Range
	// ExpectedKeyframes are video sample indices that are random-access.
	ExpectedKeyframes []int
}

// RTPPacket mirrors only the header fields the pipeline consumes; payloads
// stay synthetic. It is parsed through github.com/pion/rtp in ingest tests.
type RTPPacket struct {
	Timestamp   uint32
	Sequence    uint16
	SSRC        uint32
	Marker      bool
	PayloadType uint8
	Payload     []byte
}
