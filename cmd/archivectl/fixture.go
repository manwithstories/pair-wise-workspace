package main

import (
	"io"
	"os"

	"github.com/abema/go-mp4"

	"archive/internal/manifest"
	"archive/internal/media"
)

// Fixture generation is fully deterministic: every byte is derived from
// the seed via a small LCG, so identical flags reproduce identical media,
// manifests and index digests.

type lcg struct{ v uint64 }

func (l *lcg) next() uint64 {
	l.v = l.v*6364136223846793005 + 1442695040888963407
	return l.v >> 16
}

// genManifest builds the demo manifest: a video and an audio track with
// non-zero starts, different timescales, edit lists (dwell differs by
// 792µs, which becomes the repaired A/V sync error), a small backward
// timestamp and a gap for the repair stage.
func genManifest(seed int64) *manifest.Manifest {
	r := &lcg{v: uint64(seed)*2654435761 + 1}
	v := manifest.RawTrack{ID: "v1", Type: "video", Timescale: 90000}
	for i := 0; i < 300; i++ {
		dts := int64(9000 + i*3000)
		switch i {
		case 121:
			dts -= 500 // small backward jump: repair clamps it
		case 200:
			dts += 20000 // gap: repair records it
		}
		v.Samples = append(v.Samples, manifest.RawSample{
			DTS: dts, Duration: 3000, Keyframe: i%60 == 0,
			Size: uint32(800 + r.next()%400),
		})
	}
	v.EditList = []manifest.RawEdit{
		{MediaTime: -1, SegmentDuration: 100000}, // 100ms dwell
		{MediaTime: 9000, SegmentDuration: 10_000_000},
	}
	a := manifest.RawTrack{ID: "a1", Type: "audio", Timescale: 48000}
	for i := 0; i < 469; i++ {
		a.Samples = append(a.Samples, manifest.RawSample{
			DTS: int64(4800 + i*1024), Duration: 1024, Keyframe: i%47 == 0,
			Size: uint32(180 + r.next()%40),
		})
	}
	a.EditList = []manifest.RawEdit{
		{MediaTime: -1, SegmentDuration: 100792}, // 100.792ms dwell
		{MediaTime: 4800, SegmentDuration: 10_005_333},
	}
	return &manifest.Manifest{
		Asset: "interview-demo", MovieTimescale: 1_000_000,
		ProgramTracks: []string{"v1", "a1"}, Tracks: []manifest.RawTrack{v, a},
	}
}

// sampleByte derives one deterministic payload byte.
func sampleByte(seed int64, trackIdx, sampleIdx, byteIdx int) byte {
	x := uint64(seed)*2862933555777941757 + uint64(trackIdx)*3037000493 +
		uint64(sampleIdx)*3202034522624059733 + uint64(byteIdx)*9696967
	x ^= x >> 33
	return byte(x)
}

// writeMP4 writes the manifest's tracks as a minimal MP4 (ftyp, mdat,
// moov with mdhd/elst/stts/stss/stsz/stco) using go-mp4. Sample payloads
// are deterministic. It returns the sample-data byte offsets per track.
func writeMP4(path string, m *manifest.Manifest, seed int64) ([][]uint64, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, media.New(media.KindInternal, "fixture.mp4", "", -1, "create", err)
	}
	defer f.Close()
	w := mp4.NewWriter(f)
	ftyp := &mp4.Ftyp{MajorBrand: mp4.BrandISOM(), MinorVersion: 0}
	ftyp.AddCompatibleBrand(mp4.BrandISOM())
	if err := addBox(w, mp4.BoxTypeFtyp(), ftyp); err != nil {
		return nil, media.New(media.KindInternal, "fixture.mp4", "", -1, "write ftyp", err)
	}
	// Layout: ftyp, moov, mdat. moov comes first so that truncating the
	// file cuts media data while the sample tables stay intact — the
	// truncated-media fixture exercises exactly that case. moov is written
	// once with placeholder stco to learn its size, then rewritten in
	// place with the real offsets (identical size: one uint32 per stco).
	moovStart, err := w.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, media.New(media.KindInternal, "fixture.mp4", "", -1, "seek", err)
	}
	placeholder := make([][]uint64, len(m.Tracks))
	if err := writeMoov(w, m, placeholder); err != nil {
		return nil, err
	}
	mdatStart, err := w.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, media.New(media.KindInternal, "fixture.mp4", "", -1, "seek", err)
	}
	offsets := make([][]uint64, len(m.Tracks))
	pos := uint64(mdatStart) + 8 // 8-byte mdat header
	for ti, tr := range m.Tracks {
		for _, s := range tr.Samples {
			offsets[ti] = append(offsets[ti], pos)
			pos += uint64(s.Size)
		}
	}
	if _, err := w.Seek(moovStart, io.SeekStart); err != nil {
		return nil, media.New(media.KindInternal, "fixture.mp4", "", -1, "rewind moov", err)
	}
	if err := writeMoov(w, m, offsets); err != nil {
		return nil, err
	}
	if _, err := w.Seek(mdatStart, io.SeekStart); err != nil {
		return nil, media.New(media.KindInternal, "fixture.mp4", "", -1, "seek mdat", err)
	}
	// mdat: all samples, track by track.
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()}); err != nil {
		return nil, media.New(media.KindInternal, "fixture.mp4", "", -1, "start mdat", err)
	}
	for ti, tr := range m.Tracks {
		for si, s := range tr.Samples {
			buf := make([]byte, s.Size)
			for bi := range buf {
				buf[bi] = sampleByte(seed, ti, si, bi)
			}
			if _, err := w.Write(buf); err != nil {
				return nil, media.New(media.KindInternal, "fixture.mp4", tr.ID, int64(si), "write sample", err)
			}
		}
	}
	if _, err := w.EndBox(); err != nil {
		return nil, media.New(media.KindInternal, "fixture.mp4", "", -1, "end mdat", err)
	}
	return offsets, nil
}
