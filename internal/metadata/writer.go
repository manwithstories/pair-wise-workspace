// Package metadata parses MP4 media metadata with go-mp4. The package also
// ships a minimal in-memory MP4 writer used by tests and the demo so that no
// external sample files are required.
package metadata

import (
	"bytes"
	"encoding/binary"
	"io"

	mp4 "github.com/abema/go-mp4"
)

// VideoTrackSpec describes the single video track embedded in a fixture MP4.
type VideoTrackSpec struct {
	TrackID   uint32
	Timescale uint32
	Width     uint16
	Height    uint16
	Duration  uint64
	Brand     [4]byte
}

// box is a typed byte buffer; nested boxes are emitted in insertion order.
type box struct {
	typ     mp4.BoxType
	payload []byte
	kids    []*box
}

func (b *box) size() uint64 {
	n := uint64(8 + len(b.payload))
	for _, k := range b.kids {
		n += k.size()
	}
	return n
}

func (b *box) writeTo(w io.Writer) error {
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr, uint32(b.size()))
	copy(hdr[4:8], b.typ[:])
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	if _, err := w.Write(b.payload); err != nil {
		return err
	}
	for _, k := range b.kids {
		if err := k.writeTo(w); err != nil {
			return err
		}
	}
	return nil
}

func raw(typ mp4.BoxType, payload []byte) *box { return &box{typ: typ, payload: payload} }

func container(typ mp4.BoxType, kids ...*box) *box {
	return &box{typ: typ, kids: kids}
}

// v0Full is a version/flags prefix for full boxes.
func v0Full() []byte { return []byte{0, 0, 0, 0} }

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func u16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

// dur32 emits the version-0 32-bit duration field.
func dur32(v uint64) []byte { return u32(uint32(v)) }

// WriteMinimalMP4 emits a ftyp + moov structure sufficient for go-mp4's
// Probe: one video trak with tkhd/mdhd and an stbl carrying stco/stts/stsc/
// stsz, plus a trailing mdat so FastStart detection has data to see.
func WriteMinimalMP4(w io.Writer, spec VideoTrackSpec, sampleCount uint32) error {
	if spec.Brand == [4]byte{} {
		spec.Brand = [4]byte{'m', 'p', 'i', 'p'}
	}
	ftyp := raw(mp4.BoxTypeFtyp(), bytes.Join([][]byte{
		spec.Brand[:], u32(0), spec.Brand[:],
	}, nil))

	mvhd := raw(mp4.BoxTypeMvhd(), bytes.Join([][]byte{
		v0Full(),
		u32(0), u32(0), // creation/modification time
		u32(spec.Timescale), dur32(spec.Duration),
		make([]byte, 80), // rate/volume/matrix/predefined/next track id region
	}, nil))

	tkhd := raw(mp4.BoxTypeTkhd(), bytes.Join([][]byte{
		v0Full(),
		u32(0), u32(0),
		u32(spec.TrackID), u32(0),
		dur32(spec.Duration),
		make([]byte, 60),
	}, nil))

	mdhd := raw(mp4.BoxTypeMdhd(), bytes.Join([][]byte{
		v0Full(),
		u32(0), u32(0),
		u32(spec.Timescale), dur32(spec.Duration),
		make([]byte, 4), // language + pre-defined
	}, nil))

	hdlr := raw(mp4.BoxTypeHdlr(), bytes.Join([][]byte{
		v0Full(),
		make([]byte, 4),
		{'v', 'i', 'd', 'e'},
		make([]byte, 12),
		{0},
	}, nil))

	avc1 := raw(mp4.BoxTypeAvc1(), bytes.Join([][]byte{
		make([]byte, 6), u16(1),
		make([]byte, 16),
		u16(spec.Width), u16(spec.Height),
		make([]byte, 50),
	}, nil))
	// Baseline 3.0 (profile 66 / level 30), 4-byte NAL length, zero SPS/PPS
	// entries: a valid decoder-configuration shell for metadata-only probing.
	avcC := raw(mp4.BoxTypeAvcC(), []byte{
		1,    // configurationVersion
		66,   // AVC profile (baseline)
		0,    // profile compatibility
		30,   // level
		0xFF, // reserved 0b111111 + lengthSizeMinusOne=3
		0xE0, // reserved 0b111 + numSPS=0
		0,    // numPPS=0
	})
	avc1.kids = append(avc1.kids, avcC)
	// stsd is a full box (version/flags + entry count) wrapping avc1.
	stsd := &box{
		typ:     mp4.BoxTypeStsd(),
		payload: append(v0Full(), u32(1)...),
		kids:    []*box{avc1},
	}

	// One chunk holds all samples; a zero offset is structurally valid for a
	// metadata-only fixture because the demo never decodes the mdat payload.
	stco := raw(mp4.BoxTypeStco(), bytes.Join([][]byte{
		v0Full(), u32(1), u32(0),
	}, nil))
	stts := raw(mp4.BoxTypeStts(), bytes.Join([][]byte{
		v0Full(), u32(1), u32(sampleCount), u32(1),
	}, nil))
	stsc := raw(mp4.BoxTypeStsc(), bytes.Join([][]byte{
		v0Full(), u32(1), u32(1), u32(sampleCount), u32(1),
	}, nil))
	// Uniform sample size of one byte each keeps EntrySize empty and valid.
	stsz := raw(mp4.BoxTypeStsz(), bytes.Join([][]byte{
		v0Full(), u32(1), u32(sampleCount),
	}, nil))
	stbl := container(mp4.BoxTypeStbl(), stsd, stts, stsc, stsz, stco)
	minf := container(mp4.BoxTypeMinf(), stbl)
	mdia := container(mp4.BoxTypeMdia(), mdhd, hdlr, minf)
	trak := container(mp4.BoxTypeTrak(), tkhd, mdia)
	moov := container(mp4.BoxTypeMoov(), mvhd, trak)
	mdat := raw(mp4.BoxTypeMdat(), make([]byte, 16))

	for _, b := range []*box{ftyp, moov, mdat} {
		if err := b.writeTo(w); err != nil {
			return err
		}
	}
	return nil
}

// Marshal writes a complete MP4 into a new byte slice.
func Marshal(spec VideoTrackSpec, sampleCount uint32) ([]byte, error) {
	var buf bytes.Buffer
	if err := WriteMinimalMP4(&buf, spec, sampleCount); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
