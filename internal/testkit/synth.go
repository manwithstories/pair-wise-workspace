// Package testkit generates deterministic Matroska/WebM fixtures for tests
// and for the terminal demo. Nothing here is compiled into production paths;
// it only fabricates local sample data.
package testkit

import (
	"math/rand"

	"mediapipe/internal/media"
)

// SyncSample is one frame to place in the fixture.
type SyncSample struct {
	Track    uint64
	DTS      media.Timestamp
	Duration media.Timestamp
	Keyframe bool
	Data     []byte
	// LateBy shifts this frame earlier in write order than its timestamp
	// implies, producing arrival-order violations for the watermark engine.
	LateBy media.Timestamp
}

// Spec describes a complete synthetic stream.
type Spec struct {
	Seed     int64
	DocType  string
	Tracks   []media.Track
	Duration media.Timestamp
	// FramesPerTrack is the number of frames generated per track over
	// Duration.
	FramesPerTrack int
	// ClusterSpan groups frames into clusters covering this many nanoseconds.
	ClusterSpan media.Timestamp
	// UnknownSegment and UnknownCluster choose streaming-style headers with
	// the all-ones size VINT.
	UnknownSegment bool
	UnknownCluster bool
	// Lacing requests a lacing mode for audio-ish tracks: 0 none, 1 xiph,
	// 2 fixed, 3 EBML.
	Lacing byte
	// LaceFrames groups this many frames per block when Lacing is nonzero.
	LaceFrames int
	// ClusterCRC writes an EBML CRC-32 element into every cluster.
	ClusterCRC bool
	// LateFrames perturbs this many frames with LateBy values.
	LateFrames int
	// TruncateAt, when positive, cuts the encoded file at that byte offset.
	TruncateAt int64
}

// SampleFile is an encoded fixture plus the timing facts tests assert on.
type SampleFile struct {
	Bytes   []byte
	Spec    Spec
	Late    []SyncSample
	Samples []SyncSample
}

// DefaultTracks returns a video, audio, and subtitle track with deterministic
// codec-private payloads. Track 1 is AVC video, 2 is Opus audio, 3 is UTF-8
// subtitles.
func DefaultTracks(rng *rand.Rand) []media.Track {
	avcC := []byte{
		0x01, 0x64, 0x00, 0x28, 0xFF, 0xE1, 0x00, 0x04,
		0x67, 0x64, 0x00, 0x28, 0x01, 0x00, 0x04, 0x68,
		0xEE, 0x3C, 0x80,
	}
	return []media.Track{
		{
			ID: 1, UID: 0xA1, Type: media.TrackVideo, Codec: media.CodecAVC,
			Language: "eng", DefaultDuration: 40 * media.Millisecond,
			Width: 1280, Height: 720, CodecPrivate: avcC,
		},
		{
			ID: 2, UID: 0xA2, Type: media.TrackAudio, Codec: media.CodecOpus,
			Language: "eng", DefaultDuration: 20 * media.Millisecond,
			Channels: 2, SampleRate: 48000, CodecPrivate: []byte("OpusHead\x01\x02"),
		},
		{
			ID: 3, UID: 0xA3, Type: media.TrackSubtitle, Codec: media.CodecText,
			Language: "deu", DefaultDuration: 500 * media.Millisecond,
		},
	}
}

// Generate builds the sample set: a clean stream, a late-arrival variant, a
// truncated variant, and an interrupted variant whose prefix is a clean
// resumable point.
func Generate(seed int64) (clean, late, truncated, interrupted SampleFile) {
	base := Spec{
		Seed:           seed,
		DocType:        "matroska",
		Duration:       12 * media.Second,
		ClusterSpan:    2 * media.Second,
		UnknownSegment: true,
		UnknownCluster: false,
		Lacing:         2,
		LaceFrames:     3,
		ClusterCRC:     true,
	}
	rng := rand.New(rand.NewSource(seed))
	base.Tracks = DefaultTracks(rng)
	base.FramesPerTrack = 60
	step := base.Duration / media.Timestamp(base.FramesPerTrack)
	for i := range base.Tracks {
		base.Tracks[i].DefaultDuration = step
	}
	clean = Build(base)

	lateSpec := base
	lateSpec.LateFrames = 6
	late = Build(lateSpec)

	truncSpec := base
	truncSpec.TruncateAt = int64(len(clean.Bytes)) * 3 / 4
	truncated = Build(truncSpec)

	interruptSpec := base
	interruptSpec.UnknownCluster = true
	interrupted = Build(interruptSpec)
	return clean, late, truncated, interrupted
}

// SamplesFor builds the frame list for a spec. Frames are interleaved by DTS
// with a stable tie breaker on track number.
func SamplesFor(spec Spec) []SyncSample {
	if spec.FramesPerTrack <= 0 {
		return nil
	}
	rng := rand.New(rand.NewSource(spec.Seed ^ 0x5EED))
	step := spec.Duration / media.Timestamp(spec.FramesPerTrack)
	if step <= 0 {
		step = media.Millisecond
	}
	var out []SyncSample
	for _, t := range spec.Tracks {
		size := 96
		switch t.Type {
		case media.TrackVideo:
			size = 1400
		case media.TrackAudio:
			size = 160
		case media.TrackSubtitle:
			size = 48
		}
		for i := 0; i < spec.FramesPerTrack; i++ {
			data := make([]byte, size)
			rng.Read(data)
			// Embed the track and frame index for easy debugging.
			data[0] = byte(t.ID)
			data[1] = byte(i)
			data[2] = byte(i >> 8)
			out = append(out, SyncSample{
				Track:    t.ID,
				DTS:      media.Timestamp(i) * step,
				Duration: step,
				Keyframe: t.Type != media.TrackVideo || i%int(media.Second/step) == 0,
				Data:     data,
			})
		}
	}
	sortSamples(out)
	// Perturb a few frames to arrive late. LateBy moves the timestamp earlier
	// than the surrounding write order without changing the frame content.
	if spec.LateFrames > 0 && len(out) > spec.LateFrames {
		for k := 0; k < spec.LateFrames; k++ {
			idx := len(out) / 4 * (k + 1) / (spec.LateFrames + 1) * 3
			if idx <= 0 || idx >= len(out) {
				continue
			}
			out[idx].LateBy = 1200 * media.Millisecond
		}
	}
	return out
}

func sortSamples(s []SyncSample) {
	// Insertion sort keeps this package dependency-light and fully stable.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && lessSample(s[j], s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func lessSample(a, b SyncSample) bool {
	if a.DTS != b.DTS {
		return a.DTS < b.DTS
	}
	return a.Track < b.Track
}

// LateSamples returns the frames that carry an arrival perturbation.
func LateSamples(spec Spec) []SyncSample {
	var out []SyncSample
	for _, s := range SamplesFor(spec) {
		if s.LateBy != 0 {
			out = append(out, s)
		}
	}
	return out
}
