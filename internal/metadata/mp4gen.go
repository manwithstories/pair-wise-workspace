package metadata

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/abema/go-mp4"
)

// MP4TrackSpec describes one track written into a generated MP4 fixture.
type MP4TrackSpec struct {
	Kind      Kind
	ID        uint32
	Timescale uint32
	Duration  uint32
	Width     uint16
	Height    uint16
	// RotationDegrees is written into the tkhd matrix after normalization to
	// the nearest right angle (non-right angles are kept for the parser test).
	RotationDegrees float64
	// Colr, when non-nil, is written as an nclx colr box.
	Colr *mp4.Colr
	// Clap, when non-nil, is written as a clap box.
	Clap *Clap
	// EditStart is written as an elst entry when > 0.
	EditStart int64
	// Audio.
	SampleRate uint32
	Channels   uint16
	// CorruptClap writes an out-of-frame aperture to exercise crop errors.
	CorruptClap bool
	// CorruptColr writes a colr with all-zero code points.
	CorruptColr bool
}

// MP4Spec is a generated MP4 fixture.
type MP4Spec struct {
	Tracks []MP4TrackSpec
	// TruncateTo, when > 0, truncates the finished file to that many bytes to
	// simulate container corruption.
	TruncateTo int
}

// rotationMatrix returns the tkhd matrix for a clockwise rotation.
func rotationMatrix(degrees float64) [9]int32 {
	m := [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}
	switch {
	case degrees >= 45 && degrees < 135:
		return [9]int32{0, 0x10000, 0, -0x10000, 0, 0, 0, 0, 0x40000000}
	case degrees >= 135 && degrees < 225:
		return [9]int32{-0x10000, 0, 0, 0, -0x10000, 0, 0, 0, 0x40000000}
	case degrees >= 225 && degrees < 315:
		return [9]int32{0, -0x10000, 0, 0x10000, 0, 0, 0, 0, 0x40000000}
	}
	return m
}

// WriteMP4 writes the fixture to path, creating parent directories.
func WriteMP4(path string, spec MP4Spec) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := mp4.NewWriter(f)
	ctx := mp4.Context{}

	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		f.Close()
		return err
	}
	ftyp := &mp4.Ftyp{
		MajorBrand:       mp4.BrandISOM(),
		MinorVersion:     0x200,
		CompatibleBrands: []mp4.CompatibleBrandElem{{CompatibleBrand: mp4.BrandISOM()}, {CompatibleBrand: mp4.BrandMP41()}},
	}
	if _, err := mp4.Marshal(w, ftyp, ctx); err != nil {
		f.Close()
		return err
	}
	if _, err := w.EndBox(); err != nil {
		f.Close()
		return err
	}

	maxDur := uint32(0)
	for _, t := range spec.Tracks {
		if t.Duration > maxDur {
			maxDur = t.Duration
		}
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		f.Close()
		return err
	}
	mvhd := &mp4.Mvhd{Timescale: 1000, DurationV0: maxDur, Rate: 0x10000, Volume: 0x100, NextTrackID: uint32(len(spec.Tracks) + 1)}
	if err := writeBox(w, mp4.BoxTypeMvhd(), mvhd, ctx); err != nil {
		f.Close()
		return err
	}

	for _, t := range spec.Tracks {
		if err := writeTrak(w, t, ctx); err != nil {
			f.Close()
			return err
		}
	}
	if _, err := w.EndBox(); err != nil {
		f.Close()
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()}); err != nil {
		f.Close()
		return err
	}
	if _, err := w.EndBox(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if spec.TruncateTo > 0 {
		return truncate(path, spec.TruncateTo)
	}
	return nil
}

func truncate(path string, n int) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if int(info.Size()) <= n {
		return fmt.Errorf("truncate target %d >= size %d", n, info.Size())
	}
	return os.Truncate(path, int64(n))
}

// writeBox marshals a box body with go-mp4 and renews its header on EndBox.
func writeBox(w *mp4.Writer, bt mp4.BoxType, body mp4.IImmutableBox, ctx mp4.Context) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: bt}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, body, ctx); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}

