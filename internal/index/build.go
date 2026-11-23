// Package index builds and commits the random-access index. The builder
// consumes repaired samples in decode order and emits one entry per
// keyframe, recording program decode/presentation time, byte offset and
// the preceding decode point. Samples beyond the known media size are
// truncated input: they raise a locatable corrupt error and never produce
// out-of-range index entries.
package index

import (
	"archive/internal/media"
	"archive/internal/timeline"
)

// Entry is one random-access point, keyed by decode time.
type Entry struct {
	Track      string
	Seq        int    // decode-order index of the keyframe sample
	DTSus      int64  // program decode time (µs)
	PTSus      int64  // program presentation time (µs)
	ByteOffset uint64 // absolute byte offset in the media file
	Size       uint32
	PrevDTSus  int64 // program decode time of the previous keyframe
}

// Builder streams samples of one track in decode order.
type Builder struct {
	rt         *timeline.RepairedTrack
	availBytes uint64
	entries    []Entry
	lastKeyUs  int64
	hasKey     bool
	truncated  bool
}

// NewBuilder creates a builder for a repaired track. availBytes is the
// actual media size; any sample reaching past it is truncation.
func NewBuilder(rt *timeline.RepairedTrack, availBytes uint64) *Builder {
	return &Builder{rt: rt, availBytes: availBytes}
}

// Add ingests one sample (decode order). byteOffset is the sample's
// absolute offset in the media file. Keyframes inside unlocatable ranges
// are skipped rather than fabricated.
func (b *Builder) Add(s timeline.Sample, byteOffset uint64) error {
	if b.truncated {
		return media.Corruptf("index.build", b.rt.TrackID, int64(s.Index),
			"sample follows truncated media")
	}
	end := byteOffset + uint64(s.Size)
	if end > b.availBytes || end < byteOffset {
		b.truncated = true
		return media.Corruptf("index.build", b.rt.TrackID, int64(byteOffset),
			"truncated sample: byte range exceeds media size")
	}
	if !s.Keyframe || b.rt.InUnlocatable(s.DTS) {
		return nil
	}
	dts, err := b.rt.ProgramMicros(s.Index)
	if err != nil {
		return err
	}
	pts, err := b.rt.ProgramMicrosOf(s.CTS, int64(s.Index))
	if err != nil {
		return err
	}
	prev := int64(-1)
	if b.hasKey {
		prev = b.lastKeyUs
	}
	b.entries = append(b.entries, Entry{
		Track: b.rt.TrackID, Seq: s.Index, DTSus: dts, PTSus: pts,
		ByteOffset: byteOffset, Size: s.Size, PrevDTSus: prev,
	})
	b.lastKeyUs, b.hasKey = dts, true
	return nil
}

// Entries returns the built entries in decode-time order.
func (b *Builder) Entries() []Entry { return b.entries }

// Truncated reports whether the builder hit truncated media.
func (b *Builder) Truncated() bool { return b.truncated }
