// Package mp4 parses and validates CMAF/fMP4 init segments and media
// fragments. Corrupt input is reported with a byte offset so callers can
// isolate the bad fragment and keep processing later ones.
package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	gomp4 "github.com/abema/go-mp4"
)

// CorruptError reports malformed box structure at a byte offset.
type CorruptError struct {
	Offset int64
	Reason string
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("corrupt box at offset %d: %s", e.Offset, e.Reason)
}

// AsCorrupt extracts a *CorruptError from err, if any.
func AsCorrupt(err error) (*CorruptError, bool) {
	var ce *CorruptError
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}

// ErrMissingBox marks fragments that lack a structurally required box.
var ErrMissingBox = errors.New("missing required box")

// BoxInfo records one box's location inside a parsed input.
type BoxInfo struct {
	Type   string
	Offset int64
	Size   int64
}

// Sample is one decoded trun entry with its media data slice.
type Sample struct {
	TrackID  uint32
	Seq      uint32 // mfhd sequence number of the source fragment
	Index    int    // sample index inside the fragment
	DTS      int64  // track-timescale units
	PTS      int64  // DTS + composition offset
	Duration uint32 // track-timescale units
	Size     uint32
	Keyframe bool
	Data     []byte
}

// ParsedBox keeps a go-mp4 decoded box plus its raw payload bytes for
// roundtrip verification (go-mp4 marshals payloads only).
type ParsedBox struct {
	Type   string
	Offset int64
	Box    gomp4.IBox
	Raw    []byte
}

// Fragment is one parsed moof+mdat pair.
type Fragment struct {
	Seq     uint32
	TrackID uint32
	TFDT    int64
	Samples []Sample
	Boxes   []BoxInfo
	Parsed  []ParsedBox
	Raw     []byte
}

// TrackInfo carries the per-track data the pipeline needs from init.
type TrackInfo struct {
	TrackID               uint32
	Timescale             uint32
	DefaultSampleDuration uint32
	DefaultSampleSize     uint32
	DefaultSampleFlags    uint32
}

// InitInfo is the parsed init segment.
type InitInfo struct {
	Tracks map[uint32]*TrackInfo
}

// Track returns the info for id.
func (i *InitInfo) Track(id uint32) (*TrackInfo, bool) {
	t, ok := i.Tracks[id]
	return t, ok
}

var containerTypes = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true,
	"stbl": true, "moof": true, "traf": true, "mvex": true,
	"edts": true, "dinf": true,
}

type boxHeader struct {
	typ    string
	offset int64
	size   int64
	hdr    int64
}

// walk validates box lengths and nesting, invoking fn for every box.
func walk(data []byte, fn func(h boxHeader, payload []byte) error) error {
	var rec func(off, end int64) error
	rec = func(off, end int64) error {
		for off < end {
			if end-off < 8 {
				return &CorruptError{off, "truncated box header"}
			}
			size := int64(binary.BigEndian.Uint32(data[off:]))
			typ := string(data[off+4 : off+8])
			hdr := int64(8)
			switch size {
			case 1:
				if end-off < 16 {
					return &CorruptError{off, "truncated extended size"}
				}
				size = int64(binary.BigEndian.Uint64(data[off+8:]))
				hdr = 16
			case 0:
				size = end - off
			}
			if size < hdr {
				return &CorruptError{off, fmt.Sprintf("invalid box size %d", size)}
			}
			if off+size > end {
				return &CorruptError{off, fmt.Sprintf("box %s size %d exceeds containing box", typ, size)}
			}
			payload := data[off+hdr : off+size]
			if err := fn(boxHeader{typ, off, size, hdr}, payload); err != nil {
				return err
			}
			if containerTypes[typ] {
				if err := rec(off+hdr, off+size); err != nil {
					return err
				}
			}
			off += size
		}
		return nil
	}
	return rec(0, int64(len(data)))
}

func unmarshal(payload []byte, dst gomp4.IBox) error {
	_, err := gomp4.Unmarshal(bytes.NewReader(payload), uint64(len(payload)), dst, gomp4.Context{})
	return err
}
