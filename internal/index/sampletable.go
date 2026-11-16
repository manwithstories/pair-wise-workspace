package index

import (
	"encoding/binary"
	"fmt"
	"io"

	"mp4loc/internal/box"
)

// tableEntry is one (count, value) run from stts/ctts.
type tableEntry struct {
	Count uint32
	Value int64 // delta (stts) or composition offset (ctts, signed for v1)
}

// buildSampleTable expands stts/ctts/stsc/stsz/stco|co64/stss into a
// per-sample table. Every count is cross-checked; mismatches are
// structural errors, never silently tolerated.
func buildSampleTable(ra io.ReaderAt, stbl *box.Node) (*box.SampleTable, error) {
	sizes, err := parseStsz(ra, stbl)
	if err != nil {
		return nil, err
	}
	n := len(sizes)

	stts, err := parseEntries(ra, stbl, "stts")
	if err != nil {
		return nil, err
	}
	if sumCounts(stts) != n {
		return nil, &box.StructureError{Offset: stbl.Offset, Reason: fmt.Sprintf("stts count %d != stsz sample count %d", sumCounts(stts), n)}
	}
	var ctts []tableEntry
	if stbl.Find("ctts") != nil {
		if ctts, err = parseEntries(ra, stbl, "ctts"); err != nil {
			return nil, err
		}
		if sumCounts(ctts) != n {
			return nil, &box.StructureError{Offset: stbl.Offset, Reason: fmt.Sprintf("ctts count %d != sample count %d", sumCounts(ctts), n)}
		}
	}
	offsets, err := sampleOffsets(ra, stbl, sizes)
	if err != nil {
		return nil, err
	}

	tbl := &box.SampleTable{Samples: make([]box.SampleRecord, n)}
	// DTS from stts runs; PTS = DTS + ctts offset.
	dts := int64(0)
	si := 0
	for _, e := range stts {
		for k := uint32(0); k < e.Count; k++ {
			tbl.Samples[si].Index = si
			tbl.Samples[si].DTS = dts
			dts += e.Value
			si++
		}
	}
	si = 0
	for _, e := range ctts {
		for k := uint32(0); k < e.Count; k++ {
			tbl.Samples[si].PTS = tbl.Samples[si].DTS + e.Value
			si++
		}
	}
	if ctts == nil {
		for i := range tbl.Samples {
			tbl.Samples[i].PTS = tbl.Samples[i].DTS
		}
	}
	for i := range tbl.Samples {
		tbl.Samples[i].Offset = offsets[i]
		tbl.Samples[i].Size = sizes[i]
	}

	// stss: explicit sync samples. Absence is recorded, never assumed.
	if stbl.Find("stss") != nil {
		keys, err := parseStss(ra, stbl, n)
		if err != nil {
			return nil, err
		}
		tbl.HasSTSS = true
		tbl.Keyframes = keys
		for _, k := range keys {
			tbl.Samples[k].Keyframe = true
		}
	}
	return tbl, nil
}

func sumCounts(es []tableEntry) int {
	var s int
	for _, e := range es {
		s += int(e.Count)
	}
	return s
}

// parseEntries reads an stts or ctts box into runs. ctts version 1
// offsets are sign-extended.
func parseEntries(ra io.ReaderAt, stbl *box.Node, typ string) ([]tableEntry, error) {
	node := stbl.Find(typ)
	if node == nil {
		return nil, &box.StructureError{Offset: stbl.Offset, Reason: "missing " + typ}
	}
	b, err := readBox(ra, node)
	if err != nil {
		return nil, err
	}
	if len(b) < 8 {
		return nil, &box.StructureError{Offset: node.Offset, Reason: "truncated " + typ}
	}
	version := b[0]
	count := int(binary.BigEndian.Uint32(b[4:]))
	if 8+count*8 > len(b) {
		return nil, &box.StructureError{Offset: node.Offset, Reason: fmt.Sprintf("%s entry count %d overruns box", typ, count)}
	}
	out := make([]tableEntry, 0, count)
	p := 8
	for i := 0; i < count; i++ {
		c := binary.BigEndian.Uint32(b[p:])
		var v int64
		if typ == "ctts" && version == 1 {
			v = int64(int32(binary.BigEndian.Uint32(b[p+4:])))
		} else {
			v = int64(binary.BigEndian.Uint32(b[p+4:]))
		}
		out = append(out, tableEntry{Count: c, Value: v})
		p += 8
	}
	return out, nil
}

// parseStsz returns the per-sample sizes.
func parseStsz(ra io.ReaderAt, stbl *box.Node) ([]uint32, error) {
	node := stbl.Find("stsz")
	if node == nil {
		return nil, &box.StructureError{Offset: stbl.Offset, Reason: "missing stsz"}
	}
	b, err := readBox(ra, node)
	if err != nil {
		return nil, err
	}
	if len(b) < 12 {
		return nil, &box.StructureError{Offset: node.Offset, Reason: "truncated stsz"}
	}
	uniform := binary.BigEndian.Uint32(b[4:])
	count := int(binary.BigEndian.Uint32(b[8:]))
	if uniform != 0 {
		sizes := make([]uint32, count)
		for i := range sizes {
			sizes[i] = uniform
		}
		return sizes, nil
	}
	if 12+count*4 > len(b) {
		return nil, &box.StructureError{Offset: node.Offset, Reason: fmt.Sprintf("stsz count %d overruns box", count)}
	}
	sizes := make([]uint32, count)
	for i := 0; i < count; i++ {
		sizes[i] = binary.BigEndian.Uint32(b[12+i*4:])
	}
	return sizes, nil
}

