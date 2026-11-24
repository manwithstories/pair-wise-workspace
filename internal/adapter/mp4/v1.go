// Package mp4 implements the versioned MP4 box adapter (v1) using
// github.com/abema/go-mp4. It writes a lightweight index-oriented
// container (ftyp, mdat, moov with the sample tables) and parses box
// hierarchy plus sample tables back, validating every offset and length
// before reading.
package mp4

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"media-check/internal/model"

	gomp4 "github.com/abema/go-mp4"
)

const (
	AdapterID = "mp4-box"
	Version   = "v1"
)

// Adapter is the MP4 box adapter v1.
type Adapter struct{}

func (Adapter) ID() string      { return AdapterID }
func (Adapter) Version() string { return Version }

// Limits guard memory and offset arithmetic on untrusted input.
const (
	MaxBoxSize   = 1 << 28 // 256 MiB per box
	MaxSamples   = 1 << 20 // samples per parse
	MaxTableRows = 1 << 20 // rows per sample table
)

// SampleTable is the parsed sample-level index of one track.
type SampleTable struct {
	Timescale    uint32
	Delta        uint32   // constant sample duration from stts
	SampleSizes  []uint32 // stsz
	ChunkOffsets []uint32 // stco
	KeySamples   []uint32 // stss, 1-based sample numbers; empty = no index
	MdatOffset   uint64   // absolute offset of mdat payload
	MdatLength   uint64   // mdat payload length
}

// BoxNode records one visited box for hierarchy reporting.
type BoxNode struct {
	Path   string
	Offset uint64
	Size   uint64
}

// memWriteSeeker is an in-memory io.WriteSeeker for the go-mp4 writer.
type memWriteSeeker struct {
	buf []byte
	pos int64
}

func (m *memWriteSeeker) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if end > int64(len(m.buf)) {
		nb := make([]byte, end)
		copy(nb, m.buf)
		m.buf = nb
	}
	copy(m.buf[m.pos:end], p)
	m.pos = end
	return len(p), nil
}

func (m *memWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = m.pos + offset
	case io.SeekEnd:
		abs = int64(len(m.buf)) + offset
	}
	if abs < 0 {
		return 0, fmt.Errorf("negative seek %d", abs)
	}
	m.pos = abs
	return abs, nil
}

// writeBox emits one complete leaf box: header (patched by EndBox),
// payload via go-mp4 Marshal, then the closing size patch.
func writeBox(mw *gomp4.Writer, box gomp4.IImmutableBox) error {
	if _, err := mw.StartBox(&gomp4.BoxInfo{Type: box.GetType()}); err != nil {
		return err
	}
	if _, err := gomp4.Marshal(mw, box, gomp4.Context{}); err != nil {
		return err
	}
	_, err := mw.EndBox()
	return err
}

// Marshal builds a complete MP4 file: ftyp, mdat (samples concatenated),
// then moov carrying the sample tables with offsets pointing into mdat.
// keySamples are 1-based sample numbers; nil means "no index written".
func (a Adapter) Marshal(samples [][]byte, keySamples []uint32, timescale, delta uint32) ([]byte, error) {
	if len(samples) > MaxSamples {
		return nil, fmt.Errorf("sample count %d exceeds limit", len(samples))
	}
	w := &memWriteSeeker{}
	mw := gomp4.NewWriter(w)

	if err := writeBox(mw, &gomp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 1,
		CompatibleBrands: []gomp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
		},
	}); err != nil {
		return nil, err
	}

	mdat, err := mw.StartBox(&gomp4.BoxInfo{Type: gomp4.BoxTypeMdat()})
	if err != nil {
		return nil, err
	}
	payloadStart := mdat.Offset + mdat.HeaderSize
	var sizes []uint32
	for _, s := range samples {
		if _, err := mw.Write(s); err != nil {
			return nil, err
		}
		sizes = append(sizes, uint32(len(s)))
	}
	if _, err := mw.EndBox(); err != nil {
		return nil, err
	}

	if _, err := mw.StartBox(&gomp4.BoxInfo{Type: gomp4.BoxTypeMoov()}); err != nil {
		return nil, err
	}
	if err := writeBox(mw, &gomp4.Mvhd{
		FullBox:            gomp4.FullBox{Version: 0},
		CreationTimeV0:     0,
		ModificationTimeV0: 0,
		Timescale:          timescale,
		DurationV0:         uint32(len(samples)) * delta,
		Rate:               0x00010000,
		Volume:             0x0100,
		Matrix:             [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000},
		NextTrackID:        2,
	}); err != nil {
		return nil, err
	}
	if err := a.writeTrack(mw, payloadStart, sizes, keySamples, timescale, delta); err != nil {
		return nil, err
	}
	if _, err := mw.EndBox(); err != nil { // moov
		return nil, err
	}
	return w.buf, nil
}

