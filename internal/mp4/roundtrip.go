// Package mp4 muxes audio access units into MP4, parses them back, and
// compares the result field by field, locating the first difference. The
// random-access index is anchored on audio access units (every AU is a
// sync sample), so keyframe lookups are exact.
package mp4

import (
	"bytes"
	"fmt"
	"io"

	gomp4 "github.com/abema/go-mp4"
)

// Sample is one audio access unit in the mux.
type Sample struct {
	Data     []byte
	Duration uint32 // track timescale ticks
}

// memFile is an in-memory io.WriteSeeker.
type memFile struct {
	buf []byte
	pos int64
}

func (m *memFile) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if end > int64(len(m.buf)) {
		grown := make([]byte, end)
		copy(grown, m.buf)
		m.buf = grown
	}
	copy(m.buf[m.pos:], p)
	m.pos = end
	return len(p), nil
}

func (m *memFile) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.pos = off
	case io.SeekCurrent:
		m.pos += off
	case io.SeekEnd:
		m.pos = int64(len(m.buf)) + off
	}
	return m.pos, nil
}

func leaf(w *gomp4.Writer, box gomp4.IImmutableBox) error {
	if _, err := w.StartBox(&gomp4.BoxInfo{Type: box.GetType()}); err != nil {
		return err
	}
	if _, err := gomp4.Marshal(w, box, gomp4.Context{}); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}

func open(w *gomp4.Writer, t gomp4.BoxType) error {
	_, err := w.StartBox(&gomp4.BoxInfo{Type: t})
	return err
}

func closeBox(w *gomp4.Writer) error {
	_, err := w.EndBox()
	return err
}

var matrix = [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}

// writeMoov serializes ftyp+moov; chunkOffset is the stco value to record.
func writeMoov(w *gomp4.Writer, samples []Sample, timescale uint32, chunkOffset uint32) error {
	var total uint32
	stts := gomp4.Stts{FullBox: gomp4.FullBox{}}
	stsz := gomp4.Stsz{SampleCount: uint32(len(samples))}
	stss := gomp4.Stss{}
	for i, s := range samples {
		total += s.Duration
		stsz.EntrySize = append(stsz.EntrySize, uint32(len(s.Data)))
		stss.SampleNumber = append(stss.SampleNumber, uint32(i+1)) // every AU is random-access
		if n := len(stts.Entries); n > 0 && stts.Entries[n-1].SampleDelta == s.Duration {
			stts.Entries[n-1].SampleCount++
		} else {
			stts.Entries = append(stts.Entries, gomp4.SttsEntry{SampleCount: 1, SampleDelta: s.Duration})
		}
	}
	stts.EntryCount = uint32(len(stts.Entries))
	stss.EntryCount = uint32(len(stss.SampleNumber))

	if err := leaf(w, &gomp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 0,
		CompatibleBrands: []gomp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
		},
	}); err != nil {
		return err
	}
	if err := open(w, gomp4.BoxTypeMoov()); err != nil {
		return err
	}
	mvhd := &gomp4.Mvhd{Timescale: timescale, DurationV0: total,
		Rate: 0x10000, Volume: 0x100, Matrix: matrix, NextTrackID: 2}
	if err := leaf(w, mvhd); err != nil {
		return err
	}
	if err := open(w, gomp4.BoxTypeTrak()); err != nil {
		return err
	}
	tkhd := &gomp4.Tkhd{TrackID: 1, DurationV0: total, Volume: 0x100, Matrix: matrix}
	tkhd.SetFlags(0x3) // enabled | in movie
	if err := leaf(w, tkhd); err != nil {
		return err
	}
	if err := open(w, gomp4.BoxTypeMdia()); err != nil {
		return err
	}
	if err := leaf(w, &gomp4.Mdhd{Timescale: timescale, DurationV0: total, Language: [3]byte{'u', 'n', 'd'}}); err != nil {
		return err
	}
	if err := leaf(w, &gomp4.Hdlr{HandlerType: [4]byte{'s', 'o', 'u', 'n'}, Name: "mediad"}); err != nil {
		return err
	}
	if err := open(w, gomp4.BoxTypeMinf()); err != nil {
		return err
	}
	if err := leaf(w, &gomp4.Smhd{}); err != nil {
		return err
	}
	if err := open(w, gomp4.BoxTypeDinf()); err != nil {
		return err
	}
	if err := open(w, gomp4.BoxTypeDref()); err != nil {
		return err
	}
	if _, err := gomp4.Marshal(w, &gomp4.Dref{EntryCount: 1}, gomp4.Context{}); err != nil {
		return err
	}
	url := &gomp4.Url{}
	url.SetFlags(0x1) // self-contained
	if err := leaf(w, url); err != nil {
		return err
	}
	if err := closeBox(w); err != nil { // dref
		return err
	}
	if err := closeBox(w); err != nil { // dinf
		return err
	}
	if err := open(w, gomp4.BoxTypeStbl()); err != nil {
		return err
	}
	if err := open(w, gomp4.BoxTypeStsd()); err != nil {
		return err
	}
	if _, err := gomp4.Marshal(w, &gomp4.Stsd{EntryCount: 1}, gomp4.Context{}); err != nil {
		return err
	}
	mp4a := &gomp4.AudioSampleEntry{
		SampleEntry: gomp4.SampleEntry{
			AnyTypeBox:         gomp4.AnyTypeBox{Type: gomp4.BoxTypeMp4a()},
			DataReferenceIndex: 1,
		},
		ChannelCount: 2,
		SampleSize:   16,
		SampleRate:   timescale << 16,
	}
	if err := leaf(w, mp4a); err != nil {
		return err
	}
	if err := closeBox(w); err != nil { // stsd
		return err
	}
	if err := leaf(w, &stts); err != nil {
		return err
	}
	stsc := &gomp4.Stsc{EntryCount: 1, Entries: []gomp4.StscEntry{
		{FirstChunk: 1, SamplesPerChunk: uint32(len(samples)), SampleDescriptionIndex: 1},
	}}
	if err := leaf(w, stsc); err != nil {
		return err
	}
	if err := leaf(w, &stsz); err != nil {
		return err
	}
	if err := leaf(w, &gomp4.Stco{EntryCount: 1, ChunkOffset: []uint32{chunkOffset}}); err != nil {
		return err
	}
	if err := leaf(w, &stss); err != nil {
		return err
	}
	for range 4 { // stbl, minf, mdia, trak
		if err := closeBox(w); err != nil {
			return err
		}
	}
	return closeBox(w) // moov
}

