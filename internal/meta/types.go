// Package meta reads MP4 (via github.com/abema/go-mp4) and the convention
// subset of MXF KLV, then reconciles rotation, crop and color description
// across explicit override, track, container and built-in default sources.
package meta

import (
	"fmt"

	"relaycheck/internal/media"
)

// Source identifies where a metadata value came from. Higher numeric value
// means higher precedence.
type Source int

const (
	SourceDefault   Source = 0 // explicit built-in default (lowest)
	SourceContainer Source = 1 // container-level description
	SourceTrack     Source = 2 // track / sample description
	SourceOverride  Source = 3 // caller-supplied explicit override (highest)
)

func (s Source) String() string {
	switch s {
	case SourceDefault:
		return "default"
	case SourceContainer:
		return "container"
	case SourceTrack:
		return "track"
	case SourceOverride:
		return "override"
	default:
		return fmt.Sprintf("source(%d)", int(s))
	}
}

// Rotation is a clockwise rotation in degrees. Legal broadcast frame
// rotations are quarter turns.
type Rotation struct {
	Degrees int `json:"degrees"`
}

// Legal reports whether the angle is an allowed quarter-turn.
func (r Rotation) Legal() bool {
	switch ((r.Degrees % 360) + 360) % 360 {
	case 0, 90, 180, 270:
		return true
	default:
		return false
	}
}

// Crop is a pixel rectangle within the coded frame. X/Y are the top-left
// offset; W/H are the kept width/height. FrameW/FrameH bound it.
type Crop struct {
	X      int64 `json:"x"`
	Y      int64 `json:"y"`
	W      int64 `json:"w"`
	H      int64 `json:"h"`
	FrameW int64 `json:"frame_w"`
	FrameH int64 `json:"frame_h"`
}

// InBounds reports whether the rectangle is positive and inside the frame.
func (c Crop) InBounds() bool {
	if c.W <= 0 || c.H <= 0 || c.X < 0 || c.Y < 0 {
		return false
	}
	if c.FrameW > 0 && c.X+c.W > c.FrameW {
		return false
	}
	if c.FrameH > 0 && c.Y+c.H > c.FrameH {
		return false
	}
	return true
}

// Equal compares two crops.
func (c Crop) Equal(o Crop) bool { return c == o }

// Color is the reconciled color description. Known is false for an unknown
// colour marker that must be preserved/reported rather than guessed.
type Color struct {
	ColourType string `json:"colour_type,omitempty"`
	Primaries  string `json:"primaries"`
	Transfer   string `json:"transfer"`
	Matrix     string `json:"matrix"`
	FullRange  bool   `json:"full_range"`
	Known      bool   `json:"known"`
	RawMarker  uint16 `json:"raw_marker,omitempty"`
}

// Equal compares two color descriptions.
func (c Color) Equal(o Color) bool { return c == o }

// Layer is one precedence tier of optional metadata claims.
type Layer struct {
	Rotation *Rotation `json:"rotation,omitempty"`
	Crop     *Crop     `json:"crop,omitempty"`
	Color    *Color    `json:"color,omitempty"`
}

// Conflict records two present sources that disagreed on one attribute.
type Conflict struct {
	TrackID   int64    `json:"track_id"`
	Attribute string   `json:"attribute"`
	Sources   []string `json:"sources"`
	Values    []string `json:"values"`
	Chosen    string   `json:"chosen_source"`
	Message   string   `json:"message"`
}

// ResolvedTrack is the adjudicated metadata for one track.
type ResolvedTrack struct {
	ID        int64           `json:"id"`
	Type      string          `json:"type"`
	TimeScale uint32          `json:"timescale"`
	Duration  int64           `json:"duration"`
	TimeRange media.TimeRange `json:"time_range"`
	Rotation  Rotation        `json:"rotation"`
	Crop      *Crop           `json:"crop,omitempty"`
	Color     *Color          `json:"color,omitempty"`
	Width     uint32          `json:"width,omitempty"`
	Height    uint32          `json:"height,omitempty"`
	Conflicts []Conflict      `json:"conflicts,omitempty"`
}

// Descriptor is the parsed/adjudicated metadata for a container.
type Descriptor struct {
	Container string          `json:"container"`
	Case      string          `json:"case,omitempty"`
	Tracks    []ResolvedTrack `json:"tracks"`
	Conflicts []Conflict      `json:"conflicts,omitempty"`
}

// TrackByID returns the track with id, or nil.
func (d *Descriptor) TrackByID(id int64) *ResolvedTrack {
	for i := range d.Tracks {
		if d.Tracks[i].ID == id {
			return &d.Tracks[i]
		}
	}
	return nil
}