func writeTrak(w *mp4.Writer, t MP4TrackSpec, ctx mp4.Context) error {
	start := func(bt mp4.BoxType) error {
		_, err := w.StartBox(&mp4.BoxInfo{Type: bt})
		return err
	}
	if err := start(mp4.BoxTypeTrak()); err != nil {
		return err
	}
	width, height := t.Width, t.Height
	tkhd := &mp4.Tkhd{
		TrackID:    t.ID,
		DurationV0: t.Duration,
		Matrix:     rotationMatrix(t.RotationDegrees),
		Width:      uint32(width) << 16,
		Height:     uint32(height) << 16,
	}
	if t.Kind == KindAudio {
		tkhd.Volume = 0x100
	}
	if err := writeBox(w, mp4.BoxTypeTkhd(), tkhd, ctx); err != nil {
		return err
	}
	if t.EditStart > 0 {
		if err := start(mp4.BoxTypeEdts()); err != nil {
			return err
		}
		elst := &mp4.Elst{EntryCount: 1, Entries: []mp4.ElstEntry{{SegmentDurationV0: t.Duration, MediaTimeV0: int32(t.EditStart)}}}
		if err := writeBox(w, mp4.BoxTypeElst(), elst, ctx); err != nil {
			return err
		}
		if _, err := w.EndBox(); err != nil {
			return err
		}
	}
	if err := start(mp4.BoxTypeMdia()); err != nil {
		return err
	}
	mdhd := &mp4.Mdhd{Timescale: t.Timescale, DurationV0: t.Duration, Language: [3]byte{21, 14, 4}}
	if err := writeBox(w, mp4.BoxTypeMdhd(), mdhd, ctx); err != nil {
		return err
	}
	handler := "vide"
	if t.Kind == KindAudio {
		handler = "soun"
	}
	hdlr := &mp4.Hdlr{HandlerType: [4]byte{handler[0], handler[1], handler[2], handler[3]}, Name: "mediad"}
	if err := writeBox(w, mp4.BoxTypeHdlr(), hdlr, ctx); err != nil {
		return err
	}
	if err := start(mp4.BoxTypeMinf()); err != nil {
		return err
	}
	if t.Kind == KindAudio {
		if err := writeBox(w, mp4.BoxTypeSmhd(), &mp4.Smhd{}, ctx); err != nil {
			return err
		}
	} else {
		if err := writeBox(w, mp4.BoxTypeVmhd(), &mp4.Vmhd{}, ctx); err != nil {
			return err
		}
	}
	if err := writeDinf(w, ctx); err != nil {
		return err
	}
	if err := start(mp4.BoxTypeStbl()); err != nil {
		return err
	}
	if err := writeStbl(w, t, ctx); err != nil {
		return err
	}
	for i := 0; i < 4; i++ { // stbl, minf, mdia, trak
		if _, err := w.EndBox(); err != nil {
			return err
		}
	}
	return nil
}

func writeDinf(w *mp4.Writer, ctx mp4.Context) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeDinf()}); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeDref()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, &mp4.Dref{EntryCount: 1}, ctx); err != nil {
		return err
	}
	url := &mp4.Url{FullBox: mp4.FullBox{Flags: [3]byte{0, 0, 1}}}
	if err := writeBox(w, mp4.BoxTypeUrl(), url, ctx); err != nil {
		return err
	}
	if _, err := w.EndBox(); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}

func writeStbl(w *mp4.Writer, t MP4TrackSpec, ctx mp4.Context) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsd()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, &mp4.Stsd{EntryCount: 1}, ctx); err != nil {
		return err
	}
	entryType := mp4.BoxTypeAvc1()
	if t.Kind == KindAudio {
		entryType = mp4.BoxTypeMp4a()
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: entryType}); err != nil {
		return err
	}
	if t.Kind == KindAudio {
		ase := &mp4.AudioSampleEntry{ChannelCount: t.Channels, SampleSize: 16, SampleRate: t.SampleRate << 16}
		ase.SetType(entryType)
		if _, err := mp4.Marshal(w, ase, ctx); err != nil {
			return err
		}
	} else {
		vse := &mp4.VisualSampleEntry{Width: t.Width, Height: t.Height, FrameCount: 1, Depth: 24}
		vse.SetType(entryType)
		if _, err := mp4.Marshal(w, vse, ctx); err != nil {
			return err
		}
		if err := writeVisualChildren(w, t, ctx); err != nil {
			return err
		}
	}
	if _, err := w.EndBox(); err != nil { // sample entry
		return err
	}
	if _, err := w.EndBox(); err != nil { // stsd
		return err
	}

	duration := t.Duration
	if duration == 0 {
		duration = 1
	}
	stts := &mp4.Stts{EntryCount: 1, Entries: []mp4.SttsEntry{{SampleCount: 1, SampleDelta: duration}}}
	if err := writeBox(w, mp4.BoxTypeStts(), stts, ctx); err != nil {
		return err
	}
	if err := writeBox(w, mp4.BoxTypeStsc(), &mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: 1, SampleDescriptionIndex: 1}}}, ctx); err != nil {
		return err
	}
	if err := writeBox(w, mp4.BoxTypeStsz(), &mp4.Stsz{SampleSize: 1, SampleCount: 1}, ctx); err != nil {
		return err
	}
	_, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStco()})
	if err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, &mp4.Stco{EntryCount: 1, ChunkOffset: []uint32{0}}, ctx); err != nil {
		return err
	}
	_, err = w.EndBox()
	return err
}

