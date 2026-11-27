// Package mp4 fragment parsing: mfhd sequence numbers, tfdt base decode
// times and trun sample tables. All offsets are validated before use.
package mp4

import (
	"encoding/binary"

	"pairwise/ingestcheck/internal/clock"
)

// FullBoxFlags carries the version and 24-bit flags of a full box.
type FullBoxFlags struct {
	Version uint8
	Flags   uint32
}

// Mfhd is the movie fragment header.
type Mfhd struct {
	FullBoxFlags
	SequenceNumber uint32
	Box            *Box
}

// Tfdt is the track fragment decode-time box.
type Tfdt struct {
	FullBoxFlags
	BaseDecodeTime uint64
	Box            *Box
}

// Trun is a track fragment run. Sample entries carry their durations, sizes,
// flags and composition offsets when present.
type Trun struct {
	FullBoxFlags
	SampleCount        uint32
	DataOffset         int32
	HasDataOffset      bool
	FirstSampleFlags   uint32
	HasFirstSampleFlag bool
	Samples            []Sample
	Box                *Box
}

// SampleCaptures what a trun entry reveals about a media sample.
type SampleCaptures struct {
	Duration        uint32
	Size            uint32
	Flags           uint32
	CompositionTime int32
}

// Sample is one trun entry. Precisely one field is populated per entry by
// the fixture builder; this keeps the model simple while preserving the
// original flags used for the keyframe index.
type Sample struct {
	Duration        uint32
	Size            uint32
	Flags           uint32
	CompositionTime int32
}

const (
	trunDataOffset      = 0x000001
	trunFirstSampleFlag = 0x000004
	trunSampleDuration  = 0x000100
	trunSampleSize      = 0x000200
	trunSampleFlags     = 0x000400
	trunSampleCTO       = 0x000800

	sampleIsNonSync = 0x0001
)

// ParseMfhd decodes an mfhd box payload.
func ParseMfhd(b *Box) (*Mfhd, error) {
	if b == nil || b.Type != "mfhd" {
		return nil, clock.At(clock.CodeFragmentMissing, "mfhd", 0, "mfhd box absent")
	}
	p := b.Payload
	if len(p) < 8 {
		return nil, clock.At(clock.CodeFragmentTruncated, "mfhd", b.Offset+b.HeaderSize, "mfhd smaller than 8 bytes")
	}
	m := &Mfhd{Box: b}
	m.Version = p[0]
	m.Flags = be24(p[1:4])
	m.SequenceNumber = binary.BigEndian.Uint32(p[4:8])
	return m, nil
}

// ParseTfdt decodes a tfdt box payload for versions 0 and 1.
func ParseTfdt(b *Box) (*Tfdt, error) {
	if b == nil || b.Type != "tfdt" {
		return nil, clock.At(clock.CodeFragmentMissing, "tfdt", 0, "tfdt box absent")
	}
	p := b.Payload
	t := &Tfdt{Box: b}
	if len(p) < 4 {
		return nil, clock.At(clock.CodeFragmentTruncated, "tfdt", b.Offset+b.HeaderSize, "tfdt smaller than 4 bytes")
	}
	t.Version = p[0]
	t.Flags = be24(p[1:4])
	switch t.Version {
	case 0:
		if len(p) < 8 {
			return nil, clock.At(clock.CodeFragmentTruncated, "tfdt", b.Offset+b.HeaderSize, "tfdt v0 smaller than 8 bytes")
		}
		t.BaseDecodeTime = uint64(binary.BigEndian.Uint32(p[4:8]))
	case 1:
		if len(p) < 12 {
			return nil, clock.At(clock.CodeFragmentTruncated, "tfdt", b.Offset+b.HeaderSize, "tfdt v1 smaller than 12 bytes")
		}
		t.BaseDecodeTime = binary.BigEndian.Uint64(p[4:12])
	default:
		return nil, clock.At(clock.CodeBoxBounds, "tfdt", b.Offset+b.HeaderSize, "unsupported tfdt version")
	}
	return t, nil
}

