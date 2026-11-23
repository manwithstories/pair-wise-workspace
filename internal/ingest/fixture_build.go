package ingest

import (
	"os"
	"path/filepath"

	mp4 "github.com/abema/go-mp4"
)

// GenerateMP4 writes a self-contained MP4 fixture described by spec.
// The layout is ftyp/moov(metadata-only)/mdat with synthetic sample
// bytes; no external media is referenced.
func GenerateMP4(spec FixtureSpec) (string, error) {
	if spec.VideoTimescale == 0 {
		spec.VideoTimescale = 30000
	}
	if spec.VideoSamples == 0 {
		spec.VideoSamples = 12
	}
	if err := ensureParent(spec.Path); err != nil {
		return "", err
	}

	mem := newMemFile(8192)
	w := mp4.NewWriter(mem)

	// ftyp
	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()})
	_, _ = mp4.Marshal(w, &mp4.Ftyp{MajorBrand: [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 512}, mp4.Context{})
	_, _ = w.EndBox()

	// moov
	moov, _ := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()})
	writeMovieHeader(w, spec)
	if spec.ContainerColor != nil {
		_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeColr()})
		c := spec.ContainerColor
		c.Source = "container"
		_, _ = mp4.Marshal(w, colrBox(c), mp4.Context{})
		_, _ = w.EndBox()
	}
	writeVideoTrak(w, spec)
	writeAudioTrak(w, spec)
	_, _ = w.EndBox() // moov
	_ = moov

	// mdat with deterministic synthetic payload
	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()})
	_, _ = mp4.Marshal(w, &mp4.Mdat{Data: syntheticPayload(spec)}, mp4.Context{})
	_, _ = w.EndBox()

	data := mem.Bytes()
	switch spec.Damage {
	case DamageTruncated:
		// Cut well inside the moov metadata so a child box header is
		// incomplete; the walker must report the structural damage.
		data = data[:len(data)/2]
	case DamageBadSampleTable:
		data = corruptSampleCount(data)
	}
	if err := os.WriteFile(spec.Path, data, 0o644); err != nil {
		return "", err
	}
	return spec.Path, nil
}

func ensureParent(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o755)
}

func writeMovieHeader(w *mp4.Writer, spec FixtureSpec) {
	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMvhd()})
	dur := spec.VideoSamples * spec.VideoTimescale / 30
	_, _ = mp4.Marshal(w, &mp4.Mvhd{
		Timescale:   spec.VideoTimescale,
		DurationV0:  dur,
		Rate:        0x10000,
		Volume:      0x100,
		Matrix:      matrixFor(spec.ContainerRotation),
		NextTrackID: 3,
	}, mp4.Context{})
	_, _ = w.EndBox()
}

func writeVideoTrak(w *mp4.Writer, spec FixtureSpec) {
	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()})

	// tkhd with track-level rotation
	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTkhd()})
	_, _ = mp4.Marshal(w, &mp4.Tkhd{
		TrackID:     1,
		DurationV0:  spec.VideoSamples * spec.VideoTimescale / 30,
		Matrix:      matrixFor(spec.TrackRotation),
		Width:       int32u16(spec.Width),
		Height:      int32u16(spec.Height),
	}, mp4.Context{})
	_, _ = w.EndBox()

	if spec.ContainerCrop != nil {
		// Container-level crop lives as a trak-level clap sibling.
		_, _ = w.StartBox(&mp4.BoxInfo{Type: boxType("clap")})
		_, _ = mp4.Marshal(w, newFixed("clap", 32, clapPayload(spec.ContainerCrop)), mp4.Context{})
		_, _ = w.EndBox()
	}

	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia()})
	writeMdhd(w, 1, spec.VideoTimescale, spec.VideoSamples*spec.VideoTimescale/30)
	writeHdlr(w, "vide", "VideoHandler")
	writeVideoMinf(w, spec)
	_, _ = w.EndBox() // mdia
	_, _ = w.EndBox() // trak
}

func writeAudioTrak(w *mp4.Writer, spec FixtureSpec) {
	rate := spec.AudioSampleRate
	if rate == 0 {
		return
	}
	samples := rate / 10 // 100ms-equivalent per sample at 10fps grouping
	n := spec.VideoSamples
	audioDur := uint32(n) * samples / 10

	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()})
	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTkhd()})
	_, _ = mp4.Marshal(w, &mp4.Tkhd{TrackID: 2, DurationV0: audioDur,
		Volume: 0x100}, mp4.Context{})
	_, _ = w.EndBox()

	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia()})
	writeMdhd(w, 2, rate, uint32(n)*rate/30)
	writeHdlr(w, "soun", "SoundHandler")
	writeAudioMinf(w, spec, n)
	_, _ = w.EndBox()
	_, _ = w.EndBox()
}

func writeMdhd(w *mp4.Writer, id, timescale, dur uint32) {
	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdhd()})
	_, _ = mp4.Marshal(w, &mp4.Mdhd{
		Timescale:  timescale,
		DurationV0: dur,
	}, mp4.Context{})
	_, _ = w.EndBox()
}

func writeHdlr(w *mp4.Writer, htype, name string) {
	_, _ = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeHdlr()})
	_, _ = mp4.Marshal(w, &mp4.Hdlr{
		HandlerType: mp4.StrToBoxType(htype),
		Name:        name + "\x00",
	}, mp4.Context{})
	_, _ = w.EndBox()
}

func int32u16(v uint32) uint32 { return v << 16 }

// syntheticPayload produces deterministic, content-addressable bytes.
func syntheticPayload(spec FixtureSpec) []byte {
	total := spec.VideoSamples * 64
	out := make([]byte, total)
	for i := range out {
		out[i] = byte((i*31 + int(spec.Width) + int(spec.Height)) & 0xff)
	}
	return out
}
