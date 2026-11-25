// Package container parses MP4 (ISO BMFF) and MXF (KLV) media into the
// container-neutral model. Parsers never trust declared lengths: every read is
// bounds-checked and every violation is reported with the byte offset where it
// was detected.
package container

import (
	"fmt"

	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/model"
)

// Limits caps resource use for untrusted input. Zero values are replaced by
// the defaults from DefaultLimits.
type Limits struct {
	MaxFileSize int64 // bytes per input file
	MaxTracks   int   // tracks per container
	MaxSamples  int   // samples per track
	MaxBoxes    int   // boxes or KLV triplets per container
	MaxDepth    int   // nesting depth while walking a container
	MaxImageDim int   // widest or tallest accepted coded dimension
	MaxKLV      int64 // longest accepted KLV value
	MaxSync     int   // sync anchors per track
	MaxReorder  int   // RTP reorder window capacity
}

// DefaultLimits returns conservative caps suitable for the demo fixtures.
func DefaultLimits() Limits {
	return Limits{
		MaxFileSize: 64 << 20,
		MaxTracks:   16,
		MaxSamples:  100000,
		MaxBoxes:    20000,
		MaxDepth:    10,
		MaxImageDim: 8192,
		MaxKLV:      16 << 20,
		MaxSync:     1024,
		MaxReorder:  64,
	}
}

// Normalize fills zero fields with defaults and rejects negative fields.
func (l Limits) Normalize() (Limits, error) {
	d := DefaultLimits()
	if l.MaxFileSize < 0 || l.MaxKLV < 0 {
		return Limits{}, fmt.Errorf("limits: byte caps must be non-negative")
	}
	if l.MaxFileSize == 0 {
		l.MaxFileSize = d.MaxFileSize
	}
	if l.MaxKLV == 0 {
		l.MaxKLV = d.MaxKLV
	}
	ints := []struct {
		name string
		v    *int
		def  int
	}{
		{"MaxTracks", &l.MaxTracks, d.MaxTracks},
		{"MaxSamples", &l.MaxSamples, d.MaxSamples},
		{"MaxBoxes", &l.MaxBoxes, d.MaxBoxes},
		{"MaxDepth", &l.MaxDepth, d.MaxDepth},
		{"MaxImageDim", &l.MaxImageDim, d.MaxImageDim},
		{"MaxSync", &l.MaxSync, d.MaxSync},
		{"MaxReorder", &l.MaxReorder, d.MaxReorder},
	}
	for _, x := range ints {
		if *x.v < 0 {
			return Limits{}, fmt.Errorf("limits: %s must be non-negative", x.name)
		}
		if *x.v == 0 {
			*x.v = x.def
		}
	}
	return l, nil
}

// File is a parsed container with its tracks and recovered faults.
type File struct {
	Name   string
	Kind   model.Container
	Data   []byte // container bytes, retained for essence access
	Tracks []*model.Track
	Faults *fault.List
	Brand  string
}

// TrackByID returns the track with the container track ID, if present.
func (f *File) TrackByID(id uint32) (*model.Track, bool) {
	for _, t := range f.Tracks {
		if t.ID == id {
			return t, true
		}
	}
	return nil, false
}

// SyncCount counts samples marked as random-access points.
func (f *File) SyncCount() int {
	n := 0
	for _, t := range f.Tracks {
		for _, s := range t.Samples {
			if s.Sync {
				n++
			}
		}
	}
	return n
}
