package metadata

import (
	"bytes"
	"fmt"
	"io"

	"github.com/abema/go-mp4"
)

// MP4Document is the in-memory box tree produced by the generator and
// consumed by the writer. Keeping it as data lets tests mutate one box to
// build corrupt fixtures without string surgery on a byte slice.
type MP4Document struct {
	ContainerColor ColorDesc
	Tracks         []MP4Track
	// SampleBytes is the payload size of one sample in mdat. The mdat box
	// holds SampleBytes * SampleCount bytes per track, interleaved in track
	// order. It bounds fixture size and is not interpreted as real media.
	SampleBytes int
}

// MP4Track is the writer-side description of one trak.
type MP4Track struct {
	ID         int
	Track      WriteTrack
	Matrix     [9]int32
	SttsDelta  uint32
	SampleSize uint32
	KeySamples []uint32 // 1-based sample numbers, first must be 1
}

// BuildDocument validates the fixture description and fills defaults.
func BuildDocument(opts WriteOptions) (*MP4Document, error) {
	if len(opts.Tracks) == 0 {
		return nil, fmt.Errorf("metadata: no tracks")
	}
	doc := &MP4Document{ContainerColor: opts.ContainerColor, SampleBytes: 1024}
	for i, wt := range opts.Tracks {
		if wt.Timescale <= 0 {
			return nil, fmt.Errorf("metadata: track %d timescale must be positive", i+1)
		}
		if wt.SampleCount <= 0 || wt.SampleDelta <= 0 {
			return nil, fmt.Errorf("metadata: track %d needs positive sample count and delta", i+1)
		}
		matrix := wt.Matrix
		if matrix == ([9]int32{}) {
			matrix = MatrixForRotation(0)
		}
		keys := []uint32{1}
		doc.Tracks = append(doc.Tracks, MP4Track{
			ID:         i + 1,
			Track:      wt,
			Matrix:     matrix,
			SttsDelta:  uint32(wt.SampleDelta),
			SampleSize: uint32(doc.SampleBytes),
			KeySamples: keys,
		})
	}
	return doc, nil
}

// box writes one box: start, payload, end.
type boxWriter struct {
	w    *mp4.Writer
	root *bytes.Buffer
}

func (bw *boxWriter) start(t string) error {
	_, err := bw.w.StartBox(&mp4.BoxInfo{Type: mp4.StrToBoxType(t)})
	return err
}

func (bw *boxWriter) end() error {
	_, err := bw.w.EndBox()
	return err
}

func (bw *boxWriter) payload(box mp4.IImmutableBox) error {
	_, err := mp4.Marshal(bw.w, box, mp4.Context{})
	return err
}

func (bw *boxWriter) fullBox(t string, body mp4.IImmutableBox) error {
	if err := bw.start(t); err != nil {
		return err
	}
	if err := bw.payload(body); err != nil {
		return err
	}
	return bw.end()
}

// Write serializes the document. The result is deterministic: box order,
// matrix values, sample sizes and keyframes depend only on the document.
func (doc *MP4Document) Write() ([]byte, error) {
	wb := &memWriter{}
	w := mp4.NewWriter(wb)
	bw := &boxWriter{w: w}

	// ftyp
	if err := bw.start("ftyp"); err != nil {
		return nil, err
	}
	if err := bw.payload(&mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 512,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
			{CompatibleBrand: [4]byte{'i', 's', 'o', '2'}},
			{CompatibleBrand: [4]byte{'m', 'p', '4', '1'}},
		},
	}); err != nil {
		return nil, err
	}
	if err := bw.end(); err != nil {
		return nil, err
	}

	// mdat payload placeholder: one track at a time so that a session
	// interrupt leaves an incomplete, detectable file.
	if err := bw.start("mdat"); err != nil {
		return nil, err
	}
	for ti := range doc.Tracks {
		tr := &doc.Tracks[ti]
		chunk := bytes.Repeat([]byte{byte('A' + ti)}, doc.SampleBytes)
		for s := 0; s < tr.Track.SampleCount; s++ {
			if _, err := w.Write(chunk); err != nil {
				return nil, err
			}
		}
	}
	if err := bw.end(); err != nil {
		return nil, err
	}

	// moov
	if err := bw.start("moov"); err != nil {
		return nil, err
	}
	if err := bw.fullBox("mvhd", &mp4.Mvhd{
		Timescale:   1000,
		DurationV0:  doc.maxDurationMs(),
		Rate:        1 << 16,
		Volume:      0x0100,
		Matrix:      MatrixForRotation(0),
		NextTrackID: uint32(len(doc.Tracks) + 1),
	}); err != nil {
		return nil, err
	}
	if err := bw.fullBox("ccol", NewCCol(doc.ContainerColor)); err != nil {
		return nil, err
	}
	for ti := range doc.Tracks {
		if err := bw.writeTrak(&doc.Tracks[ti]); err != nil {
			return nil, err
		}
	}
	if err := bw.end(); err != nil {
		return nil, err
	}
	return wb.Bytes(), nil
}

