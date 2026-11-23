package ingest

import (
	"bytes"
	"encoding/binary"

	mp4 "github.com/abema/go-mp4"
)

// RawBox carries an arbitrary box whose payload is kept verbatim. It is
// registered for avcC, which go-mp4 v1.3.0 does not model. Variable
// length means it must only be used for leaf boxes.
type RawBox struct {
	mp4.AnyTypeBox
	Data []byte `mp4:"0,size=8"`
}

// fixedBox reads exactly PrefixLen payload bytes and leaves any
// remaining bytes (child boxes) for the walker to expand. Concrete
// types below repeat the Data field with a dynamic length so that
// go-mp4 calls the concrete type's GetFieldLength on read.
type fixedBox struct {
	mp4.AnyTypeBox
	PrefixLen int
}

// Concrete sample-entry types with their fixed-prefix sizes (ISO 14496-12).
type avc1Box struct {
	mp4.AnyTypeBox
	Data []byte `mp4:"0,size=8,len=dynamic"`
}

type mp4aBox struct {
	mp4.AnyTypeBox
	Data []byte `mp4:"0,size=8,len=dynamic"`
}

type clapBox struct {
	mp4.AnyTypeBox
	Data []byte `mp4:"0,size=8,len=dynamic"`
}

func (*avc1Box) GetFieldLength(string, mp4.Context) uint { return 78 }
func (*mp4aBox) GetFieldLength(string, mp4.Context) uint { return 28 }
func (*clapBox) GetFieldLength(string, mp4.Context) uint { return 32 }

func (b *avc1Box) Payload() []byte { return b.Data }
func (b *mp4aBox) Payload() []byte { return b.Data }
func (b *clapBox) Payload() []byte { return b.Data }

func newFixed(t string, _ int, data []byte) mp4.IImmutableBox {
	switch t {
	case "avc1":
		return &avc1Box{AnyTypeBox: mp4.AnyTypeBox{Type: mp4.StrToBoxType(t)}, Data: data}
	case "mp4a":
		return &mp4aBox{AnyTypeBox: mp4.AnyTypeBox{Type: mp4.StrToBoxType(t)}, Data: data}
	default:
		return &clapBox{AnyTypeBox: mp4.AnyTypeBox{Type: mp4.StrToBoxType(t)}, Data: data}
	}
}

func init() {
	mp4.AddAnyTypeBoxDef(&RawBox{}, mp4.StrToBoxType("avcC"))
	mp4.AddAnyTypeBoxDef(&avc1Box{}, mp4.StrToBoxType("avc1"))
	mp4.AddAnyTypeBoxDef(&mp4aBox{}, mp4.StrToBoxType("mp4a"))
	mp4.AddAnyTypeBoxDef(&clapBox{}, mp4.StrToBoxType("clap"))
}

// FixtureSpec describes one generated MP4 test input.
type FixtureSpec struct {
	Path              string
	Width, Height     uint32
	VideoTimescale    uint32
	VideoSamples      uint32
	KeyframeEvery     uint32 // 0 means every sample is a keyframe
	ContainerRotation Rotation
	TrackRotation     Rotation
	ContainerCrop     *Crop
	TrackCrop         *Crop
	ContainerColor    *Color
	TrackColor        *Color
	AudioSampleRate   uint32
	AudioChannels     uint16
	AudioLayout       []string
	// Damage selects a deliberate corruption.
	Damage FixtureDamage
}

// FixtureDamage enumerates the supported corruption modes.
type FixtureDamage string

const (
	DamageNone             FixtureDamage = ""
	DamageTruncated        FixtureDamage = "truncated"
	DamageBadSampleTable   FixtureDamage = "bad-sample-table"
	DamageMissingKeyframes FixtureDamage = "missing-keyframes"
	DamageBadLayout        FixtureDamage = "bad-audio-layout"
)

// matrixFor maps a rotation to the fixed-point 2.30 display matrix.
func matrixFor(r Rotation) [9]int32 {
	identity := [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}
	switch r {
	case 90:
		return [9]int32{0, 0x10000, 0, -0x10000, 0, 0, 0, 0, 0x40000000}
	case 180:
		return [9]int32{-0x10000, 0, 0, 0, -0x10000, 0, 0, 0, 0x40000000}
	case 270:
		return [9]int32{0, -0x10000, 0, 0x10000, 0, 0, 0, 0, 0x40000000}
	default:
		return identity
	}
}

func boxType(s string) mp4.BoxType { return mp4.StrToBoxType(s) }

// clapPayload builds the clean-aperture payload (8 int32 values).
func clapPayload(c *Crop) []byte {
	buf := bytes.NewBuffer(nil)
	for _, v := range []int32{
		int32(c.Width), int32(1), int32(c.Height), int32(1),
		int32(c.X), int32(1), int32(c.Y), int32(1),
	} {
		_ = binary.Write(buf, binary.BigEndian, v)
	}
	return buf.Bytes()
}

func colrBox(c *Color) *mp4.Colr {
	if c.ICCProfile {
		return &mp4.Colr{
			ColourType: [4]byte{'r', 'I', 'C', 'C'},
			Profile:    []byte{0x70, 0x72, 0x6f, 0x66},
		}
	}
	return &mp4.Colr{
		ColourType:              [4]byte{'n', 'c', 'l', 'x'},
		ColourPrimaries:         c.Primaries,
		TransferCharacteristics: c.Transfer,
		MatrixCoefficients:      c.Matrix,
		FullRangeFlag:           true,
	}
}

// avc1Payload builds a minimal visual sample entry; child boxes are
// attached by the caller through the box hierarchy.
func avc1Payload(spec FixtureSpec) []byte {
	buf := bytes.NewBuffer(nil)
	// 6 reserved bytes + 2 data_reference_index
	buf.Write(make([]byte, 6))
	_ = binary.Write(buf, binary.BigEndian, uint16(1))
	_ = binary.Write(buf, binary.BigEndian, uint16(0)) // pre_defined
	_ = binary.Write(buf, binary.BigEndian, uint16(0)) // reserved
	buf.Write(make([]byte, 12)) // pre_defined[3]
	_ = binary.Write(buf, binary.BigEndian, uint16(spec.Width))
	_ = binary.Write(buf, binary.BigEndian, uint16(spec.Height))
	_ = binary.Write(buf, binary.BigEndian, uint32(0x00480000)) // h/v res
	_ = binary.Write(buf, binary.BigEndian, uint32(0x00480000))
	_ = binary.Write(buf, binary.BigEndian, uint32(0)) // reserved
	_ = binary.Write(buf, binary.BigEndian, uint16(1)) // frame count
	buf.Write(make([]byte, 32))                        // compressor name
	_ = binary.Write(buf, binary.BigEndian, uint16(0x0018)) // depth
	_ = binary.Write(buf, binary.BigEndian, int16(-1))      // pre_defined
	return buf.Bytes()
}

func mp4aPayload(sampleRate uint32, channels uint16) []byte {
	buf := bytes.NewBuffer(nil)
	buf.Write(make([]byte, 6))
	_ = binary.Write(buf, binary.BigEndian, uint16(1))
	buf.Write(make([]byte, 8))
	_ = binary.Write(buf, binary.BigEndian, channels)
	_ = binary.Write(buf, binary.BigEndian, uint16(16)) // sample size
	buf.Write(make([]byte, 4))
	_ = binary.Write(buf, binary.BigEndian, uint32(sampleRate<<16)) // fixed point
	return buf.Bytes()
}
