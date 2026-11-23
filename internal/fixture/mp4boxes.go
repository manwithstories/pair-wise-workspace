package fixture

import (
	"encoding/binary"

	"github.com/abema/go-mp4"
)

func (s Spec) writeMinf(mw *mp4.Writer) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMinf()}); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeVmhd()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(mw, &mp4.Vmhd{}, mp4.Context{}); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil {
		return err
	}
	if err := s.writeDinf(mw); err != nil {
		return err
	}
	if err := s.writeStbl(mw); err != nil {
		return err
	}
	_, err := mw.EndBox()
	return err
}

func (s Spec) writeDinf(mw *mp4.Writer) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeDinf()}); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeDref()}); err != nil {
		return err
	}
	dref := &mp4.Dref{EntryCount: 1}
	if _, err := mp4.Marshal(mw, dref, mp4.Context{}); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeUrl()}); err != nil {
		return err
	}
	// Flag 1 = self-contained: the url box carries no location string.
	url := &mp4.Url{FullBox: mp4.FullBox{Flags: [3]byte{0, 0, 1}}}
	if _, err := mp4.Marshal(mw, url, mp4.Context{}); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil { // dref
		return err
	}
	_, err := mw.EndBox() // dinf
	return err
}

func (s Spec) writeStbl(mw *mp4.Writer) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStbl()}); err != nil {
		return err
	}
	if err := s.writeStsd(mw); err != nil {
		return err
	}
	if err := s.writeTimeAndSync(mw); err != nil {
		return err
	}
	if err := s.writeStsc(mw); err != nil {
		return err
	}
	if err := s.writeStsz(mw); err != nil {
		return err
	}
	// stco is rewritten after mdat layout is known; reserve the box with a
	// placeholder offset and patch it via PatchMP4STCO.
	if err := writeStcoPlaceholder(mw, s.SampleCount); err != nil {
		return err
	}
	_, err := mw.EndBox()
	return err
}

func (s Spec) writeStsd(mw *mp4.Writer) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsd()}); err != nil {
		return err
	}
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr[4:], 1) // one sample entry
	if _, err := mw.Write(hdr); err != nil {
		return err
	}
	codec := s.Codec
	if codec == "" {
		codec = "avc1"
	}
	// "unknown" is a spec-level marker; emit a real fourCC that the probe
	// does not classify as supported.
	if codec == "unknown" {
		codec = "mp4v"
	}
	if err := writeVisualEntry(mw, codec, s); err != nil {
		return err
	}
	_, err := mw.EndBox()
	return err
}

func writeVisualEntry(mw *mp4.Writer, codec string, s Spec) error {
	payload := make([]byte, 78) // standard VisualSampleEntry payload
	binary.BigEndian.PutUint16(payload[6:], 1) // data reference index
	binary.BigEndian.PutUint16(payload[24:], uint16(s.Width))
	binary.BigEndian.PutUint16(payload[26:], uint16(s.Height))
	binary.BigEndian.PutUint32(payload[28:], 0x00480000) // 72 dpi
	binary.BigEndian.PutUint32(payload[32:], 0x00480000)
	binary.BigEndian.PutUint16(payload[40:], 1) // frame count
	payload[74] = 0x18                          // depth
	binary.BigEndian.PutUint16(payload[76:], 0xffff)
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.StrToBoxType(codec)}); err != nil {
		return err
	}
	if _, err := mw.Write(payload); err != nil {
		return err
	}
	if s.TrackColour != nil {
		if err := writeTrackColr(mw, *s.TrackColour); err != nil {
			return err
		}
	}
	if s.IllegalCrop {
		if err := writeIllegalClap(mw, s); err != nil {
			return err
		}
	}
	_, err := mw.EndBox()
	return err
}

func writeTrackColr(mw *mp4.Writer, c [3]uint16) error {
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

// writeIllegalClap emits a clean aperture wider than the coded frame.
// The box is written fully raw (no StartBox/EndBox) because go-mp4 v1.3.0
// does not register a clap box definition; the probe reads it structurally.
func writeIllegalClap(mw *mp4.Writer, s Spec) error {
	payload := make([]byte, 36) // version/flags + 8 uint32 rationals
	binary.BigEndian.PutUint32(payload[4:], s.Width+200) // clean width num
	binary.BigEndian.PutUint32(payload[8:], 1)
	binary.BigEndian.PutUint32(payload[12:], s.Height) // clean height num
	binary.BigEndian.PutUint32(payload[16:], 1)
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr[0:], uint32(8+len(payload)))
	copy(hdr[4:], []byte("clap"))
	if _, err := mw.Write(hdr); err != nil {
		return err
	}
	_, err := mw.Write(payload)
	return err
}

func (s Spec) writeTimeAndSync(mw *mp4.Writer) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStts()}); err != nil {
		return err
	}
	payload := make([]byte, 16)
	binary.BigEndian.PutUint32(payload[4:], 1) // one run
	binary.BigEndian.PutUint32(payload[8:], uint32(s.SampleCount))
	binary.BigEndian.PutUint32(payload[12:], s.FrameDelta)
	if _, err := mw.Write(payload); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil {
		return err
	}
	if s.KeyEvery > 0 {
		if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStss()}); err != nil {
			return err
		}
		keys := 0
		for i := 0; i < s.SampleCount; i += s.KeyEvery {
			keys++
		}
		hdr := make([]byte, 8)
		binary.BigEndian.PutUint32(hdr[4:], uint32(keys))
		if _, err := mw.Write(hdr); err != nil {
			return err
		}
		buf := make([]byte, 4)
		for i := 0; i < s.SampleCount; i += s.KeyEvery {
			binary.BigEndian.PutUint32(buf, uint32(i+1)) // 1-based
			if _, err := mw.Write(buf); err != nil {
				return err
			}
		}
		if _, err := mw.EndBox(); err != nil {
			return err
		}
	}
	return nil
}

func (s Spec) writeStsc(mw *mp4.Writer) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsc()}); err != nil {
		return err
	}
	payload := make([]byte, 20)
	binary.BigEndian.PutUint32(payload[4:], 1) // one entry
	binary.BigEndian.PutUint32(payload[8:], 1) // first chunk
	binary.BigEndian.PutUint32(payload[12:], 1) // one sample per chunk
	if _, err := mw.Write(payload); err != nil {
		return err
	}
	_, err := mw.EndBox()
	return err
}

func (s Spec) writeStsz(mw *mp4.Writer) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsz()}); err != nil {
		return err
	}
	count := s.SampleCount
	if s.TruncateSTSZ {
		count = s.SampleCount / 2 // declared count kept, entries halved
	}
	hdr := make([]byte, 12)
	binary.BigEndian.PutUint32(hdr[4:], 0) // variable sample size
	binary.BigEndian.PutUint32(hdr[8:], uint32(s.SampleCount))
	if _, err := mw.Write(hdr); err != nil {
		return err
	}
	buf := make([]byte, 4)
	for i := 0; i < count; i++ {
		binary.BigEndian.PutUint32(buf, uint32(SampleSize(s, i)))
		if _, err := mw.Write(buf); err != nil {
			return err
		}
	}
	_, err := mw.EndBox()
	return err
}

func writeStcoPlaceholder(mw *mp4.Writer, count int) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStco()}); err != nil {
		return err
	}
	payload := make([]byte, 8+4*count)
	binary.BigEndian.PutUint32(payload[4:], uint32(count))
	_, err := mw.Write(payload)
	if err != nil {
		return err
	}
	_, err = mw.EndBox()
	return err
}