func writeVisualChildren(w *mp4.Writer, t MP4TrackSpec, ctx mp4.Context) error {
	if t.CorruptColr {
		bad := &mp4.Colr{ColourType: [4]byte{'n', 'c', 'l', 'x'}}
		if err := writeBox(w, mp4.BoxTypeColr(), bad, ctx); err != nil {
			return err
		}
	} else if t.Colr != nil {
		if err := writeBox(w, mp4.BoxTypeColr(), t.Colr, ctx); err != nil {
			return err
		}
	}
	if t.CorruptClap {
		// Rational but out of frame, so ValidateCrop (not the rational check)
		// is what rejects it.
		bad := &Clap{CleanApertureWidthN: int32(t.Width), CleanApertureWidthD: 1,
			CleanApertureHeightN: int32(t.Height), CleanApertureHeightD: 1,
			HorizOffN: 400, HorizOffD: 1, VertOffN: 0, VertOffD: 1}
		if err := writeBox(w, boxType("clap"), bad, ctx); err != nil {
			return err
		}
	} else if t.Clap != nil {
		if err := writeBox(w, boxType("clap"), t.Clap, ctx); err != nil {
			return err
		}
	}
	return nil
}

// demoMP4Spec is the standard two-video/one-audio fixture set used by the CLI
// and metadata tests: one 1080p track rotated 90 with BT.2020, one 720p track
// with a clean aperture and BT.709, and a 48 kHz stereo audio track.
func demoMP4Spec(corrupt bool) MP4Spec {
	spec := MP4Spec{Tracks: []MP4TrackSpec{
		{
			Kind: KindVideo, ID: 1, Timescale: 30000, Duration: 60000,
			Width: 1920, Height: 1080, RotationDegrees: 90,
			Colr: &mp4.Colr{ColourType: [4]byte{'n', 'c', 'l', 'x'},
				ColourPrimaries: PrimariesBT2020, TransferCharacteristics: TransferPQ,
				MatrixCoefficients: MatrixBT2020, FullRangeFlag: true},
			EditStart: 900,
		},
		{
			Kind: KindVideo, ID: 2, Timescale: 30000, Duration: 60000,
			Width: 1280, Height: 720,
			Colr: &mp4.Colr{ColourType: [4]byte{'n', 'c', 'l', 'x'},
				ColourPrimaries: PrimariesBT709, TransferCharacteristics: TransferBT709,
				MatrixCoefficients: MatrixBT709},
			Clap: &Clap{CleanApertureWidthN: 1024, CleanApertureWidthD: 1,
				CleanApertureHeightN: 576, CleanApertureHeightD: 1,
				HorizOffN: 0, HorizOffD: 1, VertOffN: 0, VertOffD: 1},
		},
		{
			Kind: KindAudio, ID: 3, Timescale: 48000, Duration: 96000,
			SampleRate: 48000, Channels: 2,
		},
	}}
	if corrupt {
		// Track v0 has an out-of-frame clean aperture; track v1 has an invalid
		// color description. Both are isolated so the audio track survives.
		spec.Tracks[0].CorruptClap = true
		spec.Tracks[1].CorruptColr = true
	}
	return spec
}

// WriteDemoMP4 writes the standard fixture to dir/sample.mp4.
func WriteDemoMP4(dir string, corrupt bool) (string, error) {
	path := filepath.Join(dir, "sample.mp4")
	if err := WriteMP4(path, demoMP4Spec(corrupt)); err != nil {
		return "", err
	}
	return path, nil
}
