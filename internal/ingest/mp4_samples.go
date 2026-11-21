package ingest

import (
	"fmt"
	"io"
	"math/big"

	"github.com/abema/go-mp4"

	"mediameld/internal/model"
)

// parseSamples reconstructs sample timing and byte hashes from the stbl
// tables. DTS and PTS are kept in the track timescale as integers.
func parseSamples(r io.ReadSeeker, stbl *boxNode, t *model.Track) error {
	sttsNode := findChild(stbl, "stts")
	stszNode := findChild(stbl, "stsz")
	stscNode := findChild(stbl, "stsc")
	if sttsNode == nil || stszNode == nil || stscNode == nil {
		return model.NewError(model.CodeTrackMissing, "track %s missing sample tables", t.ID).WithOffset(int64(stbl.Offset))
	}
	if findChild(stbl, "stco") == nil && findChild(stbl, "co64") == nil {
		return model.NewError(model.CodeTrackMissing, "track %s missing chunk offsets", t.ID).WithOffset(int64(stbl.Offset))
	}

	stts, err := unmarshalAt(r, sttsNode, &mp4.Stts{})
	if err != nil {
		return err
	}
	stsz, err := unmarshalAt(r, stszNode, &mp4.Stsz{})
	if err != nil {
		return err
	}
	stsc, err := unmarshalAt(r, stscNode, &mp4.Stsc{})
	if err != nil {
		return err
	}
	offsets, err := chunkOffsets(r, stbl)
	if err != nil {
		return err
	}

	sizes := make([]int32, 0, stsz.SampleCount)
	if stsz.SampleSize != 0 {
		for i := uint32(0); i < stsz.SampleCount; i++ {
			sizes = append(sizes, int32(stsz.SampleSize))
		}
	} else {
		if uint32(len(stsz.EntrySize)) != stsz.SampleCount {
			return model.NewError(model.CodeMP4BoxLength,
				"stsz has %d sizes for %d samples", len(stsz.EntrySize), stsz.SampleCount).WithOffset(int64(stszNode.Offset))
		}
		for _, s := range stsz.EntrySize {
			sizes = append(sizes, int32(s))
		}
	}

	// Expand the time-to-sample table into per-sample deltas.
	var deltas []uint32
	for _, e := range stts.Entries {
		for i := uint32(0); i < e.SampleCount; i++ {
			deltas = append(deltas, e.SampleDelta)
		}
	}
	if len(deltas) != len(sizes) {
		return model.NewError(model.CodeMP4BoxLength,
			"stts covers %d samples but stsz has %d", len(deltas), len(sizes)).WithOffset(int64(sttsNode.Offset))
	}

	cts := make(map[int]int64)
	if cttsNode := findChild(stbl, "ctts"); cttsNode != nil {
		ctts, err := unmarshalAt(r, cttsNode, &mp4.Ctts{})
		if err != nil {
			return err
		}
		idx := 0
		for _, e := range ctts.Entries {
			for i := uint32(0); i < e.SampleCount; i++ {
				off := int64(int32(e.SampleOffsetV0))
				if ctts.GetVersion() == 1 {
					off = int64(e.SampleOffsetV1)
				}
				if idx < len(sizes) {
					cts[idx] = off
				}
				idx++
			}
		}
	}

	// Map each sample index to its absolute file offset via chunk layout.
	fileOffsets, err := sampleFileOffsets(stsc, offsets, sizes)
	if err != nil {
		return err
	}

	var dts int64
	for i, size := range sizes {
		pts := dts + cts[i]
		abs := fileOffsets[i]
		sum, err := hashRegion(r, abs, int64(size))
		if err != nil {
			return model.TagOffset(model.Tag(err, t.ID), abs)
		}
		t.Samples = append(t.Samples, model.Sample{
			Index: i, DTS: dts, PTS: pts, Duration: int64(deltas[i]),
			Size: size, Hash: sum, SourceOffset: abs,
		})
		dts += int64(deltas[i])
	}
	return nil
}

func chunkOffsets(r io.ReadSeeker, stbl *boxNode) ([]uint64, error) {
	if n := findChild(stbl, "stco"); n != nil {
		stco, err := unmarshalAt(r, n, &mp4.Stco{})
		if err != nil {
			return nil, err
		}
		out := make([]uint64, len(stco.ChunkOffset))
		for i, v := range stco.ChunkOffset {
			out[i] = uint64(v)
		}
		return out, nil
	}
	n := findChild(stbl, "co64")
	co64, err := unmarshalAt(r, n, &mp4.Co64{})
	if err != nil {
		return nil, err
	}
	return co64.ChunkOffset, nil
}

