package mp4

import (
	"io"

	"github.com/abema/go-mp4"
)

// WriteClassic writes a non-fragmented (moov sample-table) MP4 with one
// chunk per sample. Payload bytes are copied verbatim. It is used for
// fixtures and for producing files that classic demuxers accept.
func WriteClassic(w io.WriteSeeker, tracks []MuxTrack, samples [][]Sample) error {
	// The chunk-offset values are fixed-width, so the moov size is known
	// before the offsets are: write it once into scratch with zero
	// offsets, then use that size to place mdat.
	scratch := &MemWS{}
	moovSize, err := classicMoovSize(tracks, samples)
	if err != nil {
		return err
	}
	_ = scratch
	ftypSize := int64(classicFtypSize())
	payloadStart := ftypSize + moovSize + 8 // + mdat header
	offsets := make([][]uint64, len(tracks))
	next := uint64(payloadStart)
	for ti := range tracks {
		for _, s := range samples[ti] {
			offsets[ti] = append(offsets[ti], next)
			next += uint64(len(s.Data))
		}
	}
	if err := writeClassicHeader(w, tracks, samples, offsets); err != nil {
		return err
	}
	ws := mp4.NewWriter(w)
	if _, err := ws.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()}); err != nil {
		return err
	}
	for ti := range tracks {
		for _, s := range samples[ti] {
			if _, err := ws.Write(s.Data); err != nil {
				return err
			}
		}
	}
	_, err = ws.EndBox()
	return err
}

// classicMoovSize measures the moov box for the given samples by writing
// it with placeholder offsets.
func classicMoovSize(tracks []MuxTrack, samples [][]Sample) (int64, error) {
	offsets := make([][]uint64, len(tracks))
	for ti := range tracks {
		offsets[ti] = make([]uint64, len(samples[ti]))
	}
	m := &MemWS{}
	if err := writeClassicMoov(m, tracks, samples, offsets); err != nil {
		return 0, err
	}
	return int64(len(m.buf)), nil
}

func classicFtypSize() int {
	m := &MemWS{}
	ws := mp4.NewWriter(m)
	ws.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()})
	mp4.Marshal(ws, &mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', '6'},
		MinorVersion: 1,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', '6'}},
			{CompatibleBrand: [4]byte{'m', 'p', '4', '1'}},
		},
	}, mp4.Context{})
	ws.EndBox()
	return len(m.buf)
}

// writeClassicHeader writes ftyp and moov. offsets[ti][i] is the absolute
// file offset of sample i.
func writeClassicHeader(w io.WriteSeeker, tracks []MuxTrack, samples [][]Sample, offsets [][]uint64) error {
	ws := mp4.NewWriter(w)
	if _, err := ws.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(ws, &mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', '6'},
		MinorVersion: 1,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', '6'}},
			{CompatibleBrand: [4]byte{'m', 'p', '4', '1'}},
		},
	}, mp4.Context{}); err != nil {
		return err
	}
	if _, err := ws.EndBox(); err != nil {
		return err
	}
	return writeClassicMoov(w, tracks, samples, offsets)
}

func writeClassicMoov(w io.WriteSeeker, tracks []MuxTrack, samples [][]Sample, offsets [][]uint64) error {
	ws := mp4.NewWriter(w)
	if _, err := ws.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return err
	}
	if _, err := ws.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMvhd()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(ws, &mp4.Mvhd{
		Timescale: 1000, Rate: 0x10000, Volume: 0x100,
		Matrix: stdMatrix, NextTrackID: uint32(len(tracks)) + 1,
	}, mp4.Context{}); err != nil {
		return err
	}
	if _, err := ws.EndBox(); err != nil {
		return err
	}
	for ti, t := range tracks {
		if err := writeClassicTrak(ws, t, samples[ti], offsets[ti]); err != nil {
			return err
		}
	}
	_, err := ws.EndBox()
	return err
}

