package fixture

import (
	"errors"
	"io"

	mp4 "github.com/abema/go-mp4"

	"relaycheck/internal/demux"
)

var identityMatrix = [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}

// writeLeaf marshals one complete box (header + payload).
func writeLeaf(w *mp4.Writer, box mp4.IImmutableBox) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: box.GetType()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, box, mp4.Context{}); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}

// WriteInit writes the two-track CMAF init segment (AVC video + AAC
// audio) used by every generated stream.
func WriteInit(w io.WriteSeeker) error {
	mw := mp4.NewWriter(w)
	ftyp := &mp4.Ftyp{MajorBrand: [4]byte{'c', 'm', 'f', 'c'}}
	ftyp.CompatibleBrands = append(ftyp.CompatibleBrands,
		mp4.CompatibleBrandElem{CompatibleBrand: [4]byte{'c', 'm', 'f', 'c'}},
		mp4.CompatibleBrandElem{CompatibleBrand: [4]byte{'i', 's', 'o', '8'}})
	if err := writeLeaf(mw, ftyp); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return err
	}
	mvhd := &mp4.Mvhd{Timescale: 1000, Rate: 0x10000, Volume: 0x100, Matrix: identityMatrix, NextTrackID: 3}
	if err := writeLeaf(mw, mvhd); err != nil {
		return err
	}
	if err := writeVideoTrak(mw); err != nil {
		return err
	}
	if err := writeAudioTrak(mw); err != nil {
		return err
	}
	_, err := mw.EndBox() // moov
	return err
}

func writeTrakShell(mw *mp4.Writer, id uint32, volume int16, width, height uint32, fn func() error) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()}); err != nil {
		return err
	}
	tkhd := &mp4.Tkhd{TrackID: id, Volume: volume, Matrix: identityMatrix, Width: width, Height: height}
	tkhd.SetFlags(0x3) // enabled | in movie
	if err := writeLeaf(mw, tkhd); err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	_, err := mw.EndBox() // trak
	return err
}

func writeMdia(mw *mp4.Writer, timescale uint32, lang, handler string, mediaHeader mp4.IImmutableBox, entry mp4.IImmutableBox) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia()}); err != nil {
		return err
	}
	mdhd := &mp4.Mdhd{Timescale: timescale, Language: demux.EncodeLanguage(lang)}
	if err := writeLeaf(mw, mdhd); err != nil {
		return err
	}
	hdlr := &mp4.Hdlr{Name: "relaycheck"}
	copy(hdlr.HandlerType[:], handler)
	if err := writeLeaf(mw, hdlr); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMinf()}); err != nil {
		return err
	}
	if err := writeLeaf(mw, mediaHeader); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStbl()}); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsd()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(mw, &mp4.Stsd{EntryCount: 1}, mp4.Context{}); err != nil {
		return err
	}
	if err := writeLeaf(mw, entry); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil { // stsd
		return err
	}
	for _, box := range []mp4.IImmutableBox{&mp4.Stts{}, &mp4.Stsc{}, &mp4.Stsz{}, &mp4.Stco{}} {
		if err := writeLeaf(mw, box); err != nil {
			return err
		}
	}
	if _, err := mw.EndBox(); err != nil { // stbl
		return err
	}
	if _, err := mw.EndBox(); err != nil { // minf
		return err
	}
	_, err := mw.EndBox() // mdia
	return err
}

func writeVideoTrak(mw *mp4.Writer) error {
	return writeTrakShell(mw, VideoTrackID, 0, 640<<16, 360<<16, func() error {
		entry := &mp4.VisualSampleEntry{Width: 640, Height: 360, Horizresolution: 0x480000, Vertresolution: 0x480000, FrameCount: 1, Depth: 0x18, PreDefined3: -1}
		entry.SetType(mp4.StrToBoxType("avc1"))
		entry.DataReferenceIndex = 1
		return writeMdia(mw, VideoTimescale, "und", "vide", &mp4.Vmhd{Graphicsmode: 0x40}, entry)
	})
}

func writeAudioTrak(mw *mp4.Writer) error {
	return writeTrakShell(mw, AudioTrackID, 0x100, 0, 0, func() error {
		entry := &mp4.AudioSampleEntry{ChannelCount: 2, SampleSize: 16, SampleRate: AudioTimescale << 16}
		entry.SetType(mp4.StrToBoxType("mp4a"))
		entry.DataReferenceIndex = 1
		return writeMdia(mw, AudioTimescale, "eng", "soun", &mp4.Smhd{}, entry)
	})
}

// BuildInit returns the init segment as bytes.
func BuildInit() ([]byte, error) {
	m := &seekBuf{}
	if err := WriteInit(m); err != nil {
		return nil, err
	}
	return m.buf, nil
}

// seekBuf is a minimal in-memory io.WriteSeeker.
type seekBuf struct {
	buf []byte
	pos int64
}

func (m *seekBuf) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if grow := int(end) - len(m.buf); grow > 0 {
		m.buf = append(m.buf, make([]byte, grow)...)
	}
	copy(m.buf[m.pos:end], p)
	m.pos = end
	return len(p), nil
}

func (m *seekBuf) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = m.pos + offset
	case io.SeekEnd:
		abs = int64(len(m.buf)) + offset
	}
	if abs < 0 {
		return 0, errors.New("negative seek")
	}
	m.pos = abs
	return abs, nil
}
