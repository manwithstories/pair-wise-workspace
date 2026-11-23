// Package demo assembles the self-generated multi-track sample media used by
// the command-line adapter. Every byte derives from the seed so results are
// reproducible; no external files are read.
package demo

import (
	"encoding/json"
	"fmt"

	"mediascan/internal/mp4"
)

// Spec is the deterministic description of a demo run.
type Spec struct {
	Seed          int64
	Tracks        int
	Frames        int
	CorruptAt     int64
	Timescale     int64
	KeyEvery      int
	KeyGapLimit   int64 // ticks
	DriftLimit    int64 // ticks
	SegmentFrames int64
}

// CanonicalParams serializes the result-influencing parameters to stable
// bytes for checkpoint and report parameter digests.
func (s Spec) CanonicalParams() []byte {
	b, _ := json.Marshal(struct {
		Seed          int64 `json:"seed"`
		Tracks        int   `json:"tracks"`
		Frames        int   `json:"frames"`
		CorruptAt     int64 `json:"corrupt_at"`
		Timescale     int64 `json:"timescale"`
		KeyEvery      int   `json:"key_every"`
		KeyGapLimit   int64 `json:"key_gap_limit"`
		DriftLimit    int64 `json:"drift_limit"`
		SegmentFrames int64 `json:"segment_frames"`
	}{
		Seed: s.Seed, Tracks: s.Tracks, Frames: s.Frames,
		CorruptAt: s.CorruptAt, Timescale: s.Timescale, KeyEvery: s.KeyEvery,
		KeyGapLimit: s.KeyGapLimit, DriftLimit: s.DriftLimit,
		SegmentFrames: s.SegmentFrames,
	})
	return b
}

// Validate enforces the structural limits and sane ranges.
func (s Spec) Validate() error {
	if s.Tracks <= 0 || s.Tracks > mp4.DefaultLimits().MaxTracks {
		return fmt.Errorf("tracks %d out of range", s.Tracks)
	}
	if s.Frames <= 0 || int64(s.Frames) > mp4.DefaultLimits().MaxSamples {
		return fmt.Errorf("frames %d out of range", s.Frames)
	}
	if s.Timescale <= 0 || s.Timescale > 100_000_000 {
		return fmt.Errorf("timescale %d invalid", s.Timescale)
	}
	if s.SegmentFrames <= 0 {
		return fmt.Errorf("segment frames must be positive")
	}
	return nil
}

// Media is the assembled in-memory sample and its deterministic byte digest.
type Media struct {
	Spec   Spec
	Bytes  []byte
	Digest string
}

// Build renders the MP4 fixture and derives its input digest. The fixture is
// structurally valid; frame-localized corruption is modeled separately by
// the engine so an intact container can still be scanned end to end.
func Build(s Spec) (*Media, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	// Cyclic VFR durations at the track timescale (two alternating deltas),
	// so every track is genuinely variable frame rate.
	vfr := []uint32{
		uint32(s.Timescale / 30),
		uint32(s.Timescale / 24),
	}
	data := mp4.BuildFixture(mp4.FixtureSpec{
		Tracks:       s.Tracks,
		Frames:       s.Frames,
		Timescale:    s.Timescale,
		VFRDurations: vfr,
		KeyEvery:     s.KeyEvery,
	})
	return &Media{Spec: s, Bytes: data, Digest: hexEncode(sum256(data))}, nil
}

// FrameOffset returns the mdat byte offset of a frame on a given track using
// the same 16-byte sample geometry the fixture builder writes.
func FrameOffset(m *Media, track, frame int) int64 {
	const sampleSize = int64(16)
	// Locate mdat payload start from the actual box layout rather than
	// assuming it, so the value is independently verifiable.
	roots, err := mp4.ScanBoxes(m.Bytes, mp4.DefaultLimits())
	if err != nil {
		return -1
	}
	mdat := mp4.First(roots, mp4.BoxMdat)
	if mdat == nil {
		return -1
	}
	return mdat.Payload +
		(int64(track)*int64(m.Spec.Frames)+int64(frame))*sampleSize
}
