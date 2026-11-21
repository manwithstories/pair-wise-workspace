package adapter

import (
	"encoding/binary"
	"errors"
	"io"

	mp4 "github.com/abema/go-mp4"

	"mediacore/internal/media"
)

// boxNode is one parsed box header with its absolute file offset and the
// extent of its payload. Children are parsed on demand.
type boxNode struct {
	typ      string
	off      int64 // offset of the box header
	payload  int64 // payload start
	size     int64 // total box size including header
	header   int64
	children []*boxNode
	parsed   bool
	unknown  bool
}

func (n *boxNode) end() int64 { return n.off + n.size }

func firstBox(nodes []*boxNode, typ string) *boxNode {
	for _, c := range nodes {
		if c.typ == typ {
			return c
		}
	}
	return nil
}

func allBoxes(nodes []*boxNode, typ string) []*boxNode {
	var out []*boxNode
	for _, c := range nodes {
		if c.typ == typ {
			out = append(out, c)
		}
	}
	return out
}

func (n *boxNode) first(typ string) *boxNode {
	for _, c := range n.children {
		if c.typ == typ {
			return c
		}
	}
	return nil
}

func (n *boxNode) all(typ string) []*boxNode {
	var out []*boxNode
	for _, c := range n.children {
		if c.typ == typ {
			out = append(out, c)
		}
	}
	return out
}

type mp4Walker struct {
	src     media.Source
	size    int64
	format  string
	unknown int
}

// walk reads consecutive box headers in [start,end). A header that does not
// fit is a truncation at its own offset; a payload that overruns its parent
// is an invalid length at the declaring header.
func (w *mp4Walker) walk(start, end int64, depth int) ([]*boxNode, error) {
	if depth > 16 {
		return nil, media.NewError(media.ClassMalformed, "box nesting deeper than 16 levels").
			WithContainer(w.format).WithOffset(start)
	}
	var nodes []*boxNode
	for off := start; off < end; {
		if end-off < 8 {
			if end-off == 0 {
				break
			}
			return nil, media.NewError(media.ClassTruncated,
				"%d trailing bytes cannot hold a box header", end-off).
				WithContainer(w.format).WithOffset(off).WithTolerance(0)
		}
		hdr := make([]byte, 8)
		if _, err := w.src.ReadAt(hdr, off); err != nil {
			return nil, media.NewError(media.ClassTruncated, "box header extends past end of input").
				WithContainer(w.format).WithOffset(off).WithTolerance(0).WithCause(err)
		}
		size32 := binary.BigEndian.Uint32(hdr[0:4])
		typ := string(hdr[4:8])
		header := int64(8)
		var boxSize int64
		switch size32 {
		case 0:
			boxSize = end - off
		case 1:
			if end-off < 16 {
				return nil, media.NewError(media.ClassTruncated,
					"large box header extends past end of input").
					WithContainer(w.format).WithOffset(off).WithTolerance(0)
			}
			large := make([]byte, 8)
			if _, err := w.src.ReadAt(large, off+8); err != nil {
				return nil, media.NewError(media.ClassTruncated, "large box size unreadable").
					WithContainer(w.format).WithOffset(off).WithTolerance(0).WithCause(err)
			}
			boxSize = int64(binary.BigEndian.Uint64(large))
			header = 16
		default:
			boxSize = int64(size32)
		}
		if boxSize < header {
			return nil, media.NewError(media.ClassInvalidLength,
				"box %q declares size %d, smaller than its %d-byte header", typ, boxSize, header).
				WithContainer(w.format).WithOffset(off).WithTolerance(0)
		}
		if off+boxSize > end {
			cause := "box %q declares size %d but only %d bytes remain"
			if len(hdr) < 8 {
				cause = "box %q is truncated"
			}
			return nil, media.NewError(media.ClassTruncated, cause, typ, boxSize, end-off).
				WithContainer(w.format).WithOffset(off).WithTolerance(0)
		}
		node := &boxNode{typ: typ, off: off, payload: off + header, size: boxSize, header: header}
		nodes = append(nodes, node)
		off += boxSize
	}
	return nodes, nil
}

// children returns the child boxes of a container node, parsing once. A node
// whose payload is not a valid box sequence is reported as a child of the
// container, which is how real files behave for reserved payloads.
func (w *mp4Walker) children(n *boxNode) ([]*boxNode, error) {
	return w.childrenFrom(n, n.payload)
}

