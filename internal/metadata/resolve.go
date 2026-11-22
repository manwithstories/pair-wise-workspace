// Package metadata merges container-level and track-level descriptive
// metadata (rotation, crop, color) into one resolved view. Every output
// value records its winning source, every conflict is logged, and an
// out-of-bounds crop fails the job before any output is committed.
package metadata

import (
	"fmt"

	"mediarun/internal/mediaerr"
)

// Source ranks where a metadata value came from. Lower ordinal wins. The
// precedence is fixed by specification: explicit override > track >
// container > built-in default.
type Source string

const (
	SourceDefault   Source = "default"
	SourceContainer Source = "container"
	SourceTrack     Source = "track"
	SourceOverride  Source = "explicit_override"
)

// rank returns the precedence ordinal; a larger number means stronger.
func (s Source) rank() int {
	switch s {
	case SourceOverride:
		return 4
	case SourceTrack:
		return 3
	case SourceContainer:
		return 2
	default:
		return 1
	}
}

// Rotation in degrees, normalized to {0,90,180,270}.
type Rotation struct {
	Degrees int    `json:"degrees"`
	Source  Source `json:"source"`
}

// Crop is a pixel rectangle relative to the coded frame's top-left corner.
type Crop struct {
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Source Source `json:"source"`
}

// Color describes transfer/primaries/matrix identifiers (e.g. "bt709").
type Color struct {
	Primaries string `json:"primaries"`
	Transfer  string `json:"transfer"`
	Matrix    string `json:"matrix"`
	Source    Source `json:"source"`
}

// Decision records one arbitration: the chosen value, its source, and every
// competing input that lost, so the manifest can explain each output field.
type Decision struct {
	Field    string   `json:"field"`
	Winner   Source   `json:"winner"`
	Value    string   `json:"value"`
	Losers   []string `json:"losers,omitempty"`
	Conflict bool     `json:"conflict"`
}

// View is the fully resolved metadata for one track.
type View struct {
	TrackID   string     `json:"track_id"`
	Rotation  Rotation   `json:"rotation"`
	Crop      *Crop      `json:"crop,omitempty"`
	Color     Color      `json:"color"`
	Decisions []Decision `json:"decisions"`
}

// Input bundles the competing descriptions of one track from each layer.
// Nil fields mean that layer provided no value.
type Input struct {
	TrackID        string
	CodedWidth     int
	CodedHeight    int
	Override       *LayerValues // explicit operator override (strongest)
	TrackLayer     *LayerValues // track-level (tkhd/colr/sample entry)
	ContainerLayer *LayerValues // container-level (mvhd/moov wide tags)
}

// LayerValues is the set of values one layer may provide. Pointers/nil
// slices distinguish "absent" from an explicit zero.
type LayerValues struct {
	Rotation *int
	Crop     *Crop
	Color    *Color
}

// Defaults applied when no layer supplies a value.
var (
	defaultRotation = Rotation{Degrees: 0, Source: SourceDefault}
	defaultColor    = Color{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709", Source: SourceDefault}
)

// validRotation normalizes and validates a rotation value.
func validRotation(deg int) bool {
	switch deg {
	case 0, 90, 180, 270, -90:
		return true
	}
	return false
}

// normalizeRotation folds -90 into 270.
func normalizeRotation(deg int) (int, error) {
	if !validRotation(deg) {
		return 0, mediaerr.New(mediaerr.CodeInvalidConfig,
			fmt.Sprintf("rotation %d not in {0,90,180,270}", deg))
	}
	if deg == -90 {
		return 270, nil
	}
	return deg, nil
}
