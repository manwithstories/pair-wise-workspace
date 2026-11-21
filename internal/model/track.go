package model

import (
	"fmt"

	"mediad/internal/clock"
)

// TrackKind is the coarse media type of a track.
type TrackKind string

const (
	// Video is a picture track.
	Video TrackKind = "video"
	// Audio is a sound track.
	Audio TrackKind = "audio"
	// Data is any other track (timecode, subtitle, metadata).
	Data TrackKind = "data"
)

// Crop is a clean-aperture rectangle expressed as the number of pixels
// removed from each edge of the coded frame.
type Crop struct {
	Left   uint32 `json:"left"`
	Right  uint32 `json:"right"`
	Top    uint32 `json:"top"`
	Bottom uint32 `json:"bottom"`
}

// IsZero reports whether no pixels are cropped.
func (c Crop) IsZero() bool { return c == Crop{} }

// Equal reports whether two crops are identical.
func (c Crop) Equal(o Crop) bool { return c == o }

// Width returns the clean width after cropping codedWidth.
func (c Crop) Width(codedWidth uint32) (uint32, error) {
	if c.Left+c.Right >= codedWidth {
		return 0, fmt.Errorf("crop removes %d+%d of %d columns", c.Left, c.Right, codedWidth)
	}
	return codedWidth - c.Left - c.Right, nil
}

// Height returns the clean height after cropping codedHeight.
func (c Crop) Height(codedHeight uint32) (uint32, error) {
	if c.Top+c.Bottom >= codedHeight {
		return 0, fmt.Errorf("crop removes %d+%d of %d rows", c.Top, c.Bottom, codedHeight)
	}
	return codedHeight - c.Top - c.Bottom, nil
}

// ColorSpace is an ISO/IEC 23001-8 (H.273) colour description.
// Code 2 means "unspecified" in every field, which is the documented default
// recorded when a container carries no description at all.
type ColorSpace struct {
	Primaries uint16 `json:"primaries"`
	Transfer  uint16 `json:"transfer"`
	Matrix    uint16 `json:"matrix"`
	FullRange bool   `json:"full_range"`
	// Description is the container encoding that carried the value
	// ("nclx", "nclc", "mxf-ul", "explicit", "default").
	Description string `json:"description"`
}

// DefaultColor is the value recorded for an unknown colour description.
func DefaultColor() ColorSpace {
	return ColorSpace{Primaries: 2, Transfer: 2, Matrix: 2, FullRange: false, Description: "default"}
}

// Equal compares the numeric fields only.
func (c ColorSpace) Equal(o ColorSpace) bool {
	return c.Primaries == o.Primaries && c.Transfer == o.Transfer &&
		c.Matrix == o.Matrix && c.FullRange == o.FullRange
}

// ColorLayer names the precedence level a colour value arrived from.
type ColorLayer string

const (
	// ColorLayerContainer is the container- or format-level default.
	ColorLayerContainer ColorLayer = "container"
	// ColorLayerTrack is a description attached to the individual track.
	ColorLayerTrack ColorLayer = "track"
	// ColorLayerExplicit is an operator-supplied override.
	ColorLayerExplicit ColorLayer = "explicit"
)

// Rank orders layers; larger wins during arbitration.
func (l ColorLayer) Rank() int {
	switch l {
	case ColorLayerContainer:
		return 1
	case ColorLayerTrack:
		return 2
	case ColorLayerExplicit:
		return 3
	default:
		return 0
	}
}

// ColorCandidate is one observed colour description and where it came from.
type ColorCandidate struct {
	Layer  ColorLayer
	Color  ColorSpace
	Origin string
}

// ColorDecision is the arbitration result for a track.
type ColorDecision struct {
	Chosen ColorSpace       `json:"chosen"`
	Trace  []ColorCandidate `json:"trace"`
	// Conflicts counts candidates whose numeric fields disagreed with the
	// winner.
	Conflicts int `json:"conflicts"`
}

