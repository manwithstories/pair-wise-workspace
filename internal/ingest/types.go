// Package ingest parses container media (MXF/MP4) into normalized track
// summaries and reconciles container- vs track-level metadata conflicts.
package ingest

import "github.com/example/mediad/internal/clock"

// Rotation degrees, only multiples of 90 are representable safely.
type Rotation int

// Crop defines a clean-aperture / image crop rectangle in pixels.
type Crop struct {
	X      int64
	Y      int64
	Width  int64
	Height int64
}

// Color describes an ICC / NCLC color description.
type Color struct {
	// Primaries/Transfer/Matrix are the NCLC triplet; 0 means absent.
	Primaries uint16
	Transfer  uint16
	Matrix    uint16
	// ICCProfile non-empty means an ICC profile was referenced.
	ICCProfile bool
	Source     string // where the description came from (container|track)
}

// AudioTrackLayout summarizes one audio track's rate/channel layout.
type AudioTrackLayout struct {
	TrackID       uint32
	Codec         string // pcm | aac
	SampleRate    uint32
	Channels      uint16
	ChannelLayout []string // ordered channel labels, e.g. L,R,C,LFE
	SampleCount   uint64
}

// VideoTrackSummary carries the descriptors needed for timeline planning.
type VideoTrackSummary struct {
	TrackID         uint32
	Codec           string // h264 | h265 | pcm-surrogate
	Width           uint32
	Height          uint32
	Timescale       uint32
	SampleCount     uint64
	KeyframeIndexes []uint32 // sample numbers that are sync samples
	Crop            *Crop
	Colors          []Color
}

// Track is the normalized result: raw descriptor values plus the
// adjudicated values a downstream stage is allowed to act on.
type Track struct {
	Kind          string // video | audio
	Video         *VideoTrackSummary
	Audio         *AudioTrackLayout
	ContainerRot  Rotation // rotation declared at movie level (mvhd)
	TrackRot      Rotation // rotation declared at track level (tkhd)
	ContainerCrop *Crop
	// Adjudicated fields, filled by conflict resolution.
	AppliedRotation Rotation
	AppliedCrop     *Crop
	AppliedColor    *Color
	ConflictNotes   []string
	// Rejected is set when descriptors cannot be safely reconciled.
	Rejected bool
}

// Summary is one parsed container file.
type Summary struct {
	Format    string // mxf | mp4
	Path      string
	Timescale uint32
	Duration  clock.Micros
	Tracks    []*Track
	// FixtureCorrupt marks intentionally damaged test inputs.
	FixtureCorrupt bool
	CorruptReason  string
}

// Segment is a planned output slice derived from a normalized track.
type Segment struct {
	ID          string
	TrackID     uint32
	Start       clock.Micros
	End         clock.Micros
	Keyframe    bool
	SourcePath  string
	SampleRange [2]uint64 // inclusive sample indexes [from, to]
}
