package fixture

import (
	"math/rand"

	"mediaslice/media"
)

// Spec is a deterministic description of one synthetic representation.
type Spec struct {
	ID              string
	Seed            int64
	DurationTicks   media.Ticks
	SegmentTicks    media.Ticks
	VideoRate       uint32 // nominal samples per second
	VideoSampleSize int
	AudioRate       uint32
	AudioSampleSize int
	// RTPStart places the first RTP timestamp near the 32-bit boundary so a
	// wrap happens inside the material.
	RTPStart uint32
	// RTPPPM is the injected linear RTP drift in parts per million.
	RTPPPM int
	// BoundaryJitterTicks is the per-segment VFR jitter applied at every
	// segment boundary, compensated within the following segment.
	BoundaryJitterTicks int
	// JitterSeed drives boundary jitter. All representations of one asset
	// share it so boundaries stay on a common clock; other fields still
	// vary per representation via Seed.
	JitterSeed int64
	// KeyframeEveryN marks a keyframe every N segments; 1 means every
	// boundary (dense), larger values produce sparse keyframes.
	KeyframeEveryN int
	// EditStartTicks is the non-zero edit-list media start in 90 kHz ticks.
	EditStartTicks media.Ticks
}

// DefaultSpec returns the demo fixture specification for one representation.
func DefaultSpec(id string, seed int64, duration, segment media.Ticks) Spec {
	return Spec{
		ID:                  id,
		Seed:                seed,
		DurationTicks:       duration,
		SegmentTicks:        segment,
		VideoRate:           25,
		VideoSampleSize:     128,
		AudioRate:           12000,
		AudioSampleSize:     32,
		RTPStart:            uint32(media.WrapMod - 360000), // wraps ~4s in
		RTPPPM:              40,
		BoundaryJitterTicks: 1,
		KeyframeEveryN:      1,
		EditStartTicks:      media.FromSeconds(1) / 2, // 0.5s non-zero start
		JitterSeed:          seed, // shared across reps: common boundaries
	}
}

// VideoSample is one generated video sample.
type VideoSample struct {
	Index       int
	Offset      uint32 // payload offset inside mdat
	Size        uint32
	StartTick   media.Ticks
	Duration    media.Ticks
	IsKeyframe  bool
}

// AudioSample is one generated audio sample (timescale = AudioRate).
type AudioSample struct {
	Index     int
	Offset    uint32
	Size      uint32
	StartRaw  uint32 // in AudioRate units
	Duration  uint32
}

// Plan holds the fully computed sample tables for one fixture.
type Plan struct {
	Spec   Spec
	Video  []VideoSample
	Audio  []AudioSample
	// MdatPayloadSize is the total sample byte count (excluding mdat header).
	MdatPayloadSize int
	MdatSize        int // including 8-byte header
	videoChunkOffset uint32
	audioChunkOffset uint32
	// RTP holds one minimal RTP packet per video sample.
	RTP [][]byte
}

// rngBytes returns a deterministic pseudo-random byte source for a seed.
func rngBytes(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }
