// Package fixture generates deterministic synthetic MP4 samples and
// manifests: normal media plus fault injections (box size overflow,
// truncated mdat, DTS regression, out-of-range edit list, missing
// keyframes, cross-bitrate cut-point offset). No external media is
// referenced; every byte is derived from the seed.
package fixture

import (
	"encoding/binary"
	"io"
	"os"

	gomp4 "github.com/abema/go-mp4"

	"mediaroute/internal/model"
)

// trackSpec describes one synthetic representation file to write.
type trackSpec struct {
	timescale      uint32
	delta          uint32 // nominal sample delta, timescale units
	sizes          []uint32
	stts           []gomp4.SttsEntry // nil → single nominal run
	sync           []uint32          // stss override; nil → every keyframeEvery
	emptyStss      bool              // write stss with zero entries
	keyframeEvery  int
	elst           *gomp4.Elst // optional edit list
	declareExtra   uint64      // mdat payload bytes declared but absent
	truncateTail   uint64      // payload bytes physically missing at the tail
	overflowTrailer bool       // append a free box declaring absurd size
	payloadAt      func(idx int) []byte
}

func (ts trackSpec) sttsEntries() []gomp4.SttsEntry {
	if ts.stts != nil {
		return ts.stts
	}
	return []gomp4.SttsEntry{{SampleCount: uint32(len(ts.sizes)), SampleDelta: ts.delta}}
}

func (ts trackSpec) syncNumbers() []uint32 {
	if ts.emptyStss || ts.sync != nil {
		return ts.sync
	}
	var out []uint32
	for i := 0; i < len(ts.sizes); i += ts.keyframeEvery {
		out = append(out, uint32(i+1))
	}
	return out
}

func (ts trackSpec) duration() uint64 {
	var d uint64
	for _, e := range ts.sttsEntries() {
		d += uint64(e.SampleCount) * uint64(e.SampleDelta)
	}
	return d
}

// writeTrack writes one synthetic MP4: ftyp, moov, mdat (+faults).
func writeTrack(path string, ts trackSpec) error {
	f, err := os.Create(path)
	if err != nil {
		return model.New(model.CodeIO, "fixture create "+path, err)
	}
	defer f.Close()
	w := gomp4.NewWriter(f)
	ctx := gomp4.Context{}
	ftypLen, err := writeBox(w, ftypBox(), ctx)
	if err != nil {
		return model.New(model.CodeIO, "fixture ftyp", err)
	}
	moov0, err := ts.moovBytes(0)
	if err != nil {
		return err
	}
	stco := uint32(ftypLen + uint64(len(moov0)) + 8)
	moov, err := ts.moovBytes(stco)
	if err != nil {
		return err
	}
	if len(moov) != len(moov0) {
		return model.Newf(model.CodeIO, "fixture", "moov size drifted")
	}
	if _, err := f.Write(moov); err != nil {
		return model.New(model.CodeIO, "fixture moov", err)
	}
	return ts.writeMdat(f)
}

func (ts trackSpec) writeMdat(f *os.File) error {
	var payloadTotal uint64
	for _, s := range ts.sizes {
		payloadTotal += uint64(s)
	}
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(8+payloadTotal+ts.declareExtra))
	copy(hdr[4:8], "mdat")
	if _, err := f.Write(hdr[:]); err != nil {
		return model.New(model.CodeIO, "fixture mdat header", err)
	}
	budget := int64(payloadTotal) - int64(ts.truncateTail)
	if budget < 0 {
		budget = 0
	}
	for i := range ts.sizes {
		if budget <= 0 {
			break
		}
		p := ts.payloadAt(i)
		if int64(len(p)) > budget {
			p = p[:budget]
		}
		if _, err := f.Write(p); err != nil {
			return model.New(model.CodeIO, "fixture payload", err)
		}
		budget -= int64(len(p))
	}
	if ts.overflowTrailer {
		binary.BigEndian.PutUint32(hdr[0:4], 0xFFFFFF00)
		copy(hdr[4:8], "free")
		if _, err := f.Write(hdr[:]); err != nil {
			return model.New(model.CodeIO, "fixture trailer", err)
		}
	}
	return nil
}