// chunkOffsets reads stco or co64.
func chunkOffsets(ra io.ReaderAt, stbl *box.Node) ([]int64, error) {
	node := stbl.Find("stco")
	wide := false
	if node == nil {
		node = stbl.Find("co64")
		wide = true
	}
	if node == nil {
		return nil, &box.StructureError{Offset: stbl.Offset, Reason: "missing stco/co64"}
	}
	b, err := readBox(ra, node)
	if err != nil {
		return nil, err
	}
	if len(b) < 8 {
		return nil, &box.StructureError{Offset: node.Offset, Reason: "truncated stco/co64"}
	}
	count := int(binary.BigEndian.Uint32(b[4:]))
	entry := 4
	if wide {
		entry = 8
	}
	if 8+count*entry > len(b) {
		return nil, &box.StructureError{Offset: node.Offset, Reason: "chunk offset count overruns box"}
	}
	out := make([]int64, count)
	for i := 0; i < count; i++ {
		if wide {
			out[i] = int64(binary.BigEndian.Uint64(b[8+i*8:]))
		} else {
			out[i] = int64(binary.BigEndian.Uint32(b[8+i*4:]))
		}
	}
	return out, nil
}

// sampleOffsets expands stsc against the chunk table and derives each
// sample's absolute file offset from stsz sizes.
func sampleOffsets(ra io.ReaderAt, stbl *box.Node, sizes []uint32) ([]int64, error) {
	chunks, err := chunkOffsets(ra, stbl)
	if err != nil {
		return nil, err
	}
	node := stbl.Find("stsc")
	if node == nil {
		return nil, &box.StructureError{Offset: stbl.Offset, Reason: "missing stsc"}
	}
	b, err := readBox(ra, node)
	if err != nil {
		return nil, err
	}
	if len(b) < 8 {
		return nil, &box.StructureError{Offset: node.Offset, Reason: "truncated stsc"}
	}
	count := int(binary.BigEndian.Uint32(b[4:]))
	if 8+count*12 > len(b) {
		return nil, &box.StructureError{Offset: node.Offset, Reason: "stsc count overruns box"}
	}
	type run struct {
		firstChunk int // 1-based
		perChunk   int
	}
	runs := make([]run, 0, count)
	for i := 0; i < count; i++ {
		fc := int(binary.BigEndian.Uint32(b[8+i*12:]))
		spc := int(binary.BigEndian.Uint32(b[8+i*12+4:]))
		if fc < 1 || fc > len(chunks)+1 {
			return nil, &box.StructureError{Offset: node.Offset, Reason: fmt.Sprintf("stsc first_chunk %d out of range", fc)}
		}
		runs = append(runs, run{firstChunk: fc, perChunk: spc})
	}
	offsets := make([]int64, 0, len(sizes))
	si := 0
	for ci := 0; ci < len(chunks); ci++ {
		chunk1 := ci + 1
		spc := 0
		for _, r := range runs {
			if r.firstChunk <= chunk1 {
				spc = r.perChunk
			} else {
				break
			}
		}
		off := chunks[ci]
		for k := 0; k < spc; k++ {
			if si >= len(sizes) {
				return nil, &box.StructureError{Offset: node.Offset, Reason: "stsc expands to more samples than stsz declares"}
			}
			offsets = append(offsets, off)
			off += int64(sizes[si])
			si++
		}
	}
	if si != len(sizes) {
		return nil, &box.StructureError{Offset: node.Offset, Reason: fmt.Sprintf("stsc expands to %d samples, stsz declares %d", si, len(sizes))}
	}
	return offsets, nil
}

// parseStss reads the sync-sample table, converting to 0-based indices
// and validating against the sample count.
func parseStss(ra io.ReaderAt, stbl *box.Node, sampleCount int) ([]int, error) {
	node := stbl.Find("stss")
	b, err := readBox(ra, node)
	if err != nil {
		return nil, err
	}
	if len(b) < 8 {
		return nil, &box.StructureError{Offset: node.Offset, Reason: "truncated stss"}
	}
	count := int(binary.BigEndian.Uint32(b[4:]))
	if 8+count*4 > len(b) {
		return nil, &box.StructureError{Offset: node.Offset, Reason: "stss count overruns box"}
	}
	out := make([]int, 0, count)
	for i := 0; i < count; i++ {
		s := int(binary.BigEndian.Uint32(b[8+i*4:]))
		if s < 1 || s > sampleCount {
			return nil, &box.StructureError{Offset: node.Offset, Reason: fmt.Sprintf("stss entry %d out of range (samples=%d)", s, sampleCount)}
		}
		out = append(out, s-1)
	}
	return out, nil
}