// Mux serializes samples into a self-contained MP4 and returns its bytes.
func Mux(samples []Sample, timescale uint32) ([]byte, error) {
	// Pass 1: measure ftyp+moov size with a placeholder stco value.
	probe := &memFile{}
	if err := writeMoov(gomp4.NewWriter(probe), samples, timescale, 0); err != nil {
		return nil, err
	}
	chunkOffset := uint32(len(probe.buf)) + 8 // + mdat header
	// Pass 2: identical layout, real stco.
	out := &memFile{}
	w := gomp4.NewWriter(out)
	if err := writeMoov(w, samples, timescale, chunkOffset); err != nil {
		return nil, err
	}
	if err := open(w, gomp4.BoxTypeMdat()); err != nil {
		return nil, err
	}
	for _, s := range samples {
		if _, err := w.Write(s.Data); err != nil {
			return nil, err
		}
	}
	if err := closeBox(w); err != nil {
		return nil, err
	}
	return out.buf, nil
}

// Table is the parsed sample table plus extracted payloads.
type Table struct {
	Timescale    uint32
	Deltas       []gomp4.SttsEntry
	Sizes        []uint32
	ChunkOffsets []uint32
	Sync         []uint32
	Samples      [][]byte
}

// Demux parses an MP4 produced by Mux back into a Table.
func Demux(data []byte) (*Table, error) {
	r := bytes.NewReader(data)
	get := func(path ...gomp4.BoxType) (gomp4.IBox, error) {
		bs, err := gomp4.ExtractBoxesWithPayload(r, nil, []gomp4.BoxPath{path})
		if err != nil {
			return nil, err
		}
		if len(bs) != 1 {
			return nil, fmt.Errorf("mp4: expected 1 box at %v, got %d", path, len(bs))
		}
		return bs[0].Payload, nil
	}
	moov := gomp4.BoxTypeMoov()
	trak := gomp4.BoxTypeTrak()
	mdia := gomp4.BoxTypeMdia()
	minf := gomp4.BoxTypeMinf()
	stbl := gomp4.BoxTypeStbl()

	t := &Table{}
	if b, err := get(moov, trak, mdia, gomp4.BoxTypeMdhd()); err != nil {
		return nil, err
	} else {
		t.Timescale = b.(*gomp4.Mdhd).Timescale
	}
	if b, err := get(moov, trak, mdia, minf, stbl, gomp4.BoxTypeStts()); err != nil {
		return nil, err
	} else {
		t.Deltas = b.(*gomp4.Stts).Entries
	}
	if b, err := get(moov, trak, mdia, minf, stbl, gomp4.BoxTypeStsz()); err != nil {
		return nil, err
	} else {
		t.Sizes = b.(*gomp4.Stsz).EntrySize
	}
	if b, err := get(moov, trak, mdia, minf, stbl, gomp4.BoxTypeStco()); err != nil {
		return nil, err
	} else {
		t.ChunkOffsets = b.(*gomp4.Stco).ChunkOffset
	}
	if b, err := get(moov, trak, mdia, minf, stbl, gomp4.BoxTypeStss()); err != nil {
		return nil, err
	} else {
		t.Sync = b.(*gomp4.Stss).SampleNumber
	}
	// Extract sample payloads from mdat using stco + stsz.
	mdat, err := gomp4.ExtractBoxes(r, nil, []gomp4.BoxPath{{gomp4.BoxTypeMdat()}})
	if err != nil {
		return nil, err
	}
	if len(mdat) != 1 {
		return nil, fmt.Errorf("mp4: expected 1 mdat, got %d", len(mdat))
	}
	off := int64(mdat[0].Offset) + int64(mdat[0].HeaderSize)
	for _, sz := range t.Sizes {
		buf := make([]byte, sz)
		if _, err := r.ReadAt(buf, off); err != nil {
			return nil, err
		}
		t.Samples = append(t.Samples, buf)
		off += int64(sz)
	}
	return t, nil
}

