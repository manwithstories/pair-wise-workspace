// Package testdata generates every fixture the pipeline consumes. Nothing is
// read from disk or the network: fixtures are built in memory with fixed
// content so repeated runs produce byte-identical inputs.
package testdata

import (
	"bytes"
	"encoding/binary"
	"io"

	gomp4 "github.com/abema/go-mp4"
)

// memBuffer is an in-memory io.WriteSeeker used to assemble MP4 fragments.
// Seeks past the current end create a zero-filled hole, as an os.File would.
// Writes at an interior position (go-mp4 back-fills box headers there) never
// shrink the logical length.
type memBuffer struct {
	b     []byte
	pos   int64
	logic int64
}

func (m *memBuffer) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if end > int64(len(m.b)) {
		grown := make([]byte, end)
		copy(grown, m.b)
		m.b = grown
	}
	copy(m.b[m.pos:end], p)
	m.pos = end
	if end > m.logic {
		m.logic = end
	}
	return len(p), nil
}

func (m *memBuffer) Seek(offset int64, whence int) (int64, error) {
	var pos int64
	switch whence {
	case io.SeekStart:
		pos = offset
	case io.SeekCurrent:
		pos = m.pos + offset
	case io.SeekEnd:
		pos = m.logic + offset
	}
	if pos < 0 {
		return 0, bytes.ErrTooLarge
	}
	if pos > int64(len(m.b)) {
		grown := make([]byte, pos)
		copy(grown, m.b)
		m.b = grown
	}
	m.pos = pos
	return pos, nil
}
func (m *memBuffer) bytes() []byte { return m.b[:m.logic] }

// MP4Spec describes the deterministic sample table of a generated clip.
type MP4Spec struct {
	Timescale   uint32
	SampleCount uint32
	Delta       uint32 // constant stts sample delta
	SampleSize  uint32 // constant stsz size
	KeyEvery    uint32 // keyframe period in samples; 0 means no stss
	Chunks      uint32 // samples are spread evenly into this many chunks
}

// BuildMP4 writes a minimal standards-shaped MP4 using go-mp4's own writer:
// ftyp, moov (trak/mdia/mdhd + minf/stbl) then an mdat payload.
func BuildMP4(s MP4Spec) ([]byte, error) {
	if s.Chunks == 0 || s.SampleCount%s.Chunks != 0 {
		s.Chunks = 1
	}
	perChunk := s.SampleCount / s.Chunks

	// First pass with placeholder offsets measures the moov byte length; the
	// second pass writes the real mdat-relative chunk offsets.
	moov, err := buildMoov(s, make([]uint32, s.Chunks), perChunk)
	if err != nil {
		return nil, err
	}
	ftyp, err := buildFtyp()
	if err != nil {
		return nil, err
	}
	mdatHeader := 8
	mdatStart := len(ftyp) + len(moov) + mdatHeader

	offsets := make([]uint32, s.Chunks)
	for c := range offsets {
		offsets[c] = uint32(mdatStart + c*int(perChunk)*int(s.SampleSize))
	}
	moov, err = buildMoov(s, offsets, perChunk)
	if err != nil {
		return nil, err
	}

	out := &memBuffer{}
	out.Write(ftyp)
	out.Write(moov)
	payloadLen := int(s.SampleCount) * int(s.SampleSize)
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr, uint32(8+payloadLen))
	copy(hdr[4:], "mdat")
	out.Write(hdr)
	out.Write(make([]byte, payloadLen))
	return out.bytes(), nil
}

func buildFtyp() ([]byte, error) {
	buf := &memBuffer{}
	w := gomp4.NewWriter(buf)
	if _, err := w.StartBox(&gomp4.BoxInfo{Type: gomp4.BoxTypeFtyp()}); err != nil {
		return nil, err
	}
	ftyp := &gomp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 0,
		CompatibleBrands: []gomp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
			{CompatibleBrand: [4]byte{'m', 'p', '4', '1'}},
		},
	}
	if _, err := gomp4.Marshal(w, ftyp, gomp4.Context{}); err != nil {
		return nil, err
	}
	if _, err := w.EndBox(); err != nil {
		return nil, err
	}
	return buf.bytes(), nil
}

