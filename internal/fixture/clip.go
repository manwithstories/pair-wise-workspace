package fixture

import (
	"os"

	mp4 "github.com/abema/go-mp4"

	"avpipe/internal/fault"
)

// ClipConfig describes a tiny diagnostic MP4 fragment.
type ClipConfig struct {
	Path      string
	Frames    int
	FPS       int
	Timescale uint32
	Width     uint16
	Height    uint16
}

// VerifyClip probes a written clip and returns the probed track count and
// duration. Used by selftest to prove the MP4 path works end to end.
func VerifyClip(path string) (tracks int, duration uint64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, fault.Wrap(fault.CodeInternal, "fixture.clip", err, "open %s", path)
	}
	defer f.Close()
	info, err := mp4.Probe(f)
	if err != nil {
		return 0, 0, fault.Wrap(fault.CodeInternal, "fixture.clip", err, "probe %s", path)
	}
	return len(info.Tracks), info.Duration, nil
}

// WriteClip writes a small, locally generated MP4 with one AVC-labelled video
// track. The samples are synthetic bytes: the file is a structural diagnostic
// artifact, not decodable video, which keeps fixtures tiny and generated at
// run time instead of checked in.
func WriteClip(cfg ClipConfig) error {
	if cfg.Timescale == 0 {
		cfg.Timescale = 90000
	}
	if cfg.FPS <= 0 {
		cfg.FPS = 30
	}
	if cfg.Frames <= 0 {
		cfg.Frames = cfg.FPS
	}
	if cfg.Width == 0 {
		cfg.Width = 320
	}
	if cfg.Height == 0 {
		cfg.Height = 180
	}
	if cfg.Path == "" {
		return fault.New(fault.CodeInvalidArgument, "fixture.clip", "empty output path")
	}
	f, err := os.Create(cfg.Path)
	if err != nil {
		return fault.Wrap(fault.CodeInternal, "fixture.clip", err, "create %s", cfg.Path)
	}
	defer f.Close()
	w := mp4.NewWriter(f)

	delta := uint32(cfg.Timescale) / uint32(cfg.FPS)
	if delta == 0 {
		delta = 1
	}
	duration := delta * uint32(cfg.Frames)
	sample := make([]byte, 16)
	for i := range sample {
		sample[i] = byte(0x40 + i)
	}

	if err := writeBox(w, &mp4.Ftyp{
		MajorBrand:   mp4.BrandISOM(),
		MinorVersion: 0x200,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: mp4.BrandISOM()},
			{CompatibleBrand: mp4.BrandISO2()},
		},
	}); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return clipErr(err)
	}
	if err := writeBox(w, &mp4.Mvhd{
		Timescale: cfg.Timescale, DurationV0: duration, Rate: 0x00010000,
		Volume: 0x0100, Matrix: identityMatrix(), NextTrackID: 2,
	}); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()}); err != nil {
		return clipErr(err)
	}
	if err := writeBox(w, &mp4.Tkhd{
		TrackID: 1, DurationV0: duration, Matrix: identityMatrix(),
		Width: uint32(cfg.Width) << 16, Height: uint32(cfg.Height) << 16,
	}); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia()}); err != nil {
		return clipErr(err)
	}
	if err := writeBox(w, &mp4.Mdhd{
		Timescale: cfg.Timescale, DurationV0: duration,
		Language: [3]byte{'u', 'n', 'd'},
	}); err != nil {
		return err
	}
	if err := writeBox(w, &mp4.Hdlr{
		HandlerType: [4]byte{'v', 'i', 'd', 'e'}, Name: "avpipe video",
	}); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMinf()}); err != nil {
		return clipErr(err)
	}
	if err := writeBox(w, &mp4.Vmhd{}); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeDinf()}); err != nil {
		return clipErr(err)
	}
	if err := writeDref(w); err != nil {
		return err
	}
	if _, err := w.EndBox(); err != nil { // dinf
		return clipErr(err)
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStbl()}); err != nil {
		return clipErr(err)
	}
	if err := writeStsd(w, cfg); err != nil {
		return err
	}
	if err := writeBox(w, &mp4.Stts{EntryCount: 1, Entries: []mp4.SttsEntry{
		{SampleCount: uint32(cfg.Frames), SampleDelta: delta},
	}}); err != nil {
		return err
	}
	if err := writeBox(w, &mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{
		{FirstChunk: 1, SamplesPerChunk: uint32(cfg.Frames), SampleDescriptionIndex: 1},
	}}); err != nil {
		return err
	}
	sizes := make([]uint32, cfg.Frames)
	for i := range sizes {
		sizes[i] = uint32(len(sample))
	}
	if err := writeBox(w, &mp4.Stsz{SampleCount: uint32(cfg.Frames), EntrySize: sizes}); err != nil {
		return err
	}
	// Placeholder chunk offset; patched once mdat has a real position.
	stcoPos, err := f.Seek(0, os.SEEK_CUR)
	if err != nil {
		return clipErr(err)
	}
	if err := writeBox(w, &mp4.Stco{EntryCount: 1, ChunkOffset: []uint32{0}}); err != nil {
		return err
	}
	w.EndBox() // stbl
	w.EndBox() // minf
	w.EndBox() // mdia
	w.EndBox() // trak
	w.EndBox() // moov

	mdatStart, err := f.Seek(0, os.SEEK_CUR)
	if err != nil {
		return clipErr(err)
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()}); err != nil {
		return clipErr(err)
	}
	for i := 0; i < cfg.Frames; i++ {
		if _, err := w.Write(sample); err != nil {
			return clipErr(err)
		}
	}
	if _, err := w.EndBox(); err != nil {
		return clipErr(err)
	}
	// stco payload: version/flags (4) + entry_count (4), then the first
	// offset (4). Patch the entry in place now that mdat is placed.
	chunk := uint32(mdatStart) + 8
	if _, err := f.Seek(stcoPos+8+8, os.SEEK_SET); err != nil {
		return clipErr(err)
	}
	if _, err := f.Write([]byte{byte(chunk >> 24), byte(chunk >> 16), byte(chunk >> 8), byte(chunk)}); err != nil {
		return clipErr(err)
	}
	return nil
}

