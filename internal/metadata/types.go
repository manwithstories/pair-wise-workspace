// Package metadata parses MP4 (via go-mp4) and a project-defined minimal MXF
// metadata sample, then normalizes both containers to one track model with a
// rational timebase, right-angle rotation and explicit color provenance.
package metadata

import (
	"fmt"
	"math"
	"sort"

	"mediad/internal/clock"
	"mediad/internal/model"
)

// Kind is the normalized media kind.
type Kind string

const (
	KindVideo Kind = "video"
	KindAudio Kind = "audio"
)

// Color primaries, transfer characteristics and matrix coefficients use
// ISO/IEC 23001-8 code points.
const (
	PrimariesBT709  uint16 = 1
	PrimariesBT2020 uint16 = 9
	PrimariesP3     uint16 = 12
	TransferBT709   uint16 = 1
	TransferSRGB    uint16 = 13
	TransferPQ      uint16 = 16
	MatrixBT709     uint16 = 1
	MatrixBT2020    uint16 = 9
)

// Color sources, highest priority first. The ranking is explicit so a
// conflict can never be resolved silently by map iteration order.
const (
	SourceDefault     = "default"
	SourceSampleEntry = "sample-entry"
	SourceColr        = "colr"
	SourceMXF         = "mxf"
)

var sourcePriority = map[string]int{
	SourceDefault:     0,
	SourceSampleEntry: 1,
	SourceColr:        3,
	SourceMXF:         3,
}

// Color is a normalized color description plus its provenance.
type Color struct {
	Primaries uint16
	Transfer  uint16
	Matrix    uint16
	FullRange bool
	Source    string
	Priority  int
	Defaulted bool
	// Incomplete means the container supplied a color description but left at
	// least one component unspecified. It is a metadata error, not a silent
	// default: the operator must see that the source was partial.
	Incomplete bool
	// Conflicts lists descriptions that were present but not chosen. They are
	// preserved so the manifest can explain the decision.
	Conflicts []string
}

// DefaultColor is recorded whenever a track/container omits a description.
var DefaultColor = Color{
	Primaries: PrimariesBT709,
	Transfer:  TransferBT709,
	Matrix:    MatrixBT709,
	Source:    SourceDefault,
	Priority:  0,
	Defaulted: true,
}

// Known reports whether all code points are recognized.
func (c Color) Known() bool {
	return c.Primaries != 0 && c.Transfer != 0 && c.Matrix != 0
}

// Key is a stable provenance string for the manifest.
func (c Color) Key() string {
	return fmt.Sprintf("p%d/t%d/m%d/full=%t/src=%s", c.Primaries, c.Transfer, c.Matrix, c.FullRange, c.Source)
}

// Crop is a validated clean aperture rectangle in coded-frame coordinates.
type Crop struct {
	X, Y, W, H int
}

// Track is one normalized metadata track.
type Track struct {
	Key     string
	Kind    Kind
	TrackID uint32
	Codec   string
	// Timebase is the rational duration of one tick, reduced.
	Timebase clock.Timebase
	// Start and Duration are in Timebase ticks; the interval is half-open.
	Start    int64
	Duration int64
	// EditStart is the media time offset declared by the container's edit
	// list (or 0), kept for provenance.
	EditStart int64

	CodedWidth, CodedHeight     int
	DisplayWidth, DisplayHeight int
	// Rotation is normalized to {0,90,180,270} clockwise.
	Rotation int
	// RotationRaw is the container-declared angle before normalization.
	RotationRaw float64
	Crop        *Crop
	Color       Color
	Container   string
	// Audio sampling metadata (zero for video tracks).
	SampleRate int
	Channels   int
	Notes      []string
}

// Interval returns the half-open timeline interval of the track.
func (t Track) Interval() model.Interval {
	return model.Interval{Start: t.Start, End: t.Start + t.Duration, Timebase: t.Timebase}
}

// CroppedWidth/Height are the clean-aperture dimensions before rotation.
func (t Track) CroppedWidth() int {
	if t.Crop != nil {
		return t.Crop.W
	}
	return t.CodedWidth
}

func (t Track) CroppedHeight() int {
	if t.Crop != nil {
		return t.Crop.H
	}
	return t.CodedHeight
}

// OutputWidth/Height are the dimensions after crop and rotation.
func (t Track) OutputWidth() int {
	if t.Rotation == 90 || t.Rotation == 270 {
		return t.CroppedHeight()
	}
	return t.CroppedWidth()
}

func (t Track) OutputHeight() int {
	if t.Rotation == 90 || t.Rotation == 270 {
		return t.CroppedWidth()
	}
	return t.CroppedHeight()
}

// FrameTicks converts a frame rate to ticks in the track timebase, rounding.
func (t Track) FrameTicks(fpsNum, fpsDen int64) int64 {
	// frames/second = fpsNum/fpsDen; ticks/frame = timebase seconds / (fpsNum/fpsDen)
	// = (Num/Den) * (fpsDen/fpsNum)
	if fpsNum <= 0 || fpsDen <= 0 || !t.Timebase.Valid() {
		return 0
	}
	// ticks/frame = (fpsDen/fpsNum seconds) / (Num/Den seconds per tick)
	return (fpsDen * t.Timebase.Den) / (fpsNum * t.Timebase.Num)
}

