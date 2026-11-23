// Sample-table expansion and structural validation for one track: stsz entry
// sizes, stsc/stco chunk walks, payload-range checks and sync-table checks.
package mp4

import (
	"fmt"
	"sort"

	gomp4 "github.com/abema/go-mp4"

	"archive.local/archive/internal/model"
)

func expandStsz(stsz *gomp4.Stsz, path string, trackID uint32) ([]uint32, error) {
	if stsz.SampleSize != 0 {
		out := make([]uint32, stsz.SampleCount)
		for i := range out {
			out[i] = stsz.SampleSize
		}
		return out, nil
	}
	if len(stsz.EntrySize) != int(stsz.SampleCount) {
		return nil, defect(path, trackID, model.CodeSourceCorrupt, minInt(len(stsz.EntrySize), int(stsz.SampleCount)), -1,
			fmt.Sprintf("stsz declares %d entries but %d samples", len(stsz.EntrySize), stsz.SampleCount), nil)
	}
	return stsz.EntrySize, nil
}

// expandChunkOffsets maps every sample to an absolute payload offset by
// walking stsc runs over the chunk offset table, advancing by sample size.
func expandChunkOffsets(stsc *gomp4.Stsc, stco *gomp4.Stco, co64 *gomp4.Co64, sizes []uint32,
	path string, trackID uint32) ([]uint64, error) {
	var chunks []uint64
	switch {
	case stco != nil:
		for _, o := range stco.ChunkOffset {
			chunks = append(chunks, uint64(o))
		}
	case co64 != nil:
		chunks = append(chunks, co64.ChunkOffset...)
	default:
		return nil, defect(path, trackID, model.CodeSourceCorrupt, -1, -1, "no chunk offset table", nil)
	}
	if len(stsc.Entries) == 0 {
		return nil, defect(path, trackID, model.CodeSourceCorrupt, -1, -1, "empty stsc table", nil)
	}
	out := make([]uint64, 0, len(sizes))
	si := 0
	for ri := range stsc.Entries {
		first := int(stsc.Entries[ri].FirstChunk)
		if first < 1 || first > len(chunks)+1 || stsc.Entries[ri].SamplesPerChunk == 0 {
			return nil, defect(path, trackID, model.CodeSourceCorrupt, si, -1, "invalid stsc run", nil)
		}
		last := len(chunks) + 1
		if ri+1 < len(stsc.Entries) {
			last = int(stsc.Entries[ri+1].FirstChunk)
			if last <= first {
				return nil, defect(path, trackID, model.CodeSourceCorrupt, si, -1,
					"stsc run boundaries are not increasing", nil)
			}
		}
		for ci := first; ci < last && ci <= len(chunks) && si < len(sizes); ci++ {
			off := chunks[ci-1]
			for k := uint32(0); k < stsc.Entries[ri].SamplesPerChunk && si < len(sizes); k++ {
				out = append(out, off)
				off += uint64(sizes[si])
				si++
			}
		}
	}
	for ; si < len(sizes); si++ {
		out = append(out, 0)
	}
	return out, nil
}

func sttsDelta(stts *gomp4.Stts, index int) uint32 {
	n := 0
	for _, e := range stts.Entries {
		if index < n+int(e.SampleCount) {
			return e.SampleDelta
		}
		n += int(e.SampleCount)
	}
	return 0
}

// validateRanges reports the first sample whose payload falls outside the file.
func validateRanges(t *Track, path string, trackID uint32) *model.Error {
	var lastEnd uint64
	for i := range t.Samples {
		s := &t.Samples[i]
		end := s.Offset + uint64(s.Size)
		if s.Offset >= uint64(t.FileSize) || end > uint64(t.FileSize) || end < s.Offset {
			first := uint64(0)
			if i > 0 {
				first = t.Samples[i-1].Offset + uint64(t.Samples[i-1].Size)
			}
			return &model.Error{Code: model.CodeTruncatedSample, File: path, Track: trackID,
				Sample: i, OffsetBytes: int64(s.Offset),
				Detail: fmt.Sprintf("sample %d needs %d bytes ending at %d, file is %d bytes",
					i, s.Size, end, t.FileSize),
				Recoverable: &model.Recoverable{FirstSample: 0, LastSample: i,
					FirstByte: int64(first), EndByte: t.FileSize}}
		}
		if lastEnd != 0 && s.Offset < lastEnd {
			return &model.Error{Code: model.CodeSourceCorrupt, File: path, Track: trackID,
				Sample: i, OffsetBytes: int64(s.Offset), Detail: "sample payload overlaps its predecessor",
				Recoverable: &model.Recoverable{FirstSample: 0, LastSample: i,
					FirstByte: 0, EndByte: int64(lastEnd)}}
		}
		lastEnd = end
		t.Complete = i + 1
	}
	return nil
}

// resolveSync validates the sync table and returns usable entry points.
func resolveSync(stss *gomp4.Stss, tr *Track, path string, trackID uint32) ([]uint32, error) {
	if stss == nil {
		all := make([]uint32, len(tr.Samples))
		for i := range all {
			all[i] = uint32(i + 1)
		}
		return all, nil
	}
	n := uint32(len(tr.Samples))
	out := make([]uint32, 0, len(stss.SampleNumber))
	for i, sn := range stss.SampleNumber {
		if sn == 0 || sn > n {
			return nil, &model.Error{Code: model.CodeCorruptKeyTable, File: path, Track: trackID,
				Sample: i, OffsetBytes: -1,
				Detail: fmt.Sprintf("sync entry %d references sample %d of %d", i, sn, n),
				Recoverable: &model.Recoverable{FirstSample: 0, LastSample: len(tr.Samples),
					FirstByte: -1, EndByte: tr.FileSize}}
		}
		if len(out) > 0 && sn <= out[len(out)-1] {
			return nil, &model.Error{Code: model.CodeCorruptKeyTable, File: path, Track: trackID,
				Sample: i, OffsetBytes: -1, Detail: "sync table is not strictly increasing"}
		}
		out = append(out, sn)
	}
	return out, nil
}

func containsSorted(a []uint32, v uint32) bool {
	i := sort.Search(len(a), func(i int) bool { return a[i] >= v })
	return i < len(a) && a[i] == v
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
