package mp4

import (
	"fmt"
	"os"
	"path/filepath"

	gomp4 "github.com/abema/go-mp4"

	"mediad/internal/media"
)

// WriteFile builds a deterministic MP4/MOV subset and atomically writes it.
func WriteFile(path string, p *media.Project) error {
	data, err := Build(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Build serializes normalized timetables and deterministic sample bytes.
func Build(p *media.Project) ([]byte, error) {
	if p == nil {
		return nil, media.NewFault(media.CodeInvalidInput, "nil project").Fatal()
	}
	if len(p.Tracks) == 0 {
		return nil, media.NewFault(media.CodeInvalidInput, "project has no tracks").Fatal()
	}
	if len(p.Tracks) > MaxTracks {
		return nil, media.NewFault(media.CodeTooManyTracks, "tracks=%d limit=%d", len(p.Tracks), MaxTracks).Fatal()
	}
	for _, t := range p.Tracks {
		if t == nil || t.ID == 0 || t.Timescale == 0 {
			return nil, media.NewFault(media.CodeInvalidInput, "track is missing id or timescale").Fatal()
		}
		if len(t.Samples) == 0 {
			return nil, media.NewFault(media.CodeInvalidInput, "track %d has no samples", t.ID).WithTrack(t.ID).Fatal()
		}
		if len(t.Samples) > int(^uint32(0)) {
			return nil, media.NewFault(media.CodeInvalidInput, "track %d has too many samples", t.ID).WithTrack(t.ID).Fatal()
		}
	}

	ftyp, err := buildFtyp(p.SourceFormat)
	if err != nil {
		return nil, err
	}
	moovPlaceholder, err := buildMoov(p)
	if err != nil {
		return nil, err
	}
	mdatStart := int64(len(ftyp) + len(moovPlaceholder) + 8)

	var mdatPayload []byte
	for _, t := range p.Tracks {
		for i := range t.Samples {
			s := &t.Samples[i]
			if s.Size == 0 {
				s.Size = 32
			}
			if int64(s.Size) > MaxFileSize || mdatStart+int64(len(mdatPayload))+int64(s.Size) > MaxFileSize {
				return nil, media.NewFault(media.CodeFileTooLarge, "sample exceeds 2 GiB safety limit").WithTrack(t.ID).Fatal()
			}
			s.Offset = mdatStart + int64(len(mdatPayload))
			payload := deterministicSamplePayload(t, s)
			mdatPayload = append(mdatPayload, payload...)
		}
	}
	if int64(len(ftyp)+len(moovPlaceholder)+8+len(mdatPayload)) > MaxFileSize {
		return nil, media.NewFault(media.CodeFileTooLarge, "output exceeds 2 GiB").Fatal()
	}

	moov, err := buildMoov(p)
	if err != nil {
		return nil, err
	}
	if len(moov) != len(moovPlaceholder) {
		return nil, fmt.Errorf("internal error: moov size changed after offset assignment")
	}
	mdat := boxBytes("mdat", mdatPayload)
	out := make([]byte, 0, len(ftyp)+len(moov)+len(mdat))
	out = append(out, ftyp...)
	out = append(out, moov...)
	out = append(out, mdat...)
	return out, nil
}

func deterministicSamplePayload(t *media.Track, s *media.Sample) []byte {
	b := make([]byte, s.Size)
	magic := []byte("MDIAMEDIA")
	if s.Sync {
		magic = []byte("MDIASYNC!")
	}
	copy(b, magic)
	for i := len(magic); i < len(b); i++ {
		b[i] = byte(t.ID*31 + uint32(s.Index)*7 + uint32(i))
	}
	return b
}

func buildFtyp(format string) ([]byte, error) {
	major := [4]byte{'i', 's', 'o', 'm'}
	brand := [4]byte{'i', 's', 'o', 'm'}
	if format == "mov" || format == "qt" {
		major = [4]byte{'q', 't', ' ', ' '}
		brand = major
	}
	f := &gomp4.Ftyp{MajorBrand: major, MinorVersion: 0x200}
	f.CompatibleBrands = []gomp4.CompatibleBrandElem{{CompatibleBrand: brand}, {CompatibleBrand: [4]byte{'m', 'p', '4', '2'}}}
	return marshalTypedBox(f)
}

func buildMoov(p *media.Project) ([]byte, error) {
	timescale := p.MovieTimescale
	if timescale == 0 {
		timescale = 1000
	}
	duration := p.DurationTicks
	if duration == 0 {
		for _, t := range p.Tracks {
			var d uint64
			for _, s := range t.Samples {
				d += uint64(s.Duration)
			}
			if d > duration {
				duration = d
			}
		}
	}
	mvhd := &gomp4.Mvhd{
		Timescale:   timescale,
		DurationV0:  uint32(duration),
		Rate:        0x10000,
		Volume:      0x0100,
		Matrix:      [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000},
		NextTrackID: uint32(len(p.Tracks) + 1),
	}
	mvhdBox, err := marshalTypedBox(mvhd)
	if err != nil {
		return nil, err
	}
	children := [][]byte{mvhdBox}
	for _, t := range p.Tracks {
		tb, err := buildTrak(t)
		if err != nil {
			return nil, err
		}
		children = append(children, tb)
	}
	return containerBytes("moov", children...), nil
}

func containerBytes(typ string, children ...[]byte) []byte {
	var payload []byte
	for _, c := range children {
		payload = append(payload, c...)
	}
	return boxBytes(typ, payload)
}

func buildTrak(t *media.Track) ([]byte, error) {
	var trackDuration uint64
	for _, s := range t.Samples {
		trackDuration += uint64(s.Duration)
	}
	flags := uint32(7)
	tkhd := &gomp4.Tkhd{
		FullBox:    gomp4.FullBox{Flags: [3]byte{byte(flags >> 16), byte(flags >> 8), byte(flags)}},
		TrackID:    t.ID,
		DurationV0: uint32(trackDuration),
		Layer:      0,
		Volume:     0,
		Matrix:     [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000},
		Width:      uint32(t.Width) << 16,
		Height:     uint32(t.Height) << 16,
	}
	if t.Kind == media.KindAudio {
		tkhd.Volume = 0x0100
	}
	tkhdBox, err := marshalTypedBox(tkhd)
	if err != nil {
		return nil, err
	}
	mdhdBox, err := marshalTypedBox(&gomp4.Mdhd{Timescale: t.Timescale, DurationV0: uint32(trackDuration)})
	if err != nil {
		return nil, err
	}
	var handlerType [4]byte
	copy(handlerType[:], string(t.Kind))
	hdlr, err := marshalTypedBox(&gomp4.Hdlr{HandlerType: handlerType, Name: "mediad"})
	if err != nil {
		return nil, err
	}
	stbl, err := buildStbl(t)
	if err != nil {
		return nil, err
	}
	minf := containerBytes("minf", stbl)
	mdia := containerBytes("mdia", mdhdBox, hdlr, minf)
	children := [][]byte{tkhdBox}
	if len(t.Edits) > 0 {
		elst, err := buildElst(t.Edits)
		if err != nil {
			return nil, err
		}
		children = append(children, containerBytes("edts", elst))
	}
	children = append(children, mdia)
	if t.Audio != nil {
		children = append(children, buildAudioAudit(t.Audio))
	}
	if t.Color != nil {
		children = append(children, buildColorAudit(t.Color))
	}
	return containerBytes("trak", children...), nil
}

func buildElst(edits []media.EditEntry) ([]byte, error) {
	anyV1 := false
	for _, e := range edits {
		if e.SegmentDuration > uint64(^uint32(0)) || e.MediaTime > mathMaxInt32 || e.MediaTime < mathMinInt32 {
			anyV1 = true
		}
	}
	box := &gomp4.Elst{EntryCount: uint32(len(edits))}
	if anyV1 {
		box.Version = 1
		box.Entries = make([]gomp4.ElstEntry, len(edits))
		for i, e := range edits {
			box.Entries[i] = gomp4.ElstEntry{SegmentDurationV1: e.SegmentDuration, MediaTimeV1: e.MediaTime, MediaRateInteger: e.MediaRate}
			if box.Entries[i].MediaRateInteger == 0 {
				box.Entries[i].MediaRateInteger = 1
			}
		}
	} else {
		box.Entries = make([]gomp4.ElstEntry, len(edits))
		for i, e := range edits {
			rate := e.MediaRate
			if rate == 0 {
				rate = 1
			}
			box.Entries[i] = gomp4.ElstEntry{SegmentDurationV0: uint32(e.SegmentDuration), MediaTimeV0: int32(e.MediaTime), MediaRateInteger: rate}
		}
	}
	return marshalTypedBox(box)
}

const (
	mathMaxInt32 = int64(^uint32(0) >> 1)
	mathMinInt32 = -mathMaxInt32 - 1
)
