package fixture

import (
	"encoding/binary"

	"github.com/abema/go-mp4"
)

func identityMatrix() [9]int32 {
	return [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
}

func rotationMatrix(deg int) [9]int32 {
	// MP4 stores a counter-clockwise rotation in the transform matrix, so a
	// file tagged "90 degrees clockwise" carries cos=0, sin=-1.
	switch deg {
	case 90:
		return [9]int32{0, -0x00010000, 0, 0x00010000, 0, 0, 0, 0, 0x40000000}
	case 180:
		return [9]int32{-0x00010000, 0, 0, 0, -0x00010000, 0, 0, 0, 0x40000000}
	case 270:
		return [9]int32{0, 0x00010000, 0, -0x00010000, 0, 0, 0, 0, 0x40000000}
	default:
		return identityMatrix()
	}
}

func marshalBox(w *mp4.Writer, box mp4.IImmutableBox) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: box.GetType()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, box, mp4.Context{}); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}

func buildStsd(w *mp4.Writer, t TrackSpec) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsd()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, &mp4.Stsd{EntryCount: 1}, mp4.Context{}); err != nil {
		return err
	}
	if t.Kind == "audio" {
		if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMp4a()}); err != nil {
			return err
		}
		// A plain ISO audio entry: the QuickTime dynamic fields stay disabled
		// because the writer context is not QuickTime-compatible.
		if _, err := mp4.Marshal(w, &mp4.AudioSampleEntry{
			SampleEntry:  mp4.SampleEntry{AnyTypeBox: mp4.AnyTypeBox{Type: mp4.BoxTypeMp4a()}, DataReferenceIndex: 1},
			ChannelCount: 2,
			SampleSize:   16,
			SampleRate:   t.Timescale << 16,
		}, mp4.Context{}); err != nil {
			return err
		}
		if _, err := w.EndBox(); err != nil {
			return err
		}
	} else {
		if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeAvc1()}); err != nil {
			return err
		}
		if _, err := mp4.Marshal(w, &mp4.VisualSampleEntry{
			SampleEntry: mp4.SampleEntry{AnyTypeBox: mp4.AnyTypeBox{Type: mp4.BoxTypeAvc1()}, DataReferenceIndex: 1},
			Width:       t.Width,
			Height:      t.Height,
			Compressorname: func() [32]byte {
				var c [32]byte
				c[0] = 6
				copy(c[1:], "mediad")
				return c
			}(),
			Depth: 24,
		}, mp4.Context{}); err != nil {
			return err
		}
		if t.CropLeft+t.CropRight+t.CropTop+t.CropBottom > 0 {
			if err := writeClap(w, t); err != nil {
				return err
			}
		}
		for _, c := range []*ColorSpec{t.Color, t.ConflictColor} {
			if c == nil {
				continue
			}
			if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeColr()}); err != nil {
				return err
			}
			if _, err := mp4.Marshal(w, &mp4.Colr{
				ColourType:              [4]byte{'n', 'c', 'l', 'x'},
				ColourPrimaries:         c.Primaries,
				TransferCharacteristics: c.Transfer,
				MatrixCoefficients:      c.Matrix,
				FullRangeFlag:           c.FullRange,
			}, mp4.Context{}); err != nil {
				return err
			}
			if _, err := w.EndBox(); err != nil {
				return err
			}
		}
		if _, err := w.EndBox(); err != nil {
			return err
		}
	}
	if _, err := w.EndBox(); err != nil {
		return err
	}
	return nil
}

func writeClap(w *mp4.Writer, t TrackSpec) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.StrToBoxType("clap")}); err != nil {
		return err
	}
	cw := int32(t.Width) - int32(t.CropLeft) - int32(t.CropRight)
	ch := int32(t.Height) - int32(t.CropTop) - int32(t.CropBottom)
	hOff := (int32(t.CropLeft) - int32(t.CropRight))
	vOff := (int32(t.CropTop) - int32(t.CropBottom))
	// Offsets in clap are in units of half a pixel horizontally and half a
	// line vertically relative to the frame centre.
	vals := [8]int32{cw, 1, ch, 1, hOff, 2, vOff, 2}
	buf := make([]byte, 32)
	for i, v := range vals {
		binary.BigEndian.PutUint32(buf[i*4:], uint32(v))
	}
	if _, err := w.Write(buf); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}