// sampleFileOffsets walks the sample-to-chunk table to compute the byte
// offset of every sample without reading any sample payload.
func sampleFileOffsets(stsc *mp4.Stsc, chunkOffsets []uint64, sizes []int32) ([]int64, error) {
	out := make([]int64, len(sizes))
	if len(stsc.Entries) == 0 {
		return nil, model.NewError(model.CodeMP4BoxLength, "stsc has no entries")
	}
	// Validate chunk references.
	sampleIdx := 0
	for ei, e := range stsc.Entries {
		first := int(e.FirstChunk) - 1
		if first < 0 || first >= len(chunkOffsets) {
			return nil, model.NewError(model.CodeMP4BoxLength,
				"stsc entry %d references chunk %d of %d", ei, e.FirstChunk, len(chunkOffsets))
		}
	}
	for ci := 0; ci < len(chunkOffsets) && sampleIdx < len(sizes); ci++ {
		spc := samplesPerChunk(stsc, uint32(ci+1))
		if spc == 0 {
			return nil, model.NewError(model.CodeMP4BoxLength, "chunk %d maps to zero samples", ci+1)
		}
		off := int64(chunkOffsets[ci])
		for k := uint32(0); k < spc && sampleIdx < len(sizes); k++ {
			out[sampleIdx] = off
			off += int64(sizes[sampleIdx])
			sampleIdx++
		}
	}
	if sampleIdx != len(sizes) {
		return nil, model.NewError(model.CodeMP4BoxLength,
			"chunk layout covers %d of %d samples", sampleIdx, len(sizes))
	}
	return out, nil
}

func samplesPerChunk(stsc *mp4.Stsc, chunk uint32) uint32 {
	spc := uint32(0)
	for _, e := range stsc.Entries {
		if e.FirstChunk <= chunk {
			spc = e.SamplesPerChunk
		} else {
			break
		}
	}
	return spc
}

// parseVisualEntry reads the sample entry to recover coded dimensions, colour
// description and clean-aperture crop. stsd is parsed by hand because sample
// entries carry fixed visual/audio fields before their child boxes, which a
// generic box scan cannot know about.
func parseVisualEntry(r io.ReadSeeker, stsd *boxNode, t *model.Track) error {
	if _, err := r.Seek(int64(stsd.Offset), io.SeekStart); err != nil {
		return err
	}
	bi, err := mp4.ReadBoxInfo(r)
	if err != nil {
		return model.WrapError(model.CodeMP4BoxLength, err, "stsd header").WithOffset(int64(stsd.Offset))
	}
	payloadEnd := int64(bi.Offset + bi.Size)
	pos := int64(bi.Offset+bi.HeaderSize) + 8 // version/flags + entry_count
	if pos > payloadEnd {
		return model.NewError(model.CodeMP4BoxLength, "stsd too short").WithOffset(pos)
	}
	for pos+8 <= payloadEnd {
		entry, err := readRawBox(r, pos, payloadEnd)
		if err != nil {
			return model.Tag(err, t.ID)
		}
		if !isVisualEntry(entry.typ) {
			pos = entry.end
			continue
		}
		t.SampleDesc = entry.typ
		if entry.end-int64(entry.payloadStart) < 78 {
			return model.NewError(model.CodeMP4BoxLength, "visual sample entry too short").WithOffset(entry.offset).WithTrack(t.ID)
		}
		if _, err := r.Seek(int64(entry.payloadStart)+24, io.SeekStart); err != nil {
			return err
		}
		var dims [4]byte
		if _, err := io.ReadFull(r, dims[:]); err != nil {
			return model.WrapError(model.CodeMP4BoxLength, err, "sample entry dimensions").WithOffset(int64(entry.payloadStart))
		}
		t.CodedWidth = int(dims[0])<<8 | int(dims[1])
		t.CodedHeight = int(dims[2])<<8 | int(dims[3])
		t.Display.Width, t.Display.Height = t.CodedWidth, t.CodedHeight
		childStart := int64(entry.payloadStart) + 78
		for childStart+8 <= entry.end {
			child, err := readRawBox(r, childStart, entry.end)
			if err != nil {
				return model.Tag(err, t.ID)
			}
			switch child.typ {
			case "colr":
				if err := parseColr(r, child, t); err != nil {
					return err
				}
			case "clap":
				if err := parseClap(r, child, t); err != nil {
					return err
				}
			}
			childStart = child.end
		}
		return nil
	}
	return nil
}

func isVisualEntry(typ string) bool {
	switch typ {
	case "avc1", "avc3", "hvc1", "hev1", "mp4v", "encv":
		return true
	}
	return false
}

// rawBox describes a box read directly from a byte range.
type rawBox struct {
	typ          string
	offset       int64
	payloadStart int64
	end          int64
}

func readRawBox(r io.ReadSeeker, off, limit int64) (rawBox, error) {
	if off+8 > limit {
		return rawBox{}, model.NewError(model.CodeMP4BoxLength, "box header exceeds parent").WithOffset(off)
	}
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return rawBox{}, err
	}
	bi, err := mp4.ReadBoxInfo(r)
	if err != nil {
		return rawBox{}, model.WrapError(model.CodeMP4BoxLength, err, "box header").WithOffset(off)
	}
	end := int64(bi.Offset + bi.Size)
	if bi.Size < bi.HeaderSize || end > limit {
		return rawBox{}, model.NewError(model.CodeMP4BoxLength,
			"box %q size %d overruns parent ending at %d", bi.Type.String(), bi.Size, limit).WithOffset(off)
	}
	return rawBox{typ: bi.Type.String(), offset: off, payloadStart: int64(bi.Offset + bi.HeaderSize), end: end}, nil
}

