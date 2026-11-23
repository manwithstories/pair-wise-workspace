package fixture

import (
	"github.com/abema/go-mp4"
)

func writeFtyp(mw *mp4.Writer) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		return err
	}
	ftyp := &mp4.Ftyp{
		MajorBrand: [4]byte{'i', 's', 'o', 'm'},
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
			{CompatibleBrand: [4]byte{'m', 'r', 'o', 'u'}},
		},
	}
	if _, err := mp4.Marshal(mw, ftyp, mp4.Context{}); err != nil {
		return err
	}
	_, err := mw.EndBox()
	return err
}

func writeContainerColr(mw *mp4.Writer, c [3]uint16) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeColr()}); err != nil {
		return err
	}
	colr := &mp4.Colr{
		ColourType:              [4]byte{'n', 'c', 'l', 'x'},
		ColourPrimaries:         c[0],
		TransferCharacteristics: c[1],
		MatrixCoefficients:      c[2],
	}
	if _, err := mp4.Marshal(mw, colr, mp4.Context{}); err != nil {
		return err
	}
	_, err := mw.EndBox()
	return err
}

func (s Spec) writeMoov(mw *mp4.Writer) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()}); err != nil {
		return err
	}
	if err := s.writeTkhd(mw); err != nil {
		return err
	}
	if err := s.writeMdia(mw); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil { // trak
		return err
	}
	if _, err := mw.EndBox(); err != nil { // moov
		return err
	}
	return nil
}

func (s Spec) writeTkhd(mw *mp4.Writer) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTkhd()}); err != nil {
		return err
	}
	duration := uint64(s.SampleCount) * uint64(s.FrameDelta)
	tk := &mp4.Tkhd{
		TrackID: 1, DurationV1: duration,
		Width: s.Width << 16, Height: s.Height << 16,
		Matrix: rotationMatrix(s.Rotation),
	}
	if _, err := mp4.Marshal(mw, tk, mp4.Context{}); err != nil {
		return err
	}
	_, err := mw.EndBox()
	return err
}

func rotationMatrix(deg int) [9]int32 {
	const one = 1 << 16
	switch deg {
	case 90:
		return [9]int32{0, one, 0, -one, 0, 0, 0, 0, one}
	case 180:
		return [9]int32{-one, 0, 0, 0, -one, 0, 0, 0, one}
	case 270:
		return [9]int32{0, -one, 0, one, 0, 0, 0, 0, one}
	default:
		return [9]int32{one, 0, 0, 0, one, 0, 0, 0, one}
	}
}

func (s Spec) writeMdia(mw *mp4.Writer) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia()}); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdhd()}); err != nil {
		return err
	}
	scale := s.Timescale
	if s.BadTimeBase {
		scale = 0
	}
	md := &mp4.Mdhd{
		Timescale:  scale,
		DurationV1: uint64(s.SampleCount) * uint64(s.FrameDelta),
	}
	if _, err := mp4.Marshal(mw, md, mp4.Context{}); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil {
		return err
	}
	if err := writeHdlr(mw); err != nil {
		return err
	}
	if err := s.writeMinf(mw); err != nil {
		return err
	}
	_, err := mw.EndBox() // mdia
	return err
}

func writeHdlr(mw *mp4.Writer) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeHdlr()}); err != nil {
		return err
	}
	h := &mp4.Hdlr{
		HandlerType: [4]byte{'v', 'i', 'd', 'e'},
		Name:        "mediaroute-fixture",
	}
	if _, err := mp4.Marshal(mw, h, mp4.Context{}); err != nil {
		return err
	}
	_, err := mw.EndBox()
	return err
}

// rawBox writes an unregistered fourCC box with raw payload, used for the
// clap clean-aperture box which go-mp4 v1.3.0 does not model.
func rawBox(mw *mp4.Writer, fourCC string, payload []byte) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.StrToBoxType(fourCC)}); err != nil {
		return err
	}
	if _, err := mw.Write(payload); err != nil {
		return err
	}
	_, err := mw.EndBox()
	return err
}