func writeStsd(w *mp4.Writer, cfg ClipConfig) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsd()}); err != nil {
		return clipErr(err)
	}
	if _, err := mp4.Marshal(w, &mp4.Stsd{EntryCount: 1}, mp4.Context{}); err != nil {
		return fault.Wrap(fault.CodeInternal, "fixture.clip", err, "marshal stsd")
	}
	entry := &mp4.VisualSampleEntry{
		SampleEntry: mp4.SampleEntry{
			AnyTypeBox:         mp4.AnyTypeBox{Type: mp4.BoxTypeAvc1()},
			DataReferenceIndex: 1,
		},
		Width: cfg.Width, Height: cfg.Height,
		Horizresolution: 0x00480000, Vertresolution: 0x00480000,
		FrameCount: 1, Depth: 0x0018,
	}
	copy(entry.Compressorname[1:], []byte("avpipe"))
	entry.Compressorname[0] = 6
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeAvc1()}); err != nil {
		return clipErr(err)
	}
	if _, err := mp4.Marshal(w, entry, mp4.Context{}); err != nil {
		return fault.Wrap(fault.CodeInternal, "fixture.clip", err, "marshal avc1")
	}
	if _, err := w.EndBox(); err != nil {
		return clipErr(err)
	}
	if _, err := w.EndBox(); err != nil { // stsd
		return clipErr(err)
	}
	return nil
}

func writeDref(w *mp4.Writer) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeDref()}); err != nil {
		return clipErr(err)
	}
	if _, err := mp4.Marshal(w, &mp4.Dref{EntryCount: 1}, mp4.Context{}); err != nil {
		return fault.Wrap(fault.CodeInternal, "fixture.clip", err, "marshal dref")
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeUrl()}); err != nil {
		return clipErr(err)
	}
	// Self-contained data reference: flags = 1, no location string.
	if _, err := mp4.Marshal(w, &mp4.Url{FullBox: mp4.FullBox{Flags: [3]byte{0, 0, 1}}}, mp4.Context{}); err != nil {
		return fault.Wrap(fault.CodeInternal, "fixture.clip", err, "marshal url")
	}
	if _, err := w.EndBox(); err != nil { // url
		return clipErr(err)
	}
	if _, err := w.EndBox(); err != nil { // dref
		return clipErr(err)
	}
	return nil
}

func writeBox(w *mp4.Writer, box mp4.IImmutableBox) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: box.GetType()}); err != nil {
		return clipErr(err)
	}
	if _, err := mp4.Marshal(w, box, mp4.Context{}); err != nil {
		return fault.Wrap(fault.CodeInternal, "fixture.clip", err, "marshal %s", box.GetType())
	}
	if _, err := w.EndBox(); err != nil {
		return clipErr(err)
	}
	return nil
}

func identityMatrix() [9]int32 {
	return [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
}

func clipErr(err error) error {
	if _, ok := err.(*fault.Error); ok {
		return err
	}
	return fault.Wrap(fault.CodeInternal, "fixture.clip", err, "write clip")
}