// moovBytes builds the moov subtree in memory (it is small: sample
// tables only, never media data).
func (ts trackSpec) moovBytes(stco uint32) ([]byte, error) {
	s := &section{}
	w := gomp4.NewWriter(s)
	ctx := gomp4.Context{}
	start := func(t gomp4.BoxType) error {
		_, err := w.StartBox(&gomp4.BoxInfo{Type: t})
		return err
	}
	for _, t := range []gomp4.BoxType{gomp4.BoxTypeMoov(), gomp4.BoxTypeTrak()} {
		if err := start(t); err != nil {
			return nil, model.New(model.CodeIO, "fixture moov", err)
		}
	}
	if ts.elst != nil {
		if err := start(gomp4.BoxTypeEdts()); err != nil {
			return nil, model.New(model.CodeIO, "fixture edts", err)
		}
		if _, err := writeBox(w, ts.elst, ctx); err != nil {
			return nil, model.New(model.CodeIO, "fixture elst", err)
		}
		if _, err := w.EndBox(); err != nil {
			return nil, model.New(model.CodeIO, "fixture edts end", err)
		}
	}
	if err := start(gomp4.BoxTypeMdia()); err != nil {
		return nil, model.New(model.CodeIO, "fixture mdia", err)
	}
	if _, err := writeBox(w, &gomp4.Mdhd{
		Timescale: ts.timescale, DurationV0: uint32(ts.duration()),
	}, ctx); err != nil {
		return nil, model.New(model.CodeIO, "fixture mdhd", err)
	}
	for _, t := range []gomp4.BoxType{gomp4.BoxTypeMinf(), gomp4.BoxTypeStbl()} {
		if err := start(t); err != nil {
			return nil, model.New(model.CodeIO, "fixture stbl", err)
		}
	}
	stts := ts.sttsEntries()
	sync := ts.syncNumbers()
	n := uint32(len(ts.sizes))
	boxes := []gomp4.IImmutableBox{
		&gomp4.Stts{EntryCount: uint32(len(stts)), Entries: stts},
		&gomp4.Stsc{EntryCount: 1, Entries: []gomp4.StscEntry{
			{FirstChunk: 1, SamplesPerChunk: n, SampleDescriptionIndex: 1}}},
		&gomp4.Stsz{SampleCount: n, EntrySize: ts.sizes},
		&gomp4.Stss{EntryCount: uint32(len(sync)), SampleNumber: sync},
		&gomp4.Stco{EntryCount: 1, ChunkOffset: []uint32{stco}},
	}
	for _, b := range boxes {
		if _, err := writeBox(w, b, ctx); err != nil {
			return nil, model.New(model.CodeIO, "fixture stbl box", err)
		}
	}
	for i := 0; i < 4; i++ { // stbl, minf, mdia, trak, moov? -> 5 ends
		if _, err := w.EndBox(); err != nil {
			return nil, model.New(model.CodeIO, "fixture moov end", err)
		}
	}
	if _, err := w.EndBox(); err != nil { // moov
		return nil, model.New(model.CodeIO, "fixture moov end", err)
	}
	return s.buf, nil
}

func ftypBox() *gomp4.Ftyp {
	return &gomp4.Ftyp{
		MajorBrand:   gomp4.BrandISOM(),
		MinorVersion: 512,
		CompatibleBrands: []gomp4.CompatibleBrandElem{
			{CompatibleBrand: gomp4.BrandISOM()},
			{CompatibleBrand: gomp4.BrandMP41()},
		},
	}
}

// writeBox writes one complete box: header, marshalled payload, and
// the back-patched size. It returns the total box size in bytes.
func writeBox(w *gomp4.Writer, box gomp4.IImmutableBox, ctx gomp4.Context) (uint64, error) {
	if _, err := w.StartBox(&gomp4.BoxInfo{Type: box.GetType()}); err != nil {
		return 0, err
	}
	if _, err := gomp4.Marshal(w, box, ctx); err != nil {
		return 0, err
	}
	bi, err := w.EndBox()
	if err != nil {
		return 0, err
	}
	return bi.Size, nil
}

// section is an in-memory io.WriteSeeker for building small box trees.
type section struct {
	buf []byte
	off int64
}

func (s *section) Write(p []byte) (int, error) {
	end := s.off + int64(len(p))
	if end > int64(len(s.buf)) {
		nb := make([]byte, end)
		copy(nb, s.buf)
		s.buf = nb
	}
	copy(s.buf[s.off:], p)
	s.off = end
	return len(p), nil
}

func (s *section) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		s.off = offset
	case io.SeekCurrent:
		s.off += offset
	case io.SeekEnd:
		s.off = int64(len(s.buf)) + offset
	}
	return s.off, nil
}