// Asset is a parsed container plus every track-level failure. A failure in one
// track never hides the remaining tracks: corrupted input is isolated.
type Asset struct {
	Path      string
	Container string
	Tracks    []Track
	Failures  []TrackFailure
	Notes     []string
}

// TrackFailure associates a metadata error with its normalized track key.
type TrackFailure struct {
	Key string
	Err *model.Error
}

// SortedTracks returns tracks ordered by kind (video first) and key.
func (a *Asset) SortedTracks() []Track {
	out := append([]Track(nil), a.Tracks...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == KindVideo
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Track looks up a normalized track by key.
func (a *Asset) Track(key string) (Track, bool) {
	for _, t := range a.Tracks {
		if t.Key == key {
			return t, true
		}
	}
	return Track{}, false
}

// Failure looks up a track-level failure by key.
func (a *Asset) Failure(key string) *model.Error {
	for _, f := range a.Failures {
		if f.Key == key {
			return f.Err
		}
	}
	return nil
}

// NormalizeRotation converts an arbitrary clockwise angle to the nearest right
// angle in {0,90,180,270}. The raw angle is retained for provenance.
func NormalizeRotation(degrees float64) (int, float64) {
	raw := degrees
	for raw < 0 {
		raw += 360
	}
	for raw >= 360 {
		raw -= 360
	}
	quarter := int((raw + 45) / 90)
	if quarter >= 4 {
		quarter -= 4
	}
	return quarter * 90, raw
}

// MatrixRotation extracts the clockwise rotation from a 3x3 tkhd matrix
// (16.16 fixed point, row-major as a,b,u,c,d,v,x,y,w).
func MatrixRotation(m [9]int32) float64 {
	// Display transform: x' = a*x + c*y + u, y' = b*x + d*y + v, with y
	// pointing down. The clockwise media rotation is atan2(b, a): the identity
	// matrix gives 0 and {0,+1,-1,0} maps right to down, i.e. 90 degrees.
	a := float64(m[0]) / 65536
	b := float64(m[1]) / 65536
	deg := 0.0
	if a != 0 || b != 0 {
		deg = math.Atan2(b, a) * 180 / math.Pi
	}
	if deg < 0 {
		deg += 360
	}
	return deg
}

// ValidateCrop verifies that the clean aperture lies inside the coded frame
// and reports a track-scoped, interval-scoped metadata error otherwise.
func ValidateCrop(t Track) *model.Error {
	if t.Crop == nil {
		return nil
	}
	c := *t.Crop
	if c.W <= 0 || c.H <= 0 || c.X < 0 || c.Y < 0 ||
		c.W > t.CodedWidth || c.H > t.CodedHeight ||
		c.X+c.W > t.CodedWidth || c.Y+c.H > t.CodedHeight {
		return model.New(model.CategoryMetadata, "crop",
			fmt.Sprintf("clean aperture x=%d y=%d w=%d h=%d exceeds coded frame %dx%d",
				c.X, c.Y, c.W, c.H, t.CodedWidth, t.CodedHeight)).
			WithTrack(t.Key).WithInterval(t.Interval())
	}
	return nil
}

// ResolveColor picks one output color description from tracks. Explicit
// descriptions outrank defaults; ties break on track key. Every non-chosen
// explicit description is recorded as a conflict, so resolution is never
// silent. Tracks without a description contribute a recorded default.
func ResolveColor(tracks []Track) Color {
	type candidate struct {
		color Color
		key   string
	}
	var cands []candidate
	for _, t := range tracks {
		if t.Kind != KindVideo {
			continue
		}
		cands = append(cands, candidate{t.Color, t.Key})
	}
	if len(cands) == 0 {
		c := DefaultColor
		c.Conflicts = append(c.Conflicts, "no video tracks: default recorded")
		return c
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].color.Priority != cands[j].color.Priority {
			return cands[i].color.Priority > cands[j].color.Priority
		}
		return cands[i].key < cands[j].key
	})
	chosen := cands[0].color
	chosen.Conflicts = append([]string(nil), chosen.Conflicts...)
	for _, c := range cands[1:] {
		if c.color.Key() == chosen.Key() && c.color.Source == chosen.Source {
			continue
		}
		reason := fmt.Sprintf("priority %d < %d", c.color.Priority, chosen.Priority)
		if c.color.Priority == chosen.Priority {
			reason = fmt.Sprintf("tie on priority %d, track key %s > %s loses", chosen.Priority, c.key, cands[0].key)
		}
		chosen.Conflicts = append(chosen.Conflicts,
			fmt.Sprintf("%s: %s not chosen (%s)", c.key, c.color.Key(), reason))
	}
	return chosen
}

// NewColor builds a normalized color and records unsupported/absent cases
// without overwriting an explicitly supplied description.
func NewColor(primaries, transfer, matrix uint16, fullRange bool, source string) Color {
	c := DefaultColor
	c.FullRange = fullRange
	c.Source = source
	c.Priority = sourcePriority[source]
	c.Defaulted = false
	if primaries != 0 {
		c.Primaries = primaries
	}
	if transfer != 0 {
		c.Transfer = transfer
	}
	if matrix != 0 {
		c.Matrix = matrix
	}
	if primaries == 0 || transfer == 0 || matrix == 0 {
		c.Defaulted = true
		c.Incomplete = true
	}
	return c
}