func (doc *MP4Document) maxDurationMs() uint32 {
	var max int64
	for _, tr := range doc.Tracks {
		ms := tr.Track.Duration * 1000 / int64(tr.Track.Timescale)
		if ms > max {
			max = ms
		}
	}
	return uint32(max)
}

func (bw *boxWriter) writeTrak(tr *MP4Track) error {
	if err := bw.start("trak"); err != nil {
		return err
	}
	if err := bw.fullBox("tkhd", &mp4.Tkhd{
		FullBox:    mp4.FullBox{Flags: [3]byte{0, 0, 3}},
		TrackID:    uint32(tr.ID),
		DurationV0: uint32(tr.Track.Duration),
		Matrix:     tr.Matrix,
		Width:      uint32(tr.Track.Width) << 16,
		Height:     uint32(tr.Track.Height) << 16,
	}); err != nil {
		return err
	}
	if err := bw.start("mdia"); err != nil {
		return err
	}
	if err := bw.fullBox("mdhd", &mp4.Mdhd{
		Timescale:  uint32(tr.Track.Timescale),
		DurationV0: uint32(tr.Track.Duration),
		Language:   [3]byte{'u', 'n', 'd'},
	}); err != nil {
		return err
	}
	handler := "vide"
	if tr.Track.Kind == KindAudio {
		handler = "soun"
	}
	if err := bw.fullBox("hdlr", &mp4.Hdlr{
		HandlerType: [4]byte{handler[0], handler[1], handler[2], handler[3]},
		Name:        handler + "\x00",
	}); err != nil {
		return err
	}
	if err := bw.start("minf"); err != nil {
		return err
	}
	if tr.Track.Kind == KindAudio {
		if err := bw.fullBox("smhd", &mp4.Smhd{}); err != nil {
			return err
		}
	} else {
		if err := bw.fullBox("vmhd", &mp4.Vmhd{}); err != nil {
			return err
		}
	}
	if err := bw.start("stbl"); err != nil {
		return err
	}
	if err := bw.fullBox("stsd", &mp4.Stsd{EntryCount: 1}); err != nil {
		return err
	}
	if err := bw.writeSampleEntry(tr); err != nil {
		return err
	}
	if err := bw.fullBox("stts", &mp4.Stts{
		EntryCount: 1,
		Entries:    []mp4.SttsEntry{{SampleCount: uint32(tr.Track.SampleCount), SampleDelta: tr.SttsDelta}},
	}); err != nil {
		return err
	}
	// Offsets point past the 8-byte mdat header; the writer fixes the
	// self-consistent value later. Every sample occupies SampleSize bytes
	// starting at the first mdat payload byte.
	if err := bw.fullBox("stco", &mp4.Stco{
		EntryCount:  1,
		ChunkOffset: []uint32{0},
	}); err != nil {
		return err
	}
	if err := bw.fullBox("stsz", &mp4.Stsz{
		SampleSize:  tr.SampleSize,
		SampleCount: uint32(tr.Track.SampleCount),
	}); err != nil {
		return err
	}
	if err := bw.fullBox("stss", &mp4.Stss{
		EntryCount:   uint32(len(tr.KeySamples)),
		SampleNumber: tr.KeySamples,
	}); err != nil {
		return err
	}
	if err := bw.end(); err != nil { // stbl
		return err
	}
	if err := bw.end(); err != nil { // minf
		return err
	}
	if err := bw.end(); err != nil { // mdia
		return err
	}
	return bw.end() // trak
}

