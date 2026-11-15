// Package moov parses the moov/trak/stbl sample tables of an MP4 file
// into flat slices. It reuses the shared box walker and never touches
// mdat payload.
package moov

import (
	"io"

	"mp4probe/internal/box"
)

// SttsEntry is one run of the time-to-sample table.
type SttsEntry struct {
	Count uint32
	Delta uint32
}

// CttsEntry is one run of the composition offset table. Offset is
// signed: version 0 values are widened, version 1 keeps its sign.
type CttsEntry struct {
	Count  uint32
	Offset int32
}

// StscEntry is one run of the sample-to-chunk table.
type StscEntry struct {
	FirstChunk      uint32
	SamplesPerChunk uint32
	SampleDescIndex uint32
}

// ElstEntry is one edit list segment. MediaTime == -1 marks an empty
// edit (dwell). Rate is 16.16 fixed point (0x00010000 == 1.0).
type ElstEntry struct {
	Duration  int64 // movie timescale
	MediaTime int64 // track timescale, -1 for empty edits
	Rate      int32 // 16.16 fixed point
}

// Trak holds the parsed tables of one track.
type Trak struct {
	TrackID       uint32
	Timescale     uint32
	MediaDuration uint64 // mdhd duration, track timescale
	TkhdDuration  uint64 // tkhd duration, movie timescale
	Handler       string // "vide", "soun", ...
	Codec         string // first stsd entry format, e.g. "avc1"

	Stts        []SttsEntry
	Ctts        []CttsEntry
	Stss        []uint32 // 1-based sync sample numbers; nil when absent
	HasStss     bool
	Stsc        []StscEntry
	Sizes       []uint32 // expanded from stsz or stz2
	ChunkOffset []uint64 // unified stco/co64
	Elst        []ElstEntry
	// ElstConflict is set when the edit list total disagrees with the
	// tkhd duration; the edit list wins per spec.
	ElstConflict bool
}

// SampleCount returns the number of samples declared by stsz/stz2.
func (t *Trak) SampleCount() int { return len(t.Sizes) }

// Moov is the parsed movie box plus mdat placement.
type Moov struct {
	MovieTimescale uint32
	MovieDuration  uint64
	Offset         uint64 // absolute offset of the moov box
	Traks          []*Trak
	MdatOffset     uint64 // absolute offset of the first mdat payload
	MdatEnd        uint64 // absolute offset one past the first mdat
	HasMdat        bool
	UnknownBoxes   int
}