// ParseTrun decodes a trun box. The loop validates every field offset against
// the payload so a truncated run reports the first missing byte.
func ParseTrun(b *Box) (*Trun, error) {
	if b == nil || b.Type != "trun" {
		return nil, clock.At(clock.CodeFragmentMissing, "trun", 0, "trun box absent")
	}
	p := b.Payload
	base := b.Offset + b.HeaderSize
	r := &Trun{Box: b}
	if len(p) < 8 {
		return nil, clock.At(clock.CodeFragmentTruncated, "trun", base, "trun smaller than 8 bytes")
	}
	r.Version = p[0]
	r.Flags = be24(p[1:4])
	r.SampleCount = binary.BigEndian.Uint32(p[4:8])
	if r.SampleCount > 4096 {
		return nil, clock.At(clock.CodeBoxBounds, "trun", base+4, "sample count exceeds bound")
	}
	pos := 8
	if r.Flags&trunDataOffset != 0 {
		if pos+4 > len(p) {
			return nil, clock.At(clock.CodeFragmentTruncated, "trun", base+int64(pos), "trun data offset truncated")
		}
		r.HasDataOffset = true
		r.DataOffset = int32(binary.BigEndian.Uint32(p[pos : pos+4]))
		pos += 4
	}
	if r.Flags&trunFirstSampleFlag != 0 {
		if pos+4 > len(p) {
			return nil, clock.At(clock.CodeFragmentTruncated, "trun", base+int64(pos), "trun first sample flags truncated")
		}
		r.HasFirstSampleFlag = true
		r.FirstSampleFlags = binary.BigEndian.Uint32(p[pos : pos+4])
		pos += 4
	}
	for i := uint32(0); i < r.SampleCount; i++ {
		var s Sample
		if r.Flags&trunSampleDuration != 0 {
			if pos+4 > len(p) {
				return nil, clock.At(clock.CodeFragmentTruncated, "trun", base+int64(pos), "sample duration truncated")
			}
			s.Duration = binary.BigEndian.Uint32(p[pos : pos+4])
			pos += 4
		}
		if r.Flags&trunSampleSize != 0 {
			if pos+4 > len(p) {
				return nil, clock.At(clock.CodeFragmentTruncated, "trun", base+int64(pos), "sample size truncated")
			}
			s.Size = binary.BigEndian.Uint32(p[pos : pos+4])
			pos += 4
		}
		if r.Flags&trunSampleFlags != 0 {
			if pos+4 > len(p) {
				return nil, clock.At(clock.CodeFragmentTruncated, "trun", base+int64(pos), "sample flags truncated")
			}
			s.Flags = binary.BigEndian.Uint32(p[pos : pos+4])
			pos += 4
		}
		if r.Flags&trunSampleCTO != 0 {
			if pos+4 > len(p) {
				return nil, clock.At(clock.CodeFragmentTruncated, "trun", base+int64(pos), "composition offset truncated")
			}
			s.CompositionTime = int32(binary.BigEndian.Uint32(p[pos : pos+4]))
			pos += 4
		}
		r.Samples = append(r.Samples, s)
	}
	if pos != len(p) {
		return nil, clock.At(clock.CodeBoxBounds, "trun", base+int64(pos), "trun payload longer than declared samples")
	}
	return r, nil
}

func be24(b []byte) uint32 {
	return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
}

// ParseFragment decodes a moof box into its fragment header, decode time and
// runs. A fragment with no mfhd or tfdt is rejected with the first bad byte.
func ParseFragment(moof *Box) (*Fragment, error) {
	if moof == nil || moof.Type != "moof" {
		return nil, clock.At(clock.CodeFragmentMissing, "moof", 0, "moof box absent")
	}
	f := &Fragment{}
	if hb := Find(moof.Children, "mfhd"); hb != nil {
		m, err := ParseMfhd(hb)
		if err != nil {
			return nil, err
		}
		f.Mfhd = m
		f.Sequence = m.SequenceNumber
	} else {
		return nil, clock.At(clock.CodeFragmentMissing, "moof", moof.Offset, "mfhd missing")
	}
	if db := Find(moof.Children, "tfdt"); db != nil {
		t, err := ParseTfdt(db)
		if err != nil {
			return nil, err
		}
		f.Tfdt = t
		f.BaseTime = t.BaseDecodeTime
	} else {
		return nil, clock.At(clock.CodeFragmentMissing, "moof", moof.Offset, "tfdt missing")
	}
	for _, tb := range FindAll(moof.Children, "trun") {
		r, err := ParseTrun(tb)
		if err != nil {
			return nil, err
		}
		f.Truns = append(f.Truns, r)
	}
	if len(f.Truns) == 0 {
		return nil, clock.At(clock.CodeFragmentMissing, "moof", moof.Offset, "trun missing")
	}
	return f, nil
}
