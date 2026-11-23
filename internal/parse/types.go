// Package parse implements streaming ISO-BMFF box parsing. Types shared across
// the parse and pipeline layers live here.
package parse

import (
	"fmt"

	"archivecheck/internal/fault"
)

// Track describes one track declared by an initialization segment.
type Track struct {
	ID              uint32
	Timescale       uint32
	Handler         string
	DefaultDuration uint32
	DefaultSize     uint32
	DefaultFlags    uint32
	HasTrexDefaults bool
}

// Init is the parsed state of an initialization segment.
type Init struct {
	Segment string
	Brand   string
	Tracks  map[uint32]*Track
	Order   []uint32
}

// TrackByID returns the declared track or an offset-bearing fault.
func (in *Init) TrackByID(id uint32) (*Track, error) {
	if tr, ok := in.Tracks[id]; ok {
		return tr, nil
	}
	return nil, fault.New(fault.KindMissingBox, in.Segment, "trex", 0,
		fmt.Sprintf("track %d is not declared by the initialization segment", id))
}

// Sample is a concrete sample range within a media segment.
type Sample struct {
	TrackID           uint32
	Duration          uint32
	Size              uint32
	Flags             uint32
	CompositionOffset int32
	Offset            int64
}

// Sync reports whether the sample is a random-access point.
func (s Sample) Sync() bool { return s.Flags&0x00010000 == 0 }

// Traf is one track fragment with its decode timeline anchor.
type Traf struct {
	TrackID        uint32
	BaseDecodeTime uint64
	HasTfdt        bool
	TfdtVersion    uint8
	Samples        []Sample
}

// Fragment is a parsed media segment: one moof plus its mdat.
type Fragment struct {
	Group       int
	Segment     string
	Sequence    uint32
	HasMfhd     bool
	MfhdOffset  int64
	MoofOffset  int64
	MoofSize    uint64
	MdatOffset  int64
	MdatSize    uint64
	MdatPayload int64
	MdatEnd     int64
	Trafs       []Traf
}