// writeTrack emits trak/mdia/minf/stbl with the sample tables. One chunk
// holds all samples, so stco has a single entry.
func (a Adapter) writeTrack(mw *gomp4.Writer, chunkOffset uint64, sizes []uint32, keySamples []uint32, timescale, delta uint32) error {
	for _, t := range []gomp4.BoxType{
		gomp4.BoxTypeTrak(), gomp4.BoxTypeMdia(), gomp4.BoxTypeMinf(), gomp4.BoxTypeStbl(),
	} {
		if _, err := mw.StartBox(&gomp4.BoxInfo{Type: t}); err != nil {
			return err
		}
	}
	if err := writeBox(mw, &gomp4.Mdhd{
		FullBox:            gomp4.FullBox{Version: 0},
		CreationTimeV0:     0,
		ModificationTimeV0: 0,
		Timescale:          timescale,
		DurationV0:         uint32(len(sizes)) * delta,
	}); err != nil {
		return err
	}
	if err := writeBox(mw, &gomp4.Stts{
		EntryCount: 1,
		Entries:    []gomp4.SttsEntry{{SampleCount: uint32(len(sizes)), SampleDelta: delta}},
	}); err != nil {
		return err
	}
	if err := writeBox(mw, &gomp4.Stsc{
		EntryCount: 1,
		Entries:    []gomp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: uint32(len(sizes)), SampleDescriptionIndex: 1}},
	}); err != nil {
		return err
	}
	if err := writeBox(mw, &gomp4.Stsz{
		SampleSize: 0, SampleCount: uint32(len(sizes)), EntrySize: sizes,
	}); err != nil {
		return err
	}
	if err := writeBox(mw, &gomp4.Stco{
		EntryCount: 1, ChunkOffset: []uint32{uint32(chunkOffset)},
	}); err != nil {
		return err
	}
	if keySamples != nil {
		if err := writeBox(mw, &gomp4.Stss{
			EntryCount: uint32(len(keySamples)), SampleNumber: keySamples,
		}); err != nil {
			return err
		}
	}
	for i := 0; i < 4; i++ { // stbl, minf, mdia, trak
		if _, err := mw.EndBox(); err != nil {
			return err
		}
	}
	return nil
}

// Samples extracts sample payloads from a parsed table, bounds-checked.
func (t SampleTable) Samples(data []byte) ([][]byte, error) {
	if len(t.ChunkOffsets) == 0 {
		return nil, fmt.Errorf("no chunk offsets")
	}
	base := uint64(t.ChunkOffsets[0])
	out := make([][]byte, 0, len(t.SampleSizes))
	off := base
	for i, sz := range t.SampleSizes {
		end := off + uint64(sz)
		if end < off || end > uint64(len(data)) {
			return nil, fmt.Errorf("sample %d out of range: [%d,%d) of %d", i, off, end, len(data))
		}
		out = append(out, data[off:end])
		off = end
	}
	return out, nil
}

// ParseResult is the outcome of parsing one MP4 input.
type ParseResult struct {
	Boxes    []BoxNode
	Table    SampleTable
	Faults   model.Faults
	Complete bool // false when truncation or table damage was located
}

var containerTypes = []gomp4.BoxType{
	gomp4.BoxTypeMoov(), gomp4.BoxTypeTrak(), gomp4.BoxTypeMdia(),
	gomp4.BoxTypeMinf(), gomp4.BoxTypeStbl(),
}