// childrenFrom parses child boxes beginning at start. Sample entries have a
// fixed prefix before their child boxes, so they cannot use their payload
// start directly.
func (w *mp4Walker) childrenFrom(n *boxNode, start int64) ([]*boxNode, error) {
	if n.parsed {
		return n.children, nil
	}
	if start > n.end() {
		return nil, media.NewError(media.ClassInvalidLength,
			"child boxes would start at %d, past the end of %q at %d", start, n.typ, n.end()).
			WithContainer(w.format).WithOffset(n.off).WithTolerance(0)
	}
	kids, err := w.walk(start, n.end(), 0)
	if err != nil {
		var e *media.Error
		if errors.As(err, &e) {
			e = e.WithOffset(mergeOffset(e.Offset, n.payload))
			return nil, e
		}
		return nil, err
	}
	n.children = kids
	return kids, nil
}

func mergeOffset(reported, parent int64) int64 {
	if reported < 0 {
		return parent
	}
	return reported
}

// unmarshal decodes a leaf box payload using go-mp4's box definitions.
func (w *mp4Walker) unmarshal(n *boxNode, dst mp4.IBox) error {
	sec := io.NewSectionReader(w.src, n.payload, n.size-n.header)
	var ctx mp4.Context
	if _, err := mp4.Unmarshal(sec, uint64(n.size-n.header), dst, ctx); err != nil {
		return media.NewError(media.ClassMalformed, "cannot decode box %q: %v", n.typ, err).
			WithContainer(w.format).WithOffset(n.off).WithCause(err)
	}
	return nil
}

// parseSampleTable collects the sample tables, validates each one against
// the real input size, and returns a lazy index.
func parseSampleTable(w *mp4Walker, stbl *boxNode, timescale uint32, trackID uint32) (*sampleTable, error) {
	wrap := func(err error) error {
		var e *media.Error
		if errors.As(err, &e) {
			return e.WithTrack(trackID).WithContainer("mp4")
		}
		return media.NewError(media.ClassMalformed, "sample table: %v", err).
			WithTrack(trackID).WithContainer("mp4")
	}

	st := &sampleTable{timescale: timescale, fileSize: w.src.Size(), sizeBase: -1}

	if n := stbl.first("stsz"); n != nil {
		var b mp4.Stsz
		if err := w.unmarshal(n, &b); err != nil {
			return nil, wrap(err)
		}
		st.count = int(b.SampleCount)
		st.uniform = b.SampleSize
		if st.uniform != 0 {
			st.validateEntryBase = n.payload
		} else {
			st.sizes = b.EntrySize
			st.sizeBase = n.payload + 12
		}
	} else if n := stbl.first("stz2"); n != nil {
		return nil, wrap(media.NewError(media.ClassUnsupportedCodec, "stz2 sample sizes are outside the supported subset").
			WithOffset(n.off))
	} else {
		return nil, wrap(media.NewError(media.ClassMalformed, "stbl has no stsz").
			WithOffset(stbl.off))
	}

	if n := stbl.first("stco"); n != nil {
		var b mp4.Stco
		if err := w.unmarshal(n, &b); err != nil {
			return nil, wrap(err)
		}
		st.chunkOffsets = make([]uint64, len(b.ChunkOffset))
		for i, v := range b.ChunkOffset {
			st.chunkOffsets[i] = uint64(v)
		}
		st.chunkOffsetBase = n.payload + 8
		st.chunkOffsetWidth = 4
	} else if n := stbl.first("co64"); n != nil {
		var b mp4.Co64
		if err := w.unmarshal(n, &b); err != nil {
			return nil, wrap(err)
		}
		st.chunkOffsets = b.ChunkOffset
		st.chunkOffsetBase = n.payload + 8
		st.chunkOffsetWidth = 8
	} else {
		return nil, wrap(media.NewError(media.ClassMalformed, "stbl has no chunk offsets").
			WithOffset(stbl.off))
	}
	if len(st.chunkOffsets) == 0 {
		return nil, wrap(media.NewError(media.ClassMalformed, "chunk offset table is empty").
			WithOffset(stbl.off))
	}
	for i, c := range st.chunkOffsets {
		if int64(c) > w.src.Size() {
			return nil, wrap(media.NewError(media.ClassInvalidLength,
				"chunk %d offset %d exceeds input size %d", i, c, w.src.Size()).
				WithOffset(st.chunkOffsetBase + int64(i*st.chunkOffsetWidth)).
				WithTolerance(0))
		}
	}

	if n := stbl.first("stsc"); n != nil {
		var b mp4.Stsc
		if err := w.unmarshal(n, &b); err != nil {
			return nil, wrap(err)
		}
		for i, e := range b.Entries {
			st.stsc = append(st.stsc, stscRun{
				FirstChunk:      e.FirstChunk,
				SamplesPerChunk: e.SamplesPerChunk,
				DescIndex:       e.SampleDescriptionIndex,
				EntryOffset:     n.payload + 8 + int64(i)*12,
			})
		}
	} else {
		return nil, wrap(media.NewError(media.ClassMalformed, "stbl has no stsc").WithOffset(stbl.off))
	}

	if n := stbl.first("stts"); n != nil {
		var b mp4.Stts
		if err := w.unmarshal(n, &b); err != nil {
			return nil, wrap(err)
		}
		for _, e := range b.Entries {
			st.stts = append(st.stts, sttsRun{SampleCount: e.SampleCount, SampleDelta: e.SampleDelta})
		}
	} else {
		return nil, wrap(media.NewError(media.ClassMalformed, "stbl has no stts").WithOffset(stbl.off))
	}

	if n := stbl.first("ctts"); n != nil {
		var b mp4.Ctts
		if err := w.unmarshal(n, &b); err != nil {
			return nil, wrap(err)
		}
		for i := range b.Entries {
			st.ctts = append(st.ctts, cttsRun{
				SampleCount:  b.Entries[i].SampleCount,
				SampleOffset: b.GetSampleOffset(i),
			})
		}
	}

	if n := stbl.first("stss"); n != nil {
		var b mp4.Stss
		if err := w.unmarshal(n, &b); err != nil {
			return nil, wrap(err)
		}
		st.syncSet = make(map[uint32]bool, len(b.SampleNumber))
		for _, v := range b.SampleNumber {
			st.syncSet[v] = true
		}
	}
	st.cursorChunk = -1
	st.cursorIndex = -1
	if err := buildSampleTable(st); err != nil {
		return nil, wrap(err)
	}
	return st, nil
}

