// Package fixture synthesises deterministic local MP4 samples and the fault
// variants the pipeline must survive. Nothing here reads real media.
package fixture

import (
	"fmt"

	"mediasim/internal/clock"
)

// Media geometry shared by fixtures, timelines and the renderer.
const (
	TickNum = 90000
	TickDen = 1

	FPSNum = 30000
	FPSDen = 1001

	VideoFrameTicks = 3003 // 1001/30000 s on a 90 kHz axis, exactly
	AudioChunkTicks = 1920 // 1024 samples at 48 kHz on a 90 kHz axis, exactly

	VideoTimescale = 30000
	VideoDelta     = 1001
	AudioTimescale = 48000
	AudioDelta     = 1024

	Cam0Frames      = 70
	Cam1Frames      = 60
	Cam1StartFrame  = Cam0Frames - CrossfadeFrames
	TotalFrames     = Cam1StartFrame + Cam1Frames
	TotalTicks      = TotalFrames * VideoFrameTicks
	CrossfadeFrames = 10

	KeyframeEvery = 10
	AudioChunks   = 188 // covers 120 frames (4.004 s) at 1024 samples
)

// Track sources referenced by timeline segments.
const (
	SourceCam0  = "cam0"
	SourceCam1  = "cam1"
	SourceVoice = "voice"
)

// Track IDs written into the fixture container.
const (
	TrackCam0  = 1
	TrackCam1  = 2
	TrackVoice = 3
)

// Sample framing shared with the recovery scanner.
const (
	HeaderLen  = 16
	VideoBytes = 1600
	AudioBytes = AudioDelta + HeaderLen // 1040
)

var (
	TickRate = clock.MustRational(TickNum, TickDen)
	FPS      = clock.MustRational(FPSNum, FPSDen)
)

// SampleRef names one sample by source and index.
type SampleRef struct {
	Source string
	Index  int
}

// Fault describes deliberate damage written into a fixture.
type Fault struct {
	// TruncateKeepBytes > 0 cuts the file at that absolute byte length.
	TruncateKeepBytes int64
	// Corrupt overwrites the magic of these samples with garbage.
	Corrupt []SampleRef
	// MissingIndex omits stsz/stco/stsc so recovery must scan the mdat.
	MissingIndex bool
}

// Spec is the input to Build. Counts default to the full fixture geometry.
type Spec struct {
	Name  string
	Seed  int64
	Cam0  int
	Cam1  int
	Audio int
	Fault Fault
}

// SampleRef carries the per-sample expectations.
type TrackSpec struct {
	ID            uint32
	Kind          string // "video" or "audio"
	Source        string
	Count         int
	SampleSize    int
	FrameTicks    int64
	Timescale     uint32
	Delta         uint32
	KeyframeEvery int
	Codec         string
}

// Manifest describes what a fixture should contain, including its damage.
type Manifest struct {
	Name       string
	Seed       int64
	TickRate   clock.Rational
	FPS        clock.Rational
	TotalTicks int64
	Tracks     []TrackSpec
	Fault      Fault
}

// Track returns the spec for a source, or false.
func (m *Manifest) Track(source string) (TrackSpec, bool) {
	for _, t := range m.Tracks {
		if t.Source == source {
			return t, true
		}
	}
	return TrackSpec{}, false
}

// VideoFrameCount is the number of decodable video samples across all video
// sources (cam0 + cam1).
func (m *Manifest) VideoFrameCount() int {
	n := 0
	for _, t := range m.Tracks {
		if t.Kind == "video" {
			n += t.Count
		}
	}
	return n
}

// PresetSpec returns the spec for one of the four required fixture classes.
func PresetSpec(name string) (Spec, error) {
	base := Spec{Seed: 17, Cam0: Cam0Frames, Cam1: Cam1Frames, Audio: AudioChunks}
	switch name {
	case "normal":
		base.Name = "normal"
	case "truncated":
		base.Name = "truncated"
		// Keep 47 complete cam0 samples plus a partial 48th.
		base.Fault.TruncateKeepBytes = 47*VideoBytes + 700
	case "corrupt":
		base.Name = "corrupt"
		for i := 30; i <= 32; i++ {
			base.Fault.Corrupt = append(base.Fault.Corrupt, SampleRef{SourceCam0, i})
		}
		base.Fault.Corrupt = append(base.Fault.Corrupt,
			SampleRef{SourceVoice, 60}, SampleRef{SourceVoice, 61})
	case "missing-index", "missing_index":
		base.Name = "missing-index"
		base.Fault.MissingIndex = true
	default:
		return Spec{}, fmt.Errorf("unknown fixture %q (want normal, truncated, corrupt, missing-index)", name)
	}
	return base, nil
}

// PresetNames lists every deterministic fixture class.
func PresetNames() []string {
	return []string{"normal", "truncated", "corrupt", "missing-index"}
}
