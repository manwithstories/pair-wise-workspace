// Package fixture builds self-contained CMAF/fMP4 sample corpora. Every byte
// is produced by FFmpeg plus Go byte surgery in a temporary directory; no
// external media is referenced.
package fixture

import (
	"archive/internal/media"
)

// CFR and VFR are the two video timing modes the fixtures exercise.
type TimingMode string

const (
	CFR TimingMode = "cfr"
	VFR TimingMode = "vfr"
)

// Level describes one representation (bitrate ladder rung).
type Level struct {
	Name     string  `json:"name"`
	BitrateK int     `json:"bitrate_k"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	FPS      float64 `json:"fps"`
	Timing   TimingMode
	// AudioBitrateK is the AAC bitrate for this rung.
	AudioBitrateK int `json:"audio_bitrate_k"`
}

// Entry describes the encoded file backing a level.
type Entry struct {
	Level        string  `json:"level"`
	Path         string  `json:"path"`
	SHA256       string  `json:"sha256"`
	SizeBytes    int64   `json:"size_bytes"`
	SegmentCount int     `json:"segment_count"`
	TimeScale    uint32  `json:"time_scale"`
	BitrateK     int     `json:"bitrate_k"`
	Width        int     `json:"width"`
	Height       int     `json:"height"`
	FPS          float64 `json:"fps"`
	Timing       TimingMode
	DurationUS   int64 `json:"duration_us"`
}

// Fault enumerates the deliberately injected defects of the mixed profile.
type Fault struct {
	Kind    string `json:"kind"`
	Level   string `json:"level,omitempty"`
	Segment uint32 `json:"segment,omitempty"`
	// Detail carries the numeric shape of the fault (drop count, ppm).
	Detail int64  `json:"detail,omitempty"`
	Note   string `json:"note,omitempty"`
}

// Manifest is the fixture's contract with recover/verify.
type Manifest struct {
	Profile        string  `json:"profile"`
	CreatedAt      string  `json:"created_at"`
	Dir            string  `json:"dir"`
	SharedTimeBase string  `json:"shared_time_base"`
	FrameCount     int     `json:"expected_frame_count"`
	DurationUS     int64   `json:"duration_us"`
	FPS            float64 `json:"fps"`
	Levels         []Entry `json:"levels"`
	Faults         []Fault `json:"faults"`
	// Expected records what the verifier must observe.
	Expected Expectation `json:"expected"`
}

// Expectation is the acceptance contract stated by the fixture itself.
type Expectation struct {
	MaxBoundaryDeltaUS int64 `json:"max_boundary_delta_us"`
	FrameCount         int   `json:"frame_count"`
	MinRecoverable     int   `json:"min_recoverable_segments"`
}

// LevelSet is the canonical three-rung ladder used by both profiles.
func LevelSet() []Level {
	return []Level{
		{Name: "high", BitrateK: 2400, Width: 640, Height: 360, FPS: 25, Timing: CFR, AudioBitrateK: 96},
		{Name: "mid", BitrateK: 1100, Width: 480, Height: 270, FPS: 25, Timing: CFR, AudioBitrateK: 96},
		{Name: "low", BitrateK: 450, Width: 320, Height: 180, FPS: 25, Timing: CFR, AudioBitrateK: 64},
	}
}

// Profile selects which fault programme to inject.
type Profile string

const (
	ProfileClean Profile = "clean"
	ProfileMixed Profile = "mixed"
)

// Fault kinds injected by the mixed profile.
const (
	FaultDroppedSegment   = "dropped_segment"
	FaultDuplicateSegment = "duplicate_segment"
	FaultTFDTRegression   = "tfdt_regression"
	FaultWrap             = "timestamp_wrap"
	FaultDrift            = "clock_drift"
	FaultVFR              = "vfr"
	FaultTruncatedMdat    = "truncated_mdat"
	FaultBoundaryConflict = "boundary_conflict"
)

// RepresentationName maps a level to its on-disk stem.
func RepresentationName(l Level) string { return l.Name }

// KindOf maps a codec fourcc to the media kind.
func KindOf(codec string) media.TrackKind {
	switch codec {
	case "mp4a":
		return media.KindAudio
	default:
		return media.KindVideo
	}
}
