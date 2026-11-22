// Package fixture builds fully synthetic broadcast delivery packages:
// a self-consistent MP4-like box stream, RTP packet sequences and
// timed metadata events. Normal, damaged and boundary cases are all
// produced in code without any external media.
package fixture

import (
	"bytes"
	"encoding/binary"

	"mediacheck/internal/inspect"
)

// Profile selects a generator scenario.
type Profile string

const (
	ProfileNormal     Profile = "normal"
	ProfileBroadcast  Profile = "broadcast"
	ProfileTruncated  Profile = "truncated"
	ProfileCorrupt    Profile = "corrupt"
	ProfileNoKeyframe Profile = "no_keyframe"
	ProfileDrift      Profile = "drift"
	ProfileBoundary   Profile = "boundary"
)

// Config parameterizes generation.
type Config struct {
	Profile    Profile
	DurationMs int64  // media duration
	LossBurst  int    // consecutive RTP sequence numbers dropped
	Seed       int64  // deterministic generation seed
	VideoTS    uint32 // video timescale
	AudioTS    uint32 // audio timescale
	DriftMs    int64  // injected cross-track clock drift
}

// DefaultConfig returns the standard broadcast-shaped scenario.
func DefaultConfig() Config {
	return Config{
		Profile: ProfileBroadcast, DurationMs: 2000, LossBurst: 12,
		Seed: 20261123, VideoTS: 90000, AudioTS: 48000, DriftMs: 0,
	}
}

// Sample is a generated media sample with ground-truth flags.
type Sample = inspect.Sample

// Span is a half-open millisecond range.
type Span struct {
	StartMs int64
	EndMs   int64
}

// GroundTruth names an injected defect for test assertions.
type GroundTruth struct {
	Kind    string
	Track   string
	StartMs int64
	EndMs   int64
}

const (
	videoTrack = "video"
	audioTrack = "audio"
	gopMs      = 500 // distance between sync samples
	frameMs    = 100 // per-sample frame duration (5 frames per GOP)
)

// box writes a minimal four-cc box with an opaque payload.
func box(typ string, payload ...[]byte) []byte {
	var b bytes.Buffer
	var body bytes.Buffer
	for _, p := range payload {
		body.Write(p)
	}
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr, uint32(8+body.Len()))
	copy(hdr[4:8], []byte(typ))
	b.Write(hdr)
	b.Write(body.Bytes())
	return b.Bytes()
}

// container nests child boxes inside one container box.
func container(typ string, children ...[]byte) []byte {
	var payload bytes.Buffer
	for _, c := range children {
		payload.Write(c)
	}
	return box(typ, payload.Bytes())
}