// parseColr decodes the colour description, preserving unknown tokens.
func parseColr(r io.ReadSeeker, n rawBox, t *model.Track) error {
	if _, err := r.Seek(n.payloadStart, io.SeekStart); err != nil {
		return err
	}
	buf := make([]byte, n.end-n.payloadStart)
	if _, err := io.ReadFull(r, buf); err != nil {
		return model.WrapError(model.CodeMP4BoxLength, err, "colr payload").WithOffset(n.offset)
	}
	if len(buf) < 4 {
		return model.NewError(model.CodeMP4BoxLength, "colr too short").WithOffset(n.offset)
	}
	tag := string(buf[:4])
	if tag == "nclx" && len(buf) >= 11 {
		p := uint16(buf[4])<<8 | uint16(buf[5])
		tr := uint16(buf[6])<<8 | uint16(buf[7])
		m := uint16(buf[8])<<8 | uint16(buf[9])
		full := buf[10]&0x80 != 0
		t.ContainerColor = model.ColorDesc{
			Primaries: primariesName(p), Transfer: transferName(tr),
			Matrix: matrixName(m), FullRange: rangeName(full),
			OriginalTag: fmt.Sprintf("nclx:%d:%d:%d:%t", p, tr, m, full),
		}
		return nil
	}
	// Unrecognized colour type is preserved verbatim but marked unknown; this
	// never authorizes rewriting sample bytes.
	t.ContainerColor = model.UnknownColor(tag)
	return nil
}

// parseClap converts a clean aperture into a top-left crop rectangle.
func parseClap(r io.ReadSeeker, n rawBox, t *model.Track) error {
	if n.end-n.payloadStart < 32 {
		return model.NewError(model.CodeCropRange, "clap too short").WithOffset(n.offset).WithTrack(t.ID)
	}
	if _, err := r.Seek(n.payloadStart, io.SeekStart); err != nil {
		return err
	}
	var buf [32]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return model.WrapError(model.CodeMP4BoxLength, err, "clap payload").WithOffset(n.offset)
	}
	w, err := ratioToInt(int32(beU32(buf[0:])), int32(beU32(buf[4:])))
	if err != nil {
		return model.Tag(err, t.ID)
	}
	h, err := ratioToInt(int32(beU32(buf[8:])), int32(beU32(buf[12:])))
	if err != nil {
		return model.Tag(err, t.ID)
	}
	horiz, err := ratioToInt(int32(beU32(buf[16:])), int32(beU32(buf[20:])))
	if err != nil {
		return model.Tag(err, t.ID)
	}
	vert, err := ratioToInt(int32(beU32(buf[24:])), int32(beU32(buf[28:])))
	if err != nil {
		return model.Tag(err, t.ID)
	}
	frameW, frameH := t.Display.Width, t.Display.Height
	x := (frameW-w)/2 + horiz
	y := (frameH-h)/2 + vert
	t.ContainerCrop = model.Crop{Valid: true, X: x, Y: y, W: w, H: h}
	return nil
}

func beU32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func ratioToInt(n, d int32) (int, error) {
	if d == 0 {
		return 0, model.NewError(model.CodeCropRange, "clean aperture denominator is zero")
	}
	v := new(big.Rat).SetFrac64(int64(n), int64(d))
	if !v.IsInt() {
		return 0, model.NewError(model.CodeCropRange, "clean aperture %d/%d is not integral", n, d)
	}
	return int(v.Num().Int64()), nil
}

func primariesName(v uint16) string {
	switch v {
	case 1:
		return "bt709"
	case 9:
		return "bt2020"
	default:
		return "unknown"
	}
}

func transferName(v uint16) string {
	switch v {
	case 1:
		return "bt709"
	case 16:
		return "pq"
	default:
		return "unknown"
	}
}

func matrixName(v uint16) string {
	switch v {
	case 1:
		return "bt709"
	case 9:
		return "bt2020nc"
	default:
		return "unknown"
	}
}

func rangeName(full bool) string {
	if full {
		return "full"
	}
	return "limited"
}

// parseAudioEntry records the audio sample description so the track can be
// identified without decoding codec private data.
func parseAudioEntry(r io.ReadSeeker, stsd *boxNode, t *model.Track) error {
	if _, err := r.Seek(int64(stsd.Offset), io.SeekStart); err != nil {
		return err
	}
	bi, err := mp4.ReadBoxInfo(r)
	if err != nil {
		return model.WrapError(model.CodeMP4BoxLength, err, "stsd header").WithOffset(int64(stsd.Offset))
	}
	pos := int64(bi.Offset+bi.HeaderSize) + 8
	limit := int64(bi.Offset + bi.Size)
	if pos+8 > limit {
		return model.NewError(model.CodeMP4BoxLength, "stsd too short").WithOffset(pos).WithTrack(t.ID)
	}
	entry, err := readRawBox(r, pos, limit)
	if err != nil {
		return model.Tag(err, t.ID)
	}
	t.SampleDesc = entry.typ
	return nil
}
