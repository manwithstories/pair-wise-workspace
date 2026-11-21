package metadata

import (
	"fmt"

	"github.com/abema/go-mp4"
)

// The fixtures use two boxes that go-mp4 does not define:
//
//	clap  clean aperture (ISO/IEC 14496-12 12.1.4.2), unsigned 32.32 rationals
//	ccol  studio container-level colour descriptor, stored once under moov
//
// Both are registered with the library so the same code writes and reads them.

func init() {
	mp4.AddBoxDef(&ClapBox{})
	mp4.AddBoxDef(&CColBox{})
}

// ClapBox is the clean-aperture box.
type ClapBox struct {
	mp4.Box
	WidthN  uint32 `mp4:"0,size=32"`
	WidthD  uint32 `mp4:"1,size=32"`
	HeightN uint32 `mp4:"2,size=32"`
	HeightD uint32 `mp4:"3,size=32"`
	HorizN  uint32 `mp4:"4,size=32"`
	HorizD  uint32 `mp4:"5,size=32"`
	VertN   uint32 `mp4:"6,size=32"`
	VertD   uint32 `mp4:"7,size=32"`
}

func (*ClapBox) GetType() mp4.BoxType { return mp4.StrToBoxType("clap") }

// NewClap encodes a crop as integers with unit denominators so that parsing
// is exact for integer pixel windows.
func NewClap(c Crop) *ClapBox {
	return &ClapBox{
		WidthN: uint32(c.Width), WidthD: 1,
		HeightN: uint32(c.Height), HeightD: 1,
		HorizN: uint32(c.X), HorizD: 1,
		VertN: uint32(c.Y), VertD: 1,
	}
}

// Crop converts the aperture back to pixel coordinates, rejecting fractional
// rationals rather than rounding them.
func (b *ClapBox) Crop(trackID int) (Crop, error) {
	if b.WidthD != 1 || b.HeightD != 1 || b.HorizD != 1 || b.VertD != 1 {
		return Crop{}, fmt.Errorf("metadata: track %d clap has non-integer rationals %d/%d %d/%d %d/%d %d/%d",
			trackID, b.WidthN, b.WidthD, b.HeightN, b.HeightD, b.HorizN, b.HorizD, b.VertN, b.VertD)
	}
	return Crop{X: int(b.HorizN), Y: int(b.VertN), Width: int(b.WidthN), Height: int(b.HeightN)}, nil
}

// CColBox carries a container-wide nclx colour description.
type CColBox struct {
	mp4.Box
	Primaries     uint16 `mp4:"0,size=16"`
	Transfer      uint16 `mp4:"1,size=16"`
	Matrix        uint16 `mp4:"2,size=16"`
	FullRangeFlag uint8  `mp4:"3,size=8"`
	Reserved      uint32 `mp4:"4,size=24"`
	Source        string `mp4:"5,size=8,string"`
}

func (*CColBox) GetType() mp4.BoxType { return mp4.StrToBoxType("ccol") }

// NewCCol builds the container descriptor from a ColorDesc.
func NewCCol(d ColorDesc) *CColBox {
	b := &CColBox{Primaries: 2, Transfer: 2, Matrix: 2, FullRangeFlag: 2, Source: "container"}
	if d.Primaries != nil {
		b.Primaries = *d.Primaries
	}
	if d.Transfer != nil {
		b.Transfer = *d.Transfer
	}
	if d.Matrix != nil {
		b.Matrix = *d.Matrix
	}
	if d.FullRange != nil && *d.FullRange {
		b.FullRangeFlag = 1
	} else if d.FullRange != nil {
		b.FullRangeFlag = 0
	}
	return b
}

// Descriptor converts the box back to a ColorDesc, treating unspecified
// values and an unspecified range as absent.
func (b *CColBox) Descriptor() ColorDesc {
	var r *bool
	switch b.FullRangeFlag {
	case 0:
		f := false
		r = &f
	case 1:
		t := true
		r = &t
	}
	return NewColorDesc(b.Primaries, b.Transfer, b.Matrix, r, "container")
}
