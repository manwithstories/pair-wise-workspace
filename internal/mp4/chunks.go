package mp4

import (
	"encoding/binary"

	"github.com/fashionweek/mediad/internal/media"
)

// decodeChunks expands stsc/stco|co64/stsz into per-sample absolute offsets
// and sizes, verifying every byte range lies inside an mdat box.
func decodeChunks(tables map[string][]byte, n int, hdr *Header, base int64) ([]int64, []int64, error) {
	sizes, err := decodeSTSZ(tables["stsz"], n, base)
	if err != nil {
		return nil, nil, err
	}
	var chunkOff []int64
	if b, ok := tables["co64"]; ok {
		chunkOff, err = decodeCO64(b, base)
	} else {
		chunkOff, err = decodeSTCO(tables["stco"], base)
	}
	if err != nil {
		return nil, nil, err
	}
	_, samplesPerChunk, err := decodeSTSC(tables["stsc"], len(chunkOff), n, base)
	if err != nil {
		return nil, nil, err
	}
	offsets := make([]int64, n)
	sampleIdx := 0
	for chunk := 0; chunk < len(chunkOff); chunk++ {
		cnt := samplesPerChunk[chunk]
		pos := chunkOff[chunk]
		for j := 0; j < cnt; j++ {
			if sampleIdx >= n {
				return nil, nil, media.New(media.PhaseIndex, media.KindBounds, "", base,
					"stsc expansion exceeds declared sample count")
			}
			if !insideMdat(hdr, pos, sizes[sampleIdx]) {
				return nil, nil, media.New(media.PhaseIndex, media.KindBounds, "", pos,
					"sample byte range falls outside any mdat box (length out of bounds)")
			}
			offsets[sampleIdx] = pos
			pos += sizes[sampleIdx]
			sampleIdx++
		}
	}
	if sampleIdx != n {
		return nil, nil, media.New(media.PhaseIndex, media.KindBounds, "", base,
			"chunk tables cover fewer samples than stts declares")
	}
	return offsets, sizes, nil
}

func insideMdat(hdr *Header, off, size int64) bool {
	if off < 0 || size < 0 {
		return false
	}
	end := off + size
	if end < off { // int64 overflow
		return false
	}
	for _, m := range hdr.Mdats {
		// mdat payload begins after its 8/16-byte header; offsets may point at
		// the payload start, accepting the header boundary keeps real-world
		// muxers that record the mdat start rather than payload start valid.
		if off >= m.Offset && end <= m.End() {
			return true
		}
	}
	return false
}

func decodeSTSZ(buf []byte, n int, base int64) ([]int64, error) {
	if len(buf) == 0 {
		return nil, media.New(media.PhaseIndex, media.KindTruncated, "", base, "stsz box missing")
	}
	_, _, rest := FullBoxVersion(buf)
	if len(rest) < 8 {
		return nil, media.New(media.PhaseScan, media.KindTruncated, "", base, "stsz header missing")
	}
	uniform := binary.BigEndian.Uint32(rest[0:4])
	count := int(binary.BigEndian.Uint32(rest[4:8]))
	if count != n {
		return nil, media.New(media.PhaseIndex, media.KindBounds, "", base,
			"stsz sample count disagrees with stts sample count")
	}
	sizes := make([]int64, n)
	if uniform != 0 {
		for i := range sizes {
			sizes[i] = int64(uniform)
		}
		return sizes, nil
	}
	at := 8
	for i := 0; i < n; i++ {
		v, a2, e := readU32(rest, at, base)
		if e != nil {
			return nil, e
		}
		at = a2
		if v == 0 {
			return nil, media.New(media.PhaseIndex, media.KindBounds, "", base,
				"zero-size sample in stsz")
		}
		sizes[i] = int64(v)
	}
	return sizes, nil
}

func decodeSTCO(buf []byte, base int64) ([]int64, error) {
	if len(buf) == 0 {
		return nil, media.New(media.PhaseIndex, media.KindTruncated, "", base,
			"stco/co64 chunk offset box missing")
	}
	_, _, rest := FullBoxVersion(buf)
	if len(rest) < 4 {
		return nil, media.New(media.PhaseScan, media.KindTruncated, "", base, "stco count missing")
	}
	n := int(binary.BigEndian.Uint32(rest[:4]))
	at := 4
	out := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		v, a2, e := readU32(rest, at, base)
		if e != nil {
			return nil, e
		}
		at = a2
		out = append(out, int64(v))
	}
	return out, nil
}

func decodeCO64(buf []byte, base int64) ([]int64, error) {
	_, _, rest := FullBoxVersion(buf)
	if len(rest) < 4 {
		return nil, media.New(media.PhaseScan, media.KindTruncated, "", base, "co64 count missing")
	}
	n := int(binary.BigEndian.Uint32(rest[:4]))
	at := 4
	out := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		v, a2, e := readU64(rest, at, base)
		if e != nil {
			return nil, e
		}
		at = a2
		if v > uint64(^uint64(0)>>1) {
			return nil, media.New(media.PhaseIndex, media.KindOverflow, "", base,
				"co64 offset exceeds int64 range")
		}
		out = append(out, int64(v))
	}
	return out, nil
}

// decodeSTSC expands first-chunk + samples-per-chunk runs into a per-chunk
// sample count slice, including the implicit tail run.
func decodeSTSC(buf []byte, chunkCount, sampleCount int, base int64) (int, []int, error) {
	if len(buf) == 0 {
		return 0, nil, media.New(media.PhaseIndex, media.KindTruncated, "", base, "stsc box missing")
	}
	_, _, rest := FullBoxVersion(buf)
	if len(rest) < 4 {
		return 0, nil, media.New(media.PhaseScan, media.KindTruncated, "", base, "stsc count missing")
	}
	entries := int(binary.BigEndian.Uint32(rest[:4]))
	at := 4
	type run struct{ firstChunk, spc, desc uint32 }
	runs := make([]run, 0, entries)
	for i := 0; i < entries; i++ {
		fc, a1, e := readU32(rest, at, base)
		if e != nil {
			return 0, nil, e
		}
		spc, a2, e := readU32(rest, a1, base)
		if e != nil {
			return 0, nil, e
		}
		desc, a3, e := readU32(rest, a2, base)
		if e != nil {
			return 0, nil, e
		}
		at = a3
		if fc == 0 || spc == 0 {
			return 0, nil, media.New(media.PhaseIndex, media.KindBounds, "", base,
				"stsc entry uses zero first-chunk or samples-per-chunk")
		}
		runs = append(runs, run{fc, spc, desc})
	}
	perChunk := make([]int, chunkCount)
	if len(runs) == 0 {
		return 0, nil, media.New(media.PhaseIndex, media.KindBounds, "", base, "empty stsc run table")
	}
	for chunk := 0; chunk < chunkCount; chunk++ {
		// stsc first_chunk values are 1-based; pick the latest run whose
		// first_chunk is <= current 1-based chunk index.
		spc := runs[0].spc
		for _, r := range runs {
			if int(r.firstChunk) <= chunk+1 {
				spc = r.spc
			} else {
				break
			}
		}
		perChunk[chunk] = int(spc)
	}
	total := 0
	for _, c := range perChunk {
		total += c
	}
	if total != sampleCount {
		return 0, nil, media.New(media.PhaseIndex, media.KindBounds, "", base,
			"stsc expansion total disagrees with sample count")
	}
	return int(runs[0].firstChunk), perChunk, nil
}
