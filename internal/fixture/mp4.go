package fixture

import (
	"bytes"
	"fmt"
	"io"

	mp4 "github.com/abema/go-mp4"
)

// seekableBuffer adapts bytes.Buffer to io.WriteSeeker for go-mp4's
// box-size back-patching (StartBox/EndBox seek to rewrite headers).
type seekableBuffer struct {
	buf bytes.Buffer
	pos int64
}

func (s *seekableBuffer) Write(p []byte) (int, error) {
	if int64(len(p))+s.pos > int64(s.buf.Len()) {
		grow := int64(len(p)) + s.pos - int64(s.buf.Len())
		s.buf.Write(make([]byte, grow))
	}
	n := copy(s.buf.Bytes()[s.pos:], p)
	s.pos += int64(n)
	return n, nil
}

func (s *seekableBuffer) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = s.pos + offset
	case io.SeekEnd:
		target = int64(s.buf.Len()) + offset
	default:
		return 0, fmt.Errorf("fixture: unknown seek whence %d", whence)
	}
	if target < 0 || target > int64(s.buf.Len()) {
		return 0, fmt.Errorf("fixture: seek %d out of range [0,%d]", target, s.buf.Len())
	}
	s.pos = target
	return target, nil
}

func (s *seekableBuffer) bytes() []byte { return s.buf.Bytes() }

// marshalBox opens a box, marshals its payload and closes the box so the
// size header is back-patched correctly.
func marshalBox(w *mp4.Writer, boxType mp4.BoxType, box mp4.IImmutableBox) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: boxType}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, box, mp4.Context{}); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}

// PreviewFrame is one entry of the deterministic off-screen frame sequence:
// its 1-based sample number, timestamp in the MP4 timescale and whether it
// is a sync (key) sample.
type PreviewFrame struct {
	Sample     uint32
	TimeScaled uint32
	Keyframe   bool
}

// PreviewSpec parameterises the generated preview container.
type PreviewSpec struct {
	Timescale uint32 // media timescale, e.g. 30000 for a 30 fps track
	Delta     uint32 // constant sample delta in timescale units
	Frames    []PreviewFrame
}

// BuildPreviewMP4 writes a minimal standards-shaped MP4 (ftyp/moov/mdat).
// The sample tables (stts/stss/stsz/stsc/stco) carry the exact timestamps
// and keyframe flags the review pipeline re-reads with go-mp4 to verify the
// time index; mdat holds deterministic placeholder bytes, not pictures.
func BuildPreviewMP4(spec PreviewSpec) ([]byte, error) {
	if spec.Timescale == 0 || spec.Delta == 0 || len(spec.Frames) == 0 {
		return nil, fmt.Errorf("fixture: invalid preview spec %+v", spec)
	}
	n := uint32(len(spec.Frames))
	var keySamples, sizes []uint32
	for _, f := range spec.Frames {
		if f.Keyframe {
			keySamples = append(keySamples, f.Sample)
		}
		sizes = append(sizes, 8+f.Sample) // deterministic per-sample size
	}
	duration := spec.Delta * n

	sb := &seekableBuffer{}
	w := mp4.NewWriter(sb)
	ftyp := &mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 0x200,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
			{CompatibleBrand: [4]byte{'i', 's', 'o', '2'}},
		},
	}
	if err := marshalBox(w, mp4.BoxTypeFtyp(), ftyp); err != nil {
		return nil, err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return nil, err
	}
	mvhd := &mp4.Mvhd{
		Timescale: spec.Timescale, DurationV0: duration,
		Rate: 0x00010000, Volume: 0x0100,
		Matrix:      [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000},
		NextTrackID: 2,
	}
	if err := marshalBox(w, mp4.BoxTypeMvhd(), mvhd); err != nil {
		return nil, err
	}
	if err := marshalTrak(w, spec, n, duration, sizes, keySamples); err != nil {
		return nil, err
	}
	if _, err := w.EndBox(); err != nil { // moov
		return nil, err
	}
	mdatPayload := make([]byte, 0, int(n)*8)
	for _, f := range spec.Frames {
		tag := []byte{byte(f.Sample), 0, 0, 0, 0, 0, 0, 0}
		mdatPayload = append(mdatPayload, tag...)
	}
	if err := marshalBox(w, mp4.BoxTypeMdat(), &mp4.Mdat{Data: mdatPayload}); err != nil {
		return nil, err
	}
	out := make([]byte, len(sb.bytes()))
	copy(out, sb.bytes())
	return out, nil
}

