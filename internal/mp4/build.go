package mp4

import (
	"bytes"
	"fmt"
	"io"

	"github.com/abema/go-mp4"
)

// Track describes one media track in a generated file.
type Track struct {
	ID        uint32
	Handler   string // "vide" or "soun"
	Timescale uint32
	// Deltas are sample decode deltas in track timescale units.
	// A single-entry compressed stts table is written when all deltas
	// agree; otherwise every delta is written explicitly (VFR).
	Deltas []uint32
	// Keys marks sync samples. A nil slice means every sample is a
	// sync sample and no stss box is written.
	Keys []bool
	// SampleSize is the byte size of each sample payload.
	SampleSize uint32
	// Payload overrides sample bytes. Missing bytes are filled with a
	// deterministic pattern; extra bytes are truncated.
	Payload []byte
}

// File is a generated MP4 with its layout recorded.
type File struct {
	Bytes  []byte
	Tracks []TrackLayout
}

// TrackLayout is the observable structure of one written track.
type TrackLayout struct {
	ID          uint32   `json:"id"`
	Handler     string   `json:"handler"`
	Timescale   uint32   `json:"timescale"`
	ChunkOffset []uint32 `json:"chunk_offsets"`
	SampleSize  uint32   `json:"sample_size"`
	SampleCount int      `json:"sample_count"`
	Sync        []bool   `json:"sync"`
	Deltas      []uint32 `json:"deltas"`
}

// Builder accumulates tracks and writes a moov/mdat file.
type Builder struct {
	tracks []Track
	// OmitSyncID suppresses the stss box even for keyed tracks,
	// producing the "missing keyframe table" fixture.
	OmitSyncID bool
	// OmitSizes writes an stsz with SampleSize = 0 but no EntrySize
	// values, producing the truncated size-table fixture.
	OmitSizes bool
	// ChunkOffsetBias is added to every chunk offset after layout.
	// A large positive value produces the out-of-bounds fixture.
	ChunkOffsetBias int64
}

// Add appends a track.
func (b *Builder) Add(t Track) { b.tracks = append(b.tracks, t) }

// Build writes a complete MP4 file. Each track contributes exactly one
// chunk whose offset and size are recorded in the returned layout.
func (b *Builder) Build() (*File, error) {
	if len(b.tracks) == 0 {
		return nil, fmt.Errorf("mp4: no tracks")
	}
	var prefix bytes.Buffer
	ftyp := &mp4.Ftyp{MajorBrand: [4]byte{'i', 's', 'o', 'm'}, MinorVersion: 512}
	ftyp.AddCompatibleBrand([4]byte{'i', 's', 'o', 'm'})
	ftyp.AddCompatibleBrand([4]byte{'i', 's', 'o', '2'})
	if err := writeBox(&prefix, ftyp); err != nil {
		return nil, err
	}
	ftypLen := prefix.Len()

	layouts := make([]TrackLayout, len(b.tracks))
	var mdat bytes.Buffer
	for ti, t := range b.tracks {
		if len(t.Deltas) == 0 {
			return nil, fmt.Errorf("mp4: track %d has no samples", t.ID)
		}
		if t.SampleSize == 0 {
			return nil, fmt.Errorf("mp4: track %d has zero sample size", t.ID)
		}
		count := len(t.Deltas)
		layout := TrackLayout{
			ID:          t.ID,
			Handler:     t.Handler,
			Timescale:   t.Timescale,
			SampleSize:  t.SampleSize,
			SampleCount: count,
			Deltas:      append([]uint32(nil), t.Deltas...),
		}
		// Chunk offset is relative to the start of the file; the moov
		// size is stable because stco entries are fixed width, so we
		// can write a provisional value and patch it once.
		layout.ChunkOffset = []uint32{0}
		for i := 0; i < count; i++ {
			sync := true
			if t.Keys != nil && i < len(t.Keys) {
				sync = t.Keys[i]
			}
			layout.Sync = append(layout.Sync, sync)
			chunk := make([]byte, t.SampleSize)
			for j := range chunk {
				pos := int64(i)*int64(t.SampleSize) + int64(j)
				if pos < int64(len(t.Payload)) {
					chunk[j] = t.Payload[pos]
				} else {
					chunk[j] = byte((int(t.ID) + i + j) & 0xFF)
				}
			}
			mdat.Write(chunk)
		}
		layouts[ti] = layout
	}

	// The moov size does not depend on chunk offsets, so build it once
	// to learn the mdat position, then rebuild with real offsets.
	moov, err := b.buildMoov(layouts)
	if err != nil {
		return nil, err
	}
	mdatAt := ftypLen + len(moov) + 8
	rel := uint64(0)
	for i := range layouts {
		layout := &layouts[i]
		layout.ChunkOffset[0] = uint32(int64(mdatAt) + int64(rel) + b.ChunkOffsetBias)
		if b.ChunkOffsetBias > 0 && layout.ChunkOffset[0] == 0 {
			return nil, fmt.Errorf("mp4: chunk offset overflow with bias %d", b.ChunkOffsetBias)
		}
		rel += uint64(layout.SampleSize) * uint64(layout.SampleCount)
	}
	moov, err = b.buildMoov(layouts)
	if err != nil {
		return nil, err
	}
	if ftypLen+len(moov)+8 != mdatAt {
		return nil, fmt.Errorf("mp4: moov size changed between passes")
	}

	out := make([]byte, 0, mdatAt+mdat.Len())
	out = append(out, prefix.Bytes()...)
	out = append(out, moov...)
	size := uint32(mdat.Len() + 8)
	out = append(out, byte(size>>24), byte(size>>16), byte(size>>8), byte(size), 'm', 'd', 'a', 't')
	out = append(out, mdat.Bytes()...)
	return &File{Bytes: out, Tracks: layouts}, nil
}