func buildMoov(s MP4Spec, offsets []uint32, perChunk uint32) ([]byte, error) {
	buf := &memBuffer{}
	w := gomp4.NewWriter(buf)

	open := func(t gomp4.BoxType) error {
		_, err := w.StartBox(&gomp4.BoxInfo{Type: t})
		return err
	}
	close := func() error { _, err := w.EndBox(); return err }
	payload := func(p gomp4.IBox) error {
		_, err := gomp4.Marshal(w, p, gomp4.Context{})
		return err
	}
	// leaf opens a box, writes its payload and closes it again.
	leaf := func(t gomp4.BoxType, p gomp4.IBox) error {
		if err := open(t); err != nil {
			return err
		}
		if err := payload(p); err != nil {
			return err
		}
		return close()
	}

	if err := open(gomp4.BoxTypeMoov()); err != nil {
		return nil, err
	}
	if err := open(gomp4.BoxTypeTrak()); err != nil {
		return nil, err
	}
	if err := open(gomp4.BoxTypeMdia()); err != nil {
		return nil, err
	}
	if err := leaf(gomp4.BoxTypeMdhd(), &gomp4.Mdhd{
		Timescale:  s.Timescale,
		DurationV0: s.SampleCount * s.Delta,
	}); err != nil {
		return nil, err
	}
	if err := open(gomp4.BoxTypeMinf()); err != nil {
		return nil, err
	}
	if err := open(gomp4.BoxTypeStbl()); err != nil {
		return nil, err
	}

	if err := leaf(gomp4.BoxTypeStts(), &gomp4.Stts{
		EntryCount: 1,
		Entries:    []gomp4.SttsEntry{{SampleCount: s.SampleCount, SampleDelta: s.Delta}},
	}); err != nil {
		return nil, err
	}
	if err := leaf(gomp4.BoxTypeStsc(), &gomp4.Stsc{
		EntryCount: 1,
		Entries:    []gomp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: perChunk, SampleDescriptionIndex: 1}},
	}); err != nil {
		return nil, err
	}
	if err := leaf(gomp4.BoxTypeStsz(), &gomp4.Stsz{
		SampleSize: s.SampleSize, SampleCount: s.SampleCount,
	}); err != nil {
		return nil, err
	}
	if err := leaf(gomp4.BoxTypeStco(), &gomp4.Stco{
		EntryCount: uint32(len(offsets)), ChunkOffset: offsets,
	}); err != nil {
		return nil, err
	}
	if s.KeyEvery > 0 {
		var keys []uint32
		for n := uint32(1); n <= s.SampleCount; n += s.KeyEvery {
			keys = append(keys, n)
		}
		if err := leaf(gomp4.BoxTypeStss(), &gomp4.Stss{
			EntryCount: uint32(len(keys)), SampleNumber: keys,
		}); err != nil {
			return nil, err
		}
	}

	for range [5]struct{}{} {
		if err := close(); err != nil {
			return nil, err
		}
	}
	return buf.bytes(), nil
}

// CorruptMP4 is one named mutation of a healthy clip.
type CorruptMP4 struct {
	Name string
	Data []byte
}

// BuildCorruptMP4 renders damaged fixtures, each exercising one defensive
// path of the index builder.
func BuildCorruptMP4(valid []byte, spec MP4Spec) []CorruptMP4 {
	return []CorruptMP4{
		{Name: "truncated", Data: truncateLastBox(valid)},
		{Name: "bad-chunk-offset", Data: mutateStco(valid, 0x7FFFFFFF)},
		{Name: "stss-out-of-range", Data: mutateStss(valid, spec.SampleCount+50)},
		{Name: "garbage", Data: []byte("NOTANMP4-FILE-\x00\x01\x02\x03")},
	}
}

func truncateLastBox(data []byte) []byte {
	if len(data) < 32 {
		return data[:len(data)/2]
	}
	return data[:len(data)-16]
}

func mutateStco(data []byte, replacement uint32) []byte {
	out := append([]byte(nil), data...)
	idx := indexOfBox(out, "stco")
	if idx >= 0 {
		pos := idx + 16 // header(8)+fullbox(4)+count(4) -> first offset
		if pos+4 <= len(out) {
			binary.BigEndian.PutUint32(out[pos:pos+4], replacement)
		}
	}
	return out
}

func mutateStss(data []byte, replacement uint32) []byte {
	out := append([]byte(nil), data...)
	idx := indexOfBox(out, "stss")
	if idx >= 0 {
		pos := idx + 16
		if pos+4 <= len(out) {
			binary.BigEndian.PutUint32(out[pos:pos+4], replacement)
		}
	}
	return out
}

// indexOfBox walks top-level and nested boxes linearly, matching by type.
func indexOfBox(data []byte, typ string) int {
	var walk func(start, end int) int
	walk = func(start, end int) int {
		for i := start; i+8 <= end; {
			size := int(binary.BigEndian.Uint32(data[i : i+4]))
			if size < 8 || i+size > end {
				return -1
			}
			if string(data[i+4:i+8]) == typ {
				return i
			}
			if found := walk(i+8, i+size); found >= 0 {
				return found
			}
			i += size
		}
		return -1
	}
	return walk(0, len(data))
}
