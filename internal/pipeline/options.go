// Package pipeline wires ingest, RTP recovery, audio mapping, sync,
// transactions and verification into one idempotent delivery run.
package pipeline

import (
	"fmt"

	"github.com/example/mediad/internal/ingest"
)

// MaxSegmentBytes bounds a single segment's in-memory payload to
// 256 MiB as required by the delivery constraints.
const MaxSegmentBytes = 256 << 20

// Options controls one demo/processing run.
type Options struct {
	// Root is the workspace boundary every path must stay inside.
	// Defaults to the current working directory when empty.
	Root       string
	WorkDir    string
	Seed       int64
	Loss       int    // burst packets initially withheld, then late
	CrashAfter string // "" | "write" | "verify" | "commit"
	// VideoSamples fixes the number of accepted segments.
	VideoSamples uint32
}

// Metrics holds the terminal-reported counters.
type Metrics struct {
	Segments          int     `json:"segments"`
	HashMatched       int     `json:"hash_matched"`
	HashTotal         int     `json:"hash_total"`
	RoundTripRate     float64 `json:"roundtrip_rate"`
	KeyframeHitRate   float64 `json:"keyframe_hit_rate"`
	RecoveredPackets  int     `json:"recovered_packets"`
	Unrecoverable     int     `json:"unrecoverable_packets"`
	AVOffsetMicros    int64   `json:"av_offset_micros"`
	RollbackCount     int     `json:"rollback_count"`
	RejectedSegments  int     `json:"rejected_segments"`
	MetadataTimeRatio float64 `json:"metadata_time_ratio"`
}

// ReportLine formats the acceptance fields in key=value style.
func (m Metrics) ReportLine() string {
	return fmt.Sprintf(
		"roundtrip_rate=%.3f keyframe_hit_rate=%.3f segments=%d "+
			"segment_hash_match=%d/%d recovered_packets=%d av_offset_ms=%d "+
			"rollback_count=%d rejected_segments=%d metadata_time_ratio=%.3f",
		m.RoundTripRate, m.KeyframeHitRate, m.Segments,
		m.HashMatched, m.HashTotal, m.RecoveredPackets,
		m.AVOffsetMicros/1000, m.RollbackCount, m.RejectedSegments,
		m.MetadataTimeRatio)
}

// FixtureSet is the generated input material for one run.
type FixtureSet struct {
	GoodMP4      string
	GoodMXF      string
	DamagedMP4   string
	BadLayoutMP4 string
}

// generateFixtures creates every self-contained sample under workDir.
func generateFixtures(dir string, samples uint32) (FixtureSet, error) {
	fs := FixtureSet{
		GoodMP4:      dir + "/input/cam-good.mp4",
		GoodMXF:      dir + "/input/cam-good.mxf",
		DamagedMP4:   dir + "/input/cam-truncated.mp4",
		BadLayoutMP4: dir + "/input/cam-badlayout.mp4",
	}
	base := ingest.FixtureSpec{
		Width: 1920, Height: 1080, VideoTimescale: 30000,
		VideoSamples: samples, KeyframeEvery: 3,
		ContainerRotation: 90, TrackRotation: 270,
		ContainerCrop:   &ingest.Crop{X: 0, Y: 0, Width: 1600, Height: 900},
		TrackCrop:       &ingest.Crop{X: 10, Y: 20, Width: 1280, Height: 720},
		ContainerColor:  &ingest.Color{Primaries: 1, Transfer: 1, Matrix: 1},
		TrackColor:      &ingest.Color{Primaries: 1, Transfer: 1, Matrix: 1},
		AudioSampleRate: 48000, AudioChannels: 2,
		AudioLayout: []string{"L", "R"},
	}

	good := base
	good.Path = fs.GoodMP4
	if _, err := ingest.GenerateMP4(good); err != nil {
		return fs, err
	}

	mxf := ingest.MXFFixtureSpec{
		Path: fs.GoodMXF, Width: 1920, Height: 1080,
		EditRate: [2]uint32{30, 1}, VideoSamples: samples,
		KeyframeEvery: 3, ContainerRotation: 90, TrackRotation: 270,
		ContainerCrop: base.ContainerCrop, TrackCrop: base.TrackCrop,
		ContainerColor: base.ContainerColor, TrackColor: base.TrackColor,
		AudioSampleRate: 48000, AudioChannels: 2,
		AudioLayout: []string{"L", "R"},
	}
	if _, err := ingest.GenerateMXF(mxf); err != nil {
		return fs, err
	}

	trunc := base
	trunc.Path = fs.DamagedMP4
	trunc.Damage = ingest.DamageTruncated
	if _, err := ingest.GenerateMP4(trunc); err != nil {
		return fs, err
	}

	bad := base
	bad.Path = fs.BadLayoutMP4
	bad.Damage = ingest.DamageBadLayout
	if _, err := ingest.GenerateMP4(bad); err != nil {
		return fs, err
	}
	return fs, nil
}