func writeClassicTrak(ws *mp4.Writer, t MuxTrack, samples []Sample, offsets []uint64) error {
	if _, err := ws.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()}); err != nil {
		return err
	}
	tk := &mp4.Tkhd{TrackID: t.ID, Matrix: stdMatrix,
		Width: uint32(t.Width) << 16, Height: uint32(t.Height) << 16}
	tk.Flags[0], tk.Flags[1], tk.Flags[2] = 0, 0, 0x07
	if t.Type == "audio" {
		tk.Volume = 0x100
	}
	if err := marshalBox(ws, mp4.BoxTypeTkhd(), tk); err != nil {
		return err
	}
	if _, err := ws.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia()}); err != nil {
		return err
	}
	md := &mp4.Mdhd{Timescale: t.Timescale}
	copy(md.Language[:], langBytes(t.Language))
	if err := marshalBox(ws, mp4.BoxTypeMdhd(), md); err != nil {
		return err
	}
	h := &mp4.Hdlr{Name: t.Type + " handler"}
	switch t.Type {
	case "video":
		h.HandlerType = [4]byte{'v', 'i', 'd', 'e'}
	case "audio":
		h.HandlerType = [4]byte{'s', 'o', 'u', 'n'}
	default:
		h.HandlerType = [4]byte{'s', 'u', 'b', 't'}
	}
	if err := marshalBox(ws, mp4.BoxTypeHdlr(), h); err != nil {
		return err
	}
	if _, err := ws.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMinf()}); err != nil {
		return err
	}
	var err error
	switch t.Type {
	case "video":
		vm := &mp4.Vmhd{}
		vm.Flags[2] = 1
		err = marshalBox(ws, mp4.BoxTypeVmhd(), vm)
	case "audio":
		err = marshalBox(ws, mp4.BoxTypeSmhd(), &mp4.Smhd{})
	default:
		err = marshalBox(ws, mp4.StrToBoxType("nmhd"), &Nmhd{})
	}
	if err != nil {
		return err
	}
	if _, err := ws.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStbl()}); err != nil {
		return err
	}
	if _, err := ws.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsd()}); err != nil {
		return err
	}
	if err := writeStsd(ws, t); err != nil {
		return err
	}
	if _, err := ws.EndBox(); err != nil { // stsd
		return err
	}
	stts := &mp4.Stts{}
	for _, s := range samples {
		n := len(stts.Entries)
		if n > 0 && stts.Entries[n-1].SampleDelta == s.Duration {
			stts.Entries[n-1].SampleCount++
		} else {
			stts.Entries = append(stts.Entries, mp4.SttsEntry{SampleCount: 1, SampleDelta: s.Duration})
		}
	}
	stts.EntryCount = uint32(len(stts.Entries))
	if err := marshalBox(ws, mp4.BoxTypeStts(), stts); err != nil {
		return err
	}
	// ctts: only when a sample has a non-zero composition offset
	var ctos []mp4.CttsEntry
	needCtts := false
	for _, s := range samples {
		if s.CTSOffset != 0 {
			needCtts = true
		}
		n := len(ctos)
		if n > 0 && ctos[n-1].SampleOffsetV1 == s.CTSOffset {
			ctos[n-1].SampleCount++
		} else {
			ctos = append(ctos, mp4.CttsEntry{SampleCount: 1, SampleOffsetV1: s.CTSOffset})
		}
	}
	if needCtts {
		ctts := &mp4.Ctts{EntryCount: uint32(len(ctos)), Entries: ctos}
		ctts.Version = 1 // signed composition offsets
		if err := marshalBox(ws, mp4.BoxTypeCtts(), ctts); err != nil {
			return err
		}
	}
	// stss: only when some samples are not sync samples
	var syncNums []uint32
	for i, s := range samples {
		if s.Keyframe {
			syncNums = append(syncNums, uint32(i+1))
		}
	}
	if len(syncNums) != len(samples) && len(syncNums) > 0 {
		stss := &mp4.Stss{EntryCount: uint32(len(syncNums)), SampleNumber: syncNums}
		if err := marshalBox(ws, mp4.BoxTypeStss(), stss); err != nil {
			return err
		}
	}
	stsc := &mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{
		{FirstChunk: 1, SamplesPerChunk: 1, SampleDescriptionIndex: 1},
	}}
	if err := marshalBox(ws, mp4.BoxTypeStsc(), stsc); err != nil {
		return err
	}
	stsz := &mp4.Stsz{SampleCount: uint32(len(samples))}
	for _, s := range samples {
		stsz.EntrySize = append(stsz.EntrySize, uint32(len(s.Data)))
	}
	if err := marshalBox(ws, mp4.BoxTypeStsz(), stsz); err != nil {
		return err
	}
	stco := &mp4.Stco{EntryCount: uint32(len(offsets))}
	for _, off := range offsets {
		stco.ChunkOffset = append(stco.ChunkOffset, uint32(off))
	}
	if err := marshalBox(ws, mp4.BoxTypeStco(), stco); err != nil {
		return err
	}
	if _, err := ws.EndBox(); err != nil { // stbl
		return err
	}
	if _, err := ws.EndBox(); err != nil { // minf
		return err
	}
	if _, err := ws.EndBox(); err != nil { // mdia
		return err
	}
	_, err = ws.EndBox() // trak
	return err
}