// readEntryColor looks for a colr box inside the first sample entry.
func readEntryColor(w *mp4Walker, entry *boxNode) media.Color {
	colr := entry.first("colr")
	if colr == nil {
		return media.Color{}
	}
	var b mp4.Colr
	// Unmarshal by payload size: the box header is not part of the payload
	// the marshaller expects.
	if err := w.unmarshal(colr, &b); err != nil {
		return media.Color{}
	}
	if string(b.ColourType[:]) != "nclx" {
		return media.Color{}
	}
	return media.Color{
		Present:   true,
		Primaries: b.ColourPrimaries,
		Transfer:  b.TransferCharacteristics,
		Matrix:    b.MatrixCoefficients,
		FullRange: b.FullRangeFlag,
		Source:    "track",
	}
}

// readEntryCrop derives a crop from clap when the entry carries one, and
// verifies it fits inside the coded frame using the entry/tkhd dimensions.
// clap values are 16.16 fixed point; sub-pixel apertures are rejected.
func readEntryCrop(w *mp4Walker, entry *boxNode, width, height int, trackID uint32) (*media.Crop, error) {
	clap := entry.first("clap")
	if clap == nil {
		return nil, nil
	}
	const clapPayload = 32
	if clap.size-clap.header < clapPayload {
		return nil, media.NewError(media.ClassInvalidLength,
			"clap box is shorter than its 32-byte payload").
			WithContainer("mp4").WithTrack(trackID).WithOffset(clap.off).WithTolerance(0)
	}
	buf := make([]byte, clapPayload)
	if _, err := w.src.ReadAt(buf, clap.payload); err != nil {
		return nil, media.NewError(media.ClassTruncated, "clap payload extends past end of input").
			WithContainer("mp4").WithTrack(trackID).WithOffset(clap.payload).WithTolerance(0).WithCause(err)
	}
	fixed := func(i int) (int, bool) {
		raw := int32(binary.BigEndian.Uint32(buf[i*4:]))
		if raw&0xffff != 0 {
			return 0, false
		}
		return int(raw >> 16), true
	}
	cw, okW := fixed(0)
	ch, okH := fixed(1)
	hoff, okHoff := fixed(2)
	voff, okVoff := fixed(3)
	if !okW || !okH || !okHoff || !okVoff {
		return nil, media.NewError(media.ClassMetadataConflict,
			"clap uses sub-pixel offsets, which are outside the supported subset").
			WithContainer("mp4").WithTrack(trackID).WithOffset(clap.off)
	}
	c := &media.Crop{Width: cw, Height: ch, Source: "track", X: hoff, Y: voff}
	if width > 0 && height > 0 {
		c.X = (width-cw)/2 + hoff
		c.Y = (height-ch)/2 + voff
	}
	return c, nil
}