// Track is the shared, time-base-normalised description of one track. Both
// the MP4 and MXF probes emit this type so downstream stages never need to
// know which container the track came from.
type Track struct {
	ID        uint32    `json:"id"`
	Kind      TrackKind `json:"kind"`
	Codec     string    `json:"codec"`
	Source    string    `json:"source"`
	Timescale uint32    `json:"timescale"`

	// Start is the presentation time of the first sample relative to the
	// timeline origin, after edit-list offsetting.
	Start clock.Rational `json:"start"`
	// Duration is the presentation duration of the track.
	Duration clock.Rational `json:"duration"`
	// EditOffset is the edit-list media time (positive delays the track).
	EditOffset clock.Rational `json:"edit_offset"`

	CodedWidth   uint32         `json:"coded_width"`
	CodedHeight  uint32         `json:"coded_height"`
	Rotation     int            `json:"rotation"`
	Crop         Crop           `json:"crop"`
	SampleAspect clock.Rational `json:"sample_aspect"`

	// Color is the arbitrated colour description and the trace that led to it.
	// It is filled by normalize from ColorCandidates; probes only collect the
	// candidates so that arbitration stays in one place.
	Color ColorDecision `json:"color"`
	// ColorCandidates are the raw observations made by a probe.
	ColorCandidates []ColorCandidate `json:"-"`

	// DeclaredBitrateKbps is whatever the container metadata claims. It is
	// only a target; the manifest computes the actual value from samples.
	DeclaredBitrateKbps float64 `json:"declared_bitrate_kbps"`
	// FrameRate is the nominal sample rate in samples/second, when known.
	FrameRate clock.Rational `json:"frame_rate"`

	Samples []Sample `json:"-"`
}

// DisplayWidth returns the width after rotation and cropping. Rotation by 90
// or 270 degrees swaps the axes.
func (t Track) DisplayWidth() uint32 {
	w, h := t.cleanDimensions()
	if t.Rotation == 90 || t.Rotation == 270 {
		return h
	}
	return w
}

// DisplayHeight returns the height after rotation and cropping.
func (t Track) DisplayHeight() uint32 {
	w, h := t.cleanDimensions()
	if t.Rotation == 90 || t.Rotation == 270 {
		return w
	}
	return h
}

func (t Track) cleanDimensions() (uint32, uint32) {
	w, h := t.CodedWidth, t.CodedHeight
	if cw, err := t.Crop.Width(w); err == nil {
		w = cw
	}
	if ch, err := t.Crop.Height(h); err == nil {
		h = ch
	}
	return w, h
}

// End returns Start+Duration.
func (t Track) End() (clock.Rational, error) { return t.Start.Add(t.Duration) }

// Validate checks internal consistency and, when samples are present, that
// their decode timestamps are strictly increasing.
func (t Track) Validate() error {
	if t.ID == 0 {
		return New(KindInvalidMedia, "model", "track id must be non-zero")
	}
	if t.Kind != Video && t.Kind != Audio && t.Kind != Data {
		return New(KindInvalidMedia, "model", fmt.Sprintf("track %d has unknown kind %q", t.ID, t.Kind)).WithTrack(t.ID)
	}
	if t.Duration.Sign() < 0 {
		return New(KindInvalidMedia, "model", "negative duration").WithTrack(t.ID)
	}
	if _, err := t.Crop.Width(t.CodedWidth); err != nil {
		return New(KindInvalidMedia, "model", err.Error()).WithTrack(t.ID)
	}
	if _, err := t.Crop.Height(t.CodedHeight); err != nil {
		return New(KindInvalidMedia, "model", err.Error()).WithTrack(t.ID)
	}
	for i := 1; i < len(t.Samples); i++ {
		if !t.Samples[i-1].DTS.Less(t.Samples[i].DTS) {
			return New(KindInvalidMedia, "model", fmt.Sprintf("sample %d DTS %v not after %v", i, t.Samples[i].DTS, t.Samples[i-1].DTS)).WithTrack(t.ID)
		}
	}
	return nil
}

// Container is a probed media file.
type Container struct {
	Format string  `json:"format"`
	Tracks []Track `json:"tracks"`
}

// TrackByID finds a track, if present.
func (c Container) TrackByID(id uint32) (Track, bool) {
	for _, t := range c.Tracks {
		if t.ID == id {
			return t, true
		}
	}
	return Track{}, false
}