func (bw *boxWriter) writeSampleEntry(tr *MP4Track) error {
	if tr.Track.Kind == KindAudio {
		if err := bw.start("mp4a"); err != nil {
			return err
		}
		entry := &mp4.AudioSampleEntry{
			SampleEntry:  mp4.SampleEntry{AnyTypeBox: mp4.AnyTypeBox{Type: mp4.StrToBoxType("mp4a")}},
			ChannelCount: uint16(tr.Track.Channels),
			SampleSize:   16,
			SampleRate:   uint32(tr.Track.SampleRate) << 16,
		}
		if err := bw.payload(entry); err != nil {
			return err
		}
		return bw.end()
	}

	if err := bw.start("avc1"); err != nil {
		return err
	}
	entry := &mp4.VisualSampleEntry{
		SampleEntry: mp4.SampleEntry{AnyTypeBox: mp4.AnyTypeBox{Type: mp4.StrToBoxType("avc1")}},
		Width:       uint16(tr.Track.Width),
		Height:      uint16(tr.Track.Height),
		FrameCount:  1,
		Depth:       0x0018,
	}
	if err := bw.payload(entry); err != nil {
		return err
	}
	// colr nclx
	if !tr.Track.Color.Empty() {
		colr := &mp4.Colr{ColourType: [4]byte{'n', 'c', 'l', 'x'}}
		if tr.Track.Color.Primaries != nil {
			colr.ColourPrimaries = *tr.Track.Color.Primaries
		}
		if tr.Track.Color.Transfer != nil {
			colr.TransferCharacteristics = *tr.Track.Color.Transfer
		}
		if tr.Track.Color.Matrix != nil {
			colr.MatrixCoefficients = *tr.Track.Color.Matrix
		}
		if tr.Track.Color.FullRange != nil {
			colr.FullRangeFlag = *tr.Track.Color.FullRange
		}
		if err := bw.start("colr"); err != nil {
			return err
		}
		if err := bw.payload(colr); err != nil {
			return err
		}
		if err := bw.end(); err != nil {
			return err
		}
	}
	if tr.Track.Crop != nil {
		if err := bw.start("clap"); err != nil {
			return err
		}
		if err := bw.payload(NewClap(*tr.Track.Crop)); err != nil {
			return err
		}
		if err := bw.end(); err != nil {
			return err
		}
	}
	return bw.end()
}

// memWriter is an append-only in-memory io.WriteSeeker. The MP4 writer seeks
// back only to patch a box header with the same number of bytes it reserved,
// so overwriting from the seek target is sufficient.
type memWriter struct {
	buf []byte
	pos int
}

func (m *memWriter) Write(p []byte) (int, error) {
	if m.pos > len(m.buf) {
		return 0, fmt.Errorf("metadata: write beyond end at %d (len %d)", m.pos, len(m.buf))
	}
	need := m.pos + len(p)
	if need <= len(m.buf) {
		copy(m.buf[m.pos:], p)
	} else {
		m.buf = append(m.buf[:m.pos], p...)
	}
	m.pos = need
	return len(p), nil
}

func (m *memWriter) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = int64(m.pos) + offset
	case io.SeekEnd:
		target = int64(len(m.buf)) + offset
	default:
		return 0, fmt.Errorf("metadata: bad whence %d", whence)
	}
	if target < 0 || target > int64(len(m.buf)) {
		return 0, fmt.Errorf("metadata: seek to %d outside 0..%d", target, len(m.buf))
	}
	m.pos = int(target)
	return target, nil
}

func (m *memWriter) Bytes() []byte { return m.buf }