// Parse walks the box hierarchy, reads the sample tables and validates
// every offset and length before any sample byte is touched. Truncated
// boxes, out-of-range sample offsets and missing keyframe index each
// produce a structured fault carrying the segment time range; the parser
// never reports a damaged input as complete.
func (a Adapter) Parse(data []byte, segIdx int, segRange model.TimeRange) ParseResult {
	res := ParseResult{}
	r := bytes.NewReader(data)
	fileSize := uint64(len(data))

	fault := func(code model.FaultCode, detail string) {
		res.Faults = append(res.Faults, model.Fault{
			Code: code, Adapter: a.ID() + "@" + a.Version(),
			Segment: segIdx, Range: segRange, Detail: detail,
		})
	}

	_, err := gomp4.ReadBoxStructure(r, func(h *gomp4.ReadHandle) (interface{}, error) {
		bi := h.BoxInfo
		if bi.Size > MaxBoxSize {
			return nil, fmt.Errorf("box %s size %d exceeds limit", bi.Type, bi.Size)
		}
		path := make([]string, 0, len(h.Path))
		for _, t := range h.Path {
			path = append(path, t.String())
		}
		res.Boxes = append(res.Boxes, BoxNode{
			Path: strings.Join(path, "/"), Offset: bi.Offset, Size: bi.Size,
		})
		if bi.Offset+bi.Size > fileSize {
			return nil, fmt.Errorf("box %s at %d size %d overruns file %d",
				bi.Type, bi.Offset, bi.Size, fileSize)
		}
		for _, ct := range containerTypes {
			if bi.Type == ct {
				_, err := h.Expand()
				return nil, err
			}
		}
		switch bi.Type {
		case gomp4.BoxTypeMdat():
			res.Table.MdatOffset = bi.Offset + bi.HeaderSize
			res.Table.MdatLength = bi.Size - bi.HeaderSize
		case gomp4.BoxTypeMdhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			res.Table.Timescale = box.(*gomp4.Mdhd).Timescale
		case gomp4.BoxTypeStts():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			stts := box.(*gomp4.Stts)
			if len(stts.Entries) > 0 {
				res.Table.Delta = stts.Entries[0].SampleDelta
			}
		case gomp4.BoxTypeStsz():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			stsz := box.(*gomp4.Stsz)
			if len(stsz.EntrySize) > MaxTableRows {
				return nil, fmt.Errorf("stsz rows %d exceed limit", len(stsz.EntrySize))
			}
			res.Table.SampleSizes = append([]uint32(nil), stsz.EntrySize...)
		case gomp4.BoxTypeStco():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			res.Table.ChunkOffsets = append([]uint32(nil), box.(*gomp4.Stco).ChunkOffset...)
		case gomp4.BoxTypeStss():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			res.Table.KeySamples = append([]uint32(nil), box.(*gomp4.Stss).SampleNumber...)
		}
		return nil, nil
	})
	if err != nil {
		fault(model.FaultBoxTruncated, fmt.Sprintf("box walk failed: %v", err))
		return res
	}

	t := res.Table
	if len(t.SampleSizes) > 0 {
		if len(t.ChunkOffsets) == 0 {
			fault(model.FaultSampleOffset, "stsz present but stco missing")
		} else {
			total := uint64(0)
			for _, sz := range t.SampleSizes {
				total += uint64(sz)
			}
			end := uint64(t.ChunkOffsets[0]) + total
			if end > t.MdatOffset+t.MdatLength || t.MdatLength == 0 {
				fault(model.FaultSampleOffset, fmt.Sprintf(
					"samples span [%d,%d) beyond mdat payload [..,%d)",
					t.ChunkOffsets[0], end, t.MdatOffset+t.MdatLength))
			}
		}
	}
	if len(t.SampleSizes) > 0 && t.KeySamples == nil {
		fault(model.FaultNoIndex, "sample table present but stss missing")
	}
	res.Complete = len(res.Faults) == 0
	return res
}