// DiffReport summarizes a field-by-field round-trip comparison.
type DiffReport struct {
	Total     int
	Matched   int
	FirstDiff string
}

// Percent returns the matched-field percentage.
func (d DiffReport) Percent() float64 {
	if d.Total == 0 {
		return 100
	}
	return 100 * float64(d.Matched) / float64(d.Total)
}

// RoundTrip muxes and demuxes samples, comparing every field in a fixed
// order and recording the first difference.
func RoundTrip(samples []Sample, timescale uint32) (DiffReport, *Table, error) {
	var rep DiffReport
	data, err := Mux(samples, timescale)
	if err != nil {
		return rep, nil, err
	}
	t, err := Demux(data)
	if err != nil {
		return rep, nil, err
	}
	check := func(name string, ok bool) {
		rep.Total++
		if ok {
			rep.Matched++
		} else if rep.FirstDiff == "" {
			rep.FirstDiff = name
		}
	}
	check("mdhd.timescale", t.Timescale == timescale)
	check("stsz.sample_count", len(t.Sizes) == len(samples))
	expDeltas := buildStts(samples)
	check("stts.entry_count", len(t.Deltas) == len(expDeltas))
	for i := range min(len(t.Deltas), len(expDeltas)) {
		check(fmt.Sprintf("stts.entries[%d]", i), t.Deltas[i] == expDeltas[i])
	}
	for i := range min(len(t.Sizes), len(samples)) {
		check(fmt.Sprintf("stsz.entry_size[%d]", i), t.Sizes[i] == uint32(len(samples[i].Data)))
	}
	check("stss.entry_count", len(t.Sync) == len(samples))
	for i := range min(len(t.Sync), len(samples)) {
		check(fmt.Sprintf("stss.sample_number[%d]", i), t.Sync[i] == uint32(i+1))
	}
	check("mdat.sample_count", len(t.Samples) == len(samples))
	for i := range min(len(t.Samples), len(samples)) {
		check(fmt.Sprintf("mdat.sample[%d]", i), bytes.Equal(t.Samples[i], samples[i].Data))
	}
	return rep, t, nil
}

func buildStts(samples []Sample) []gomp4.SttsEntry {
	var out []gomp4.SttsEntry
	for _, s := range samples {
		if n := len(out); n > 0 && out[n-1].SampleDelta == s.Duration {
			out[n-1].SampleCount++
		} else {
			out = append(out, gomp4.SttsEntry{SampleCount: 1, SampleDelta: s.Duration})
		}
	}
	return out
}

// Anchors returns the random-access index: sample numbers of sync audio
// access units.
func Anchors(t *Table) []uint32 { return t.Sync }

// KeyframeHit returns the fraction of seek targets that land exactly on an
// index anchor.
func KeyframeHit(anchors, targets []uint32) float64 {
	if len(targets) == 0 {
		return 100
	}
	set := map[uint32]bool{}
	for _, a := range anchors {
		set[a] = true
	}
	hit := 0
	for _, tg := range targets {
		if set[tg] {
			hit++
		}
	}
	return 100 * float64(hit) / float64(len(targets))
}