func marshalTrak(w *mp4.Writer, spec PreviewSpec, n, duration uint32,
	sizes, keySamples []uint32) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()}); err != nil {
		return err
	}
	tkhd := &mp4.Tkhd{
		TrackID: 1, DurationV0: duration, Volume: 0,
		Width:  1 << 16,
		Height: 1 << 16,
		Matrix: [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000},
	}
	if err := marshalBox(w, mp4.BoxTypeTkhd(), tkhd); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia()}); err != nil {
		return err
	}
	mdhd := &mp4.Mdhd{Timescale: spec.Timescale, DurationV0: duration}
	if err := marshalBox(w, mp4.BoxTypeMdhd(), mdhd); err != nil {
		return err
	}
	hdlr := &mp4.Hdlr{HandlerType: [4]byte{'v', 'i', 'd', 'e'}, Name: "review-preview"}
	if err := marshalBox(w, mp4.BoxTypeHdlr(), hdlr); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMinf()}); err != nil {
		return err
	}
	if err := marshalBox(w, mp4.BoxTypeVmhd(), &mp4.Vmhd{}); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeDinf()}); err != nil {
		return err
	}
	if err := marshalBox(w, mp4.BoxTypeDref(), &mp4.Dref{EntryCount: 0}); err != nil {
		return err
	}
	if _, err := w.EndBox(); err != nil { // dinf
		return err
	}
	if err := marshalStbl(w, spec, n, sizes, keySamples); err != nil {
		return err
	}
	if _, err := w.EndBox(); err != nil { // minf
		return err
	}
	if _, err := w.EndBox(); err != nil { // mdia
		return err
	}
	if _, err := w.EndBox(); err != nil { // trak
		return err
	}
	return nil
}

func marshalStbl(w *mp4.Writer, spec PreviewSpec, n uint32,
	sizes, keySamples []uint32) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStbl()}); err != nil {
		return err
	}
	if err := marshalBox(w, mp4.BoxTypeStsd(), &mp4.Stsd{EntryCount: 0}); err != nil {
		return err
	}
	stts := &mp4.Stts{EntryCount: 1,
		Entries: []mp4.SttsEntry{{SampleCount: n, SampleDelta: spec.Delta}}}
	if err := marshalBox(w, mp4.BoxTypeStts(), stts); err != nil {
		return err
	}
	stss := &mp4.Stss{EntryCount: uint32(len(keySamples)), SampleNumber: keySamples}
	if err := marshalBox(w, mp4.BoxTypeStss(), stss); err != nil {
		return err
	}
	stsc := &mp4.Stsc{EntryCount: 1,
		Entries: []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: n, SampleDescriptionIndex: 1}}}
	if err := marshalBox(w, mp4.BoxTypeStsc(), stsc); err != nil {
		return err
	}
	if err := marshalBox(w, mp4.BoxTypeStsz(),
		&mp4.Stsz{SampleCount: n, EntrySize: sizes}); err != nil {
		return err
	}
	// One chunk; offset unused by index verification and stays zero.
	if err := marshalBox(w, mp4.BoxTypeStco(),
		&mp4.Stco{EntryCount: 1, ChunkOffset: []uint32{0}}); err != nil {
		return err
	}
	if _, err := w.EndBox(); err != nil { // stbl
		return err
	}
	return nil
}