// Parse walks the top-level boxes, locates moov (head or tail) and the
// first mdat, and parses every trak. A missing moov is a structural
// error.
func Parse(r io.ReaderAt, size uint64) (*Moov, error) {
	m := &Moov{}
	var moovBox box.Header
	var haveMoov bool
	w := &box.Walker{}
	err := w.Walk(r, 0, size, 0, func(h box.Header) (bool, error) {
		switch h.Type {
		case "moov":
			moovBox, haveMoov = h, true
		case "mdat":
			if !m.HasMdat {
				m.MdatOffset, m.MdatEnd, m.HasMdat = h.DataOffset(), h.End(), true
			}
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	if !haveMoov {
		return nil, box.ErrMissingBox("parse moov", "moov")
	}
	m.Offset = moovBox.Offset
	m.UnknownBoxes = w.Unknown
	start, end := box.Children(moovBox)
	tw := &box.Walker{}
	err = tw.Walk(r, start, end, 1, func(h box.Header) (bool, error) {
		switch h.Type {
		case "mvhd":
			return false, m.parseMvhd(r, h)
		case "trak":
			t, err := parseTrak(r, h)
			if err != nil {
				return false, err
			}
			m.Traks = append(m.Traks, t)
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	m.UnknownBoxes += tw.Unknown
	if m.MovieTimescale == 0 {
		return nil, box.Errorf("parse moov", moovBox.Offset, "mvhd timescale is zero")
	}
	return m, nil
}

// parseTrak parses one trak box subtree.
func parseTrak(r io.ReaderAt, th box.Header) (*Trak, error) {
	t := &Trak{}
	start, end := box.Children(th)
	var mdia, edts box.Header
	var haveMdia, haveEdts bool
	w := &box.Walker{}
	err := w.Walk(r, start, end, 2, func(h box.Header) (bool, error) {
		switch h.Type {
		case "tkhd":
			return false, t.parseTkhd(r, h)
		case "mdia":
			mdia, haveMdia = h, true
		case "edts":
			edts, haveEdts = h, true
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	if !haveMdia {
		return nil, box.ErrMissingBox("parse trak", "mdia")
	}
	if haveEdts {
		if err := t.parseEdts(r, edts); err != nil {
			return nil, err
		}
	}
	if err := t.parseMdia(r, mdia); err != nil {
		return nil, err
	}
	if len(t.Elst) > 0 {
		var sum int64
		for _, e := range t.Elst {
			sum += e.Duration
		}
		// The edit list is authoritative; a mismatch against tkhd is
		// recorded as a conflict, never silently patched.
		if d := sum - int64(t.TkhdDuration); d > 2 || d < -2 {
			t.ElstConflict = true
		}
	}
	return t, nil
}

// parseMdia handles mdhd/hdlr and descends into minf/stbl.
func (t *Trak) parseMdia(r io.ReaderAt, mh box.Header) error {
	start, end := box.Children(mh)
	var minf box.Header
	var haveMinf bool
	w := &box.Walker{}
	err := w.Walk(r, start, end, 3, func(h box.Header) (bool, error) {
		switch h.Type {
		case "mdhd":
			return false, t.parseMdhd(r, h)
		case "hdlr":
			return false, t.parseHdlr(r, h)
		case "minf":
			minf, haveMinf = h, true
		}
		return false, nil
	})
	if err != nil {
		return err
	}
	if t.Timescale == 0 {
		return box.Errorf("parse mdhd", mh.Offset, "track %d: mdhd missing or zero timescale", t.TrackID)
	}
	if !haveMinf {
		return box.ErrMissingBox("parse mdia", "minf")
	}
	ms, me := box.Children(minf)
	stbl, ok, err := box.FindChild(r, ms, me, "stbl")
	if err != nil {
		return err
	}
	if !ok {
		return box.ErrMissingBox("parse minf", "stbl")
	}
	return t.parseStbl(r, stbl)
}

// parseStbl reads the sample tables. stts/stsc/stsz(or stz2)/stco(or
// co64) are mandatory; stss and ctts are optional.
func (t *Trak) parseStbl(r io.ReaderAt, sh box.Header) error {
	start, end := box.Children(sh)
	var haveStts, haveStsc, haveSizes, haveChunks bool
	w := &box.Walker{}
	err := w.Walk(r, start, end, 4, func(h box.Header) (bool, error) {
		var err error
		switch h.Type {
		case "stsd":
			err = t.parseStsd(r, h)
		case "stts":
			t.Stts, err = parseStts(r, h)
			haveStts = err == nil
		case "ctts":
			t.Ctts, err = parseCtts(r, h)
		case "stss":
			t.Stss, err = parseStss(r, h)
			t.HasStss = err == nil
		case "stsc":
			t.Stsc, err = parseStsc(r, h)
			haveStsc = err == nil
		case "stsz":
			t.Sizes, err = parseStsz(r, h)
			haveSizes = err == nil
		case "stz2":
			t.Sizes, err = parseStz2(r, h)
			haveSizes = err == nil
		case "stco":
			t.ChunkOffset, err = parseStco(r, h)
			haveChunks = err == nil
		case "co64":
			t.ChunkOffset, err = parseCo64(r, h)
			haveChunks = err == nil
		}
		return false, err
	})
	if err != nil {
		return err
	}
	for _, req := range []struct {
		ok  bool
		typ string
	}{{haveStts, "stts"}, {haveStsc, "stsc"}, {haveSizes, "stsz/stz2"}, {haveChunks, "stco/co64"}} {
		if !req.ok {
			return box.ErrMissingBox("parse stbl", req.typ)
		}
	}
	return nil
}

// parseEdts reads the edit list box.
func (t *Trak) parseEdts(r io.ReaderAt, eh box.Header) error {
	start, end := box.Children(eh)
	elst, ok, err := box.FindChild(r, start, end, "elst")
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	p, err := box.ReadPayload(r, elst)
	if err != nil {
		return err
	}
	t.Elst, err = parseElst(p, elst.DataOffset())
	return err
}

// cursor bounds-checks sequential reads inside one box payload.
type cursor struct {
	p   []byte
	off uint64 // absolute file offset of p[0], for error reporting
	pos int
}

func (c *cursor) need(n int) error {
	if len(c.p)-c.pos < n {
		return box.ErrTruncated("parse table", c.off+uint64(c.pos), uint64(n), uint64(len(c.p)-c.pos))
	}
	return nil
}

func (c *cursor) u8() uint8 { v := c.p[c.pos]; c.pos++; return v }
func (c *cursor) u16() uint16 {
	v := uint16(c.p[c.pos])<<8 | uint16(c.p[c.pos+1])
	c.pos += 2
	return v
}
func (c *cursor) u32() uint32 {
	v := uint32(c.p[c.pos])<<24 | uint32(c.p[c.pos+1])<<16 | uint32(c.p[c.pos+2])<<8 | uint32(c.p[c.pos+3])
	c.pos += 4
	return v
}
func (c *cursor) u64() uint64 {
	hi := c.u32()
	return uint64(hi)<<32 | uint64(c.u32())
}

// fullBox reads the version/flags word and returns the version.
func (c *cursor) fullBox() (uint8, error) {
	if err := c.need(4); err != nil {
		return 0, err
	}
	v := c.u8()
	c.pos += 3 // flags
	return v, nil
}

func (m *Moov) parseMvhd(r io.ReaderAt, h box.Header) error {
	p, err := box.ReadPayload(r, h)
	if err != nil {
		return err
	}
	c := &cursor{p: p, off: h.DataOffset()}
	v, err := c.fullBox()
	if err != nil {
		return err
	}
	switch v {
	case 0:
		if err := c.need(16); err != nil {
			return err
		}
		c.pos += 8
		m.MovieTimescale = c.u32()
		m.MovieDuration = uint64(c.u32())
	case 1:
		if err := c.need(28); err != nil {
			return err
		}
		c.pos += 16
		m.MovieTimescale = c.u32()
		m.MovieDuration = c.u64()
	default:
		return box.Errorf("parse mvhd", h.DataOffset(), "unsupported version %d", v)
	}
	return nil
}

func (t *Trak) parseTkhd(r io.ReaderAt, h box.Header) error {
	p, err := box.ReadPayload(r, h)
	if err != nil {
		return err
	}
	c := &cursor{p: p, off: h.DataOffset()}
	v, err := c.fullBox()
	if err != nil {
		return err
	}
	switch v {
	case 0:
		if err := c.need(20); err != nil {
			return err
		}
		c.pos += 8
		t.TrackID = c.u32()
		c.pos += 4
		t.TkhdDuration = uint64(c.u32())
	case 1:
		if err := c.need(32); err != nil {
			return err
		}
		c.pos += 16
		t.TrackID = c.u32()
		c.pos += 4
		t.TkhdDuration = c.u64()
	default:
		return box.Errorf("parse tkhd", h.DataOffset(), "unsupported version %d", v)
	}
	return nil
}

func (t *Trak) parseMdhd(r io.ReaderAt, h box.Header) error {
	p, err := box.ReadPayload(r, h)
	if err != nil {
		return err
	}
	c := &cursor{p: p, off: h.DataOffset()}
	v, err := c.fullBox()
	if err != nil {
		return err
	}
	switch v {
	case 0:
		if err := c.need(16); err != nil {
			return err
		}
		c.pos += 8
		t.Timescale = c.u32()
		t.MediaDuration = uint64(c.u32())
	case 1:
		if err := c.need(28); err != nil {
			return err
		}
		c.pos += 16
		t.Timescale = c.u32()
		t.MediaDuration = c.u64()
	default:
		return box.Errorf("parse mdhd", h.DataOffset(), "unsupported version %d", v)
	}
	return nil
}

func (t *Trak) parseHdlr(r io.ReaderAt, h box.Header) error {
	p, err := box.ReadPayload(r, h)
	if err != nil {
		return err
	}
	c := &cursor{p: p, off: h.DataOffset()}
	if _, err := c.fullBox(); err != nil {
		return err
	}
	if err := c.need(8); err != nil {
		return err
	}
	c.pos += 4
	t.Handler = string(c.p[c.pos : c.pos+4])
	return nil
}

func (t *Trak) parseStsd(r io.ReaderAt, h box.Header) error {
	p, err := box.ReadPayload(r, h)
	if err != nil {
		return err
	}
	c := &cursor{p: p, off: h.DataOffset()}
	if _, err := c.fullBox(); err != nil {
		return err
	}
	if err := c.need(4); err != nil {
		return err
	}
	count := c.u32()
	if count == 0 {
		return box.Errorf("parse stsd", h.DataOffset(), "no sample entries")
	}
	if err := c.need(8); err != nil {
		return err
	}
	entrySize := c.u32()
	if entrySize < 8 {
		return box.Errorf("parse stsd", h.DataOffset(), "sample entry size %d < 8", entrySize)
	}
	t.Codec = string(c.p[c.pos : c.pos+4])
	return nil
}

// tableHeader reads version/flags and the entry count of a table box.
func tableHeader(r io.ReaderAt, h box.Header) (*cursor, uint8, uint32, error) {
	p, err := box.ReadPayload(r, h)
	if err != nil {
		return nil, 0, 0, err
	}
	c := &cursor{p: p, off: h.DataOffset()}
	v, err := c.fullBox()
	if err != nil {
		return nil, 0, 0, err
	}
	if err := c.need(4); err != nil {
		return nil, 0, 0, err
	}
	return c, v, c.u32(), nil
}

func parseStts(r io.ReaderAt, h box.Header) ([]SttsEntry, error) {
	c, _, count, err := tableHeader(r, h)
	if err != nil {
		return nil, err
	}
	out := make([]SttsEntry, 0, count)
	for i := uint32(0); i < count; i++ {
		if err := c.need(8); err != nil {
			return nil, err
		}
		out = append(out, SttsEntry{Count: c.u32(), Delta: c.u32()})
	}
	return out, nil
}

func parseCtts(r io.ReaderAt, h box.Header) ([]CttsEntry, error) {
	c, v, count, err := tableHeader(r, h)
	if err != nil {
		return nil, err
	}
	out := make([]CttsEntry, 0, count)
	for i := uint32(0); i < count; i++ {
		if err := c.need(8); err != nil {
			return nil, err
		}
		n := c.u32()
		off := int32(c.u32()) // two's complement: correct for v0 and v1
		out = append(out, CttsEntry{Count: n, Offset: off})
	}
	_ = v
	return out, nil
}

func parseStss(r io.ReaderAt, h box.Header) ([]uint32, error) {
	c, _, count, err := tableHeader(r, h)
	if err != nil {
		return nil, err
	}
	out := make([]uint32, 0, count)
	for i := uint32(0); i < count; i++ {
		if err := c.need(4); err != nil {
			return nil, err
		}
		out = append(out, c.u32())
	}
	return out, nil
}

func parseStsc(r io.ReaderAt, h box.Header) ([]StscEntry, error) {
	c, _, count, err := tableHeader(r, h)
	if err != nil {
		return nil, err
	}
	out := make([]StscEntry, 0, count)
	for i := uint32(0); i < count; i++ {
		if err := c.need(12); err != nil {
			return nil, err
		}
		out = append(out, StscEntry{FirstChunk: c.u32(), SamplesPerChunk: c.u32(), SampleDescIndex: c.u32()})
	}
	return out, nil
}

func parseStsz(r io.ReaderAt, h box.Header) ([]uint32, error) {
	p, err := box.ReadPayload(r, h)
	if err != nil {
		return nil, err
	}
	c := &cursor{p: p, off: h.DataOffset()}
	if _, err := c.fullBox(); err != nil {
		return nil, err
	}
	if err := c.need(8); err != nil {
		return nil, err
	}
	uniform := c.u32()
	count := c.u32()
	if uniform != 0 {
		out := make([]uint32, count)
		for i := range out {
			out[i] = uniform
		}
		return out, nil
	}
	out := make([]uint32, 0, count)
	for i := uint32(0); i < count; i++ {
		if err := c.need(4); err != nil {
			return nil, err
		}
		out = append(out, c.u32())
	}
	return out, nil
}

// parseStz2 expands the compact sample size table (4/8/16-bit fields).
func parseStz2(r io.ReaderAt, h box.Header) ([]uint32, error) {
	p, err := box.ReadPayload(r, h)
	if err != nil {
		return nil, err
	}
	c := &cursor{p: p, off: h.DataOffset()}
	if _, err := c.fullBox(); err != nil {
		return nil, err
	}
	if err := c.need(8); err != nil {
		return nil, err
	}
	c.pos += 3 // reserved
	fieldSize := c.u8()
	count := c.u32()
	out := make([]uint32, 0, count)
	switch fieldSize {
	case 4:
		for i := uint32(0); i < count; i += 2 {
			if err := c.need(1); err != nil {
				return nil, err
			}
			b := c.u8()
			out = append(out, uint32(b>>4))
			if i+1 < count {
				out = append(out, uint32(b&0x0f))
			}
		}
	case 8:
		for i := uint32(0); i < count; i++ {
			if err := c.need(1); err != nil {
				return nil, err
			}
			out = append(out, uint32(c.u8()))
		}
	case 16:
		for i := uint32(0); i < count; i++ {
			if err := c.need(2); err != nil {
				return nil, err
			}
			out = append(out, uint32(c.u16()))
		}
	default:
		return nil, box.Errorf("parse stz2", h.DataOffset(), "unsupported field size %d", fieldSize)
	}
	return out, nil
}

func parseStco(r io.ReaderAt, h box.Header) ([]uint64, error) {
	c, _, count, err := tableHeader(r, h)
	if err != nil {
		return nil, err
	}
	out := make([]uint64, 0, count)
	for i := uint32(0); i < count; i++ {
		if err := c.need(4); err != nil {
			return nil, err
		}
		out = append(out, uint64(c.u32()))
	}
	return out, nil
}

func parseCo64(r io.ReaderAt, h box.Header) ([]uint64, error) {
	c, _, count, err := tableHeader(r, h)
	if err != nil {
		return nil, err
	}
	out := make([]uint64, 0, count)
	for i := uint32(0); i < count; i++ {
		if err := c.need(8); err != nil {
			return nil, err
		}
		out = append(out, c.u64())
	}
	return out, nil
}

// parseElst parses the edit list payload. Version 1 carries 64-bit
// durations and signed 64-bit media times (negative dwell offsets).
func parseElst(p []byte, off uint64) ([]ElstEntry, error) {
	c := &cursor{p: p, off: off}
	v, err := c.fullBox()
	if err != nil {
		return nil, err
	}
	if err := c.need(4); err != nil {
		return nil, err
	}
	count := c.u32()
	out := make([]ElstEntry, 0, count)
	for i := uint32(0); i < count; i++ {
		var e ElstEntry
		switch v {
		case 0:
			if err := c.need(12); err != nil {
				return nil, err
			}
			e.Duration = int64(c.u32())
			e.MediaTime = int64(int32(c.u32()))
		case 1:
			if err := c.need(20); err != nil {
				return nil, err
			}
			e.Duration = int64(c.u64())
			e.MediaTime = int64(c.u64())
		default:
			return nil, box.Errorf("parse elst", off, "unsupported version %d", v)
		}
		if err := c.need(4); err != nil {
			return nil, err
		}
		rateInt := int16(c.u16())
		rateFrac := c.u16()
		e.Rate = int32(rateInt)<<16 | int32(rateFrac)
		if e.Duration < 0 || e.MediaTime < -1 {
			return nil, box.Errorf("parse elst", off, "corrupt entry %d: duration %d media_time %d", i, e.Duration, e.MediaTime)
		}
		out = append(out, e)
	}
	return out, nil
}