func (b *Builder) buildMoov(layouts []TrackLayout) ([]byte, error) {
	var body bytes.Buffer
	mvhd := &mp4.Mvhd{Timescale: 1000, NextTrackID: uint32(len(layouts) + 1)}
	mvhd.Rate = 1 << 16
	mvhd.Volume = 1 << 8
	mvhd.Matrix = [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
	if err := writeBox(&body, mvhd); err != nil {
		return nil, err
	}
	for ti := range layouts {
		trak, err := b.buildTrak(&b.tracks[ti], layouts[ti])
		if err != nil {
			return nil, err
		}
		if err := writeRaw(&body, "trak", trak); err != nil {
			return nil, err
		}
	}
	return frame("moov", body.Bytes()), nil
}

func (b *Builder) buildTrak(t *Track, l TrackLayout) ([]byte, error) {
	var trak bytes.Buffer
	tkhd := &mp4.Tkhd{TrackID: l.ID}
	tkhd.SetVersion(0)
	tkhd.Matrix = [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
	if err := writeBox(&trak, tkhd); err != nil {
		return nil, err
	}
	var mdia bytes.Buffer
	mdhd := &mp4.Mdhd{Timescale: l.Timescale, Language: [3]byte{'u', 'n', 'd'}}
	mdhd.SetVersion(0)
	if err := writeBox(&mdia, mdhd); err != nil {
		return nil, err
	}
	var handler [4]byte
	copy(handler[:], l.Handler)
	hdlr := &mp4.Hdlr{HandlerType: handler, Name: "Pairflow"}
	if err := writeBox(&mdia, hdlr); err != nil {
		return nil, err
	}
	var minf bytes.Buffer
	if l.Handler == "soun" {
		if err := writeBox(&minf, &mp4.Smhd{}); err != nil {
			return nil, err
		}
	} else {
		if err := writeBox(&minf, &mp4.Vmhd{}); err != nil {
			return nil, err
		}
	}
	stbl, err := b.buildStbl(t, l)
	if err != nil {
		return nil, err
	}
	if err := writeRaw(&minf, "stbl", stbl); err != nil {
		return nil, err
	}
	if err := writeRaw(&mdia, "minf", minf.Bytes()); err != nil {
		return nil, err
	}
	if err := writeRaw(&trak, "mdia", mdia.Bytes()); err != nil {
		return nil, err
	}
	return trak.Bytes(), nil
}

func (b *Builder) buildStbl(t *Track, l TrackLayout) ([]byte, error) {
	var stbl bytes.Buffer
	if err := writeBox(&stbl, &mp4.Stsd{EntryCount: 0}); err != nil {
		return nil, err
	}
	stts := &mp4.Stts{}
	if uniformDeltas(l.Deltas) {
		stts.Entries = []mp4.SttsEntry{{SampleCount: uint32(len(l.Deltas)), SampleDelta: l.Deltas[0]}}
	} else {
		for _, d := range l.Deltas {
			stts.Entries = append(stts.Entries, mp4.SttsEntry{SampleCount: 1, SampleDelta: d})
		}
	}
	stts.EntryCount = uint32(len(stts.Entries))
	if err := writeBox(&stbl, stts); err != nil {
		return nil, err
	}
	if err := writeBox(&stbl, &mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{{
		FirstChunk: 1, SamplesPerChunk: uint32(l.SampleCount), SampleDescriptionIndex: 1,
	}}}); err != nil {
		return nil, err
	}
	if err := b.writeStsz(&stbl, l); err != nil {
		return nil, err
	}
	if err := writeBox(&stbl, &mp4.Stco{EntryCount: 1, ChunkOffset: l.ChunkOffset}); err != nil {
		return nil, err
	}
	if t.Keys != nil && !b.OmitSyncID {
		stss := &mp4.Stss{}
		for i, k := range l.Sync {
			if k {
				stss.SampleNumber = append(stss.SampleNumber, uint32(i+1))
			}
		}
		stss.EntryCount = uint32(len(stss.SampleNumber))
		if err := writeBox(&stbl, stss); err != nil {
			return nil, err
		}
	}
	return stbl.Bytes(), nil
}

// writeStsz emits the sample size table. In truncated mode the entry
// count keeps the declared sample count but only half the required
// entry bytes are present, which is what a recording cut mid-write
// looks like. The parser must report the shortfall rather than invent
// sizes.
func (b *Builder) writeStsz(w *bytes.Buffer, l TrackLayout) error {
	if !b.OmitSizes {
		return writeBox(w, &mp4.Stsz{SampleSize: l.SampleSize, SampleCount: uint32(l.SampleCount)})
	}
	// Hand-build the payload: go-mp4 cannot marshal a size table whose
	// dynamic entry count does not match its sample count.
	var body bytes.Buffer
	putU32 := func(v uint32) {
		body.Write([]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
	}
	putU32(0) // version + flags
	putU32(0) // sample_size = 0 means entries follow
	putU32(uint32(l.SampleCount))
	for i := 0; i < l.SampleCount/2; i++ {
		putU32(l.SampleSize)
	}
	_, err := w.Write(frame("stsz", body.Bytes()))
	return err
}

func uniformDeltas(d []uint32) bool {
	for i := 1; i < len(d); i++ {
		if d[i] != d[0] {
			return false
		}
	}
	return true
}

// frame wraps a payload in an 8-byte box header.
func frame(typ string, payload []byte) []byte {
	size := uint32(len(payload) + 8)
	out := make([]byte, 8, 8+len(payload))
	out[0] = byte(size >> 24)
	out[1] = byte(size >> 16)
	out[2] = byte(size >> 8)
	out[3] = byte(size)
	copy(out[4:], typ)
	return append(out, payload...)
}

func writeRaw(w io.Writer, typ string, payload []byte) error {
	_, err := w.Write(frame(typ, payload))
	return err
}

func writeBox(w io.Writer, box mp4.IImmutableBox) error {
	var body bytes.Buffer
	if _, err := mp4.Marshal(&body, box, mp4.Context{}); err != nil {
		return err
	}
	t := box.GetType()
	return writeRaw(w, t.String(), body.Bytes())
}
