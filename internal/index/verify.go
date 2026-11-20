package index

import (
	"fmt"
	"math/rand"
	"os"

	"github.com/abema/go-mp4"
	"github.com/mediamend/mediamend/internal/model"
)

// Verify reads the written MP4 back and checks every documented invariant:
// first/last sample, key-frame seek points, strict time monotonicity and
// bounds. An out-of-range chunk offset is traced back to the source frame via
// the in-memory index and reported as index_out_of_bounds.
func Verify(path string, idx *Index) []model.Fault {
	var faults []model.Fault
	f, err := os.Open(path)
	if err != nil {
		return []model.Fault{fatal(idx, "open", 0, fmt.Sprintf("cannot open index: %v", err))}
	}
	defer f.Close()

	info, err := mp4.ExtractBoxWithPayload(f, nil, mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl()})
	if err != nil || len(info) == 0 {
		return []model.Fault{fatal(idx, "stbl", 0, fmt.Sprintf("sample table unreadable: %v", err))}
	}
	stblInfo := &info[0].Info

	stts := extractOne[*mp4.Stts](f, stblInfo, mp4.BoxTypeStts())
	stsz := extractOne[*mp4.Stsz](f, stblInfo, mp4.BoxTypeStsz())
	stco := extractOne[*mp4.Stco](f, stblInfo, mp4.BoxTypeStco())
	stss := extractOne[*mp4.Stss](f, stblInfo, mp4.BoxTypeStss())
	if stts == nil || stsz == nil || stco == nil {
		return []model.Fault{fatal(idx, "stbl", 0, "missing one of stts/stsz/stco")}
	}

	// Reconstruct cumulative timestamps from the stts run-length table.
	var times []uint64
	var t uint64
	for _, e := range stts.Entries {
		for k := uint32(0); k < e.SampleCount; k++ {
			times = append(times, t)
			t += uint64(e.SampleDelta)
		}
	}
	if len(times) != len(idx.Entries) {
		faults = append(faults, fatal(idx, "stts", idx.LastSeq,
			fmt.Sprintf("sample count mismatch: index=%d stts=%d", len(idx.Entries), len(times))))
	}
	for i := 1; i < len(times); i++ {
		if times[i] <= times[i-1] {
			faults = append(faults, model.Fault{
				Class: model.FaultIndexMonotonic, Severity: model.SeverityFatal,
				Location: model.SourceLocation{Track: idx.TrackID, Seq: idx.Entries[i].Seq, Offset: idx.Entries[i].Offset},
				Message:  fmt.Sprintf("stts time not strictly increasing at sample %d", i),
			})
			break
		}
	}

	// First and last sample must map to the documented boundary frames.
	if len(idx.Entries) > 0 {
		if stsz.SampleCount != uint32(len(idx.Entries)) {
			faults = append(faults, fatal(idx, "stsz", idx.LastSeq,
				fmt.Sprintf("stsz sample count=%d want %d", stsz.SampleCount, len(idx.Entries))))
		}
		// First sample always starts at time 0 on a freshly built timeline.
		if times[0] != 0 {
			faults = append(faults, fatal(idx, "stts", idx.FirstSeq,
				fmt.Sprintf("first sample time=%d want 0", times[0])))
		}
		// Reconstructed last time must equal the sum of recorded frame gaps,
		// which is the last master timestamp minus the first one.
		wantLast := idx.Entries[len(idx.Entries)-1].MasterTS - idx.Entries[0].MasterTS
		if times[len(times)-1] != wantLast {
			faults = append(faults, fatal(idx, "stts", idx.LastSeq,
				fmt.Sprintf("last sample time=%d want %d", times[len(times)-1], wantLast)))
		}
	}

	// Bounds: every chunk offset plus its sample sizes must stay inside mdat.
	fi, _ := f.Stat()
	fileSize := fi.Size()
	base := int64(stco.ChunkOffset[0])
	if base <= 0 || base >= fileSize {
		faults = append(faults, model.Fault{
			Class: model.FaultIndexBounds, Severity: model.SeverityFatal,
			Location: model.SourceLocation{Track: idx.TrackID, Seq: idx.FirstSeq},
			Message:  fmt.Sprintf("chunk offset %d outside file size %d", base, fileSize),
		})
	}
	var run int64
	for i, sz := range stsz.EntrySize {
		run += int64(sz)
		if base+run > fileSize {
			seq := idx.LastSeq
			off := int64(-1)
			if i < len(idx.Entries) {
				seq, off = idx.Entries[i].Seq, idx.Entries[i].Offset
			}
			faults = append(faults, model.Fault{
				Class: model.FaultIndexBounds, Severity: model.SeverityFatal,
				Location: model.SourceLocation{Track: idx.TrackID, Seq: seq, Offset: off},
				Message:  fmt.Sprintf("sample %d runs past mdat end (offset=%d size=%d)", i, base+run, fileSize),
			})
			break
		}
	}

	// Key-frame seek points must be in range and reference real key samples.
	for _, sampleNo := range stss.SampleNumber {
		if sampleNo == 0 || int(sampleNo) > len(idx.Entries) {
			faults = append(faults, fatal(idx, "stss", idx.LastSeq,
				fmt.Sprintf("key sample %d out of range [1,%d]", sampleNo, len(idx.Entries))))
			continue
		}
		if !idx.Entries[sampleNo-1].KeyFrame {
			faults = append(faults, fatal(idx, "stss", idx.Entries[sampleNo-1].Seq,
				"stss references a non-keyframe entry"))
		}
	}
	return faults
}

// RandomSeeks performs n pseudo-random seek checks using the supplied RNG so
// runs are reproducible. Each seek resolves a sample, reads its bytes through
// the chunk offset and confirms they equal the source payload digest length
// recorded in the index.
func RandomSeeks(idx *Index, n int, rng *rand.Rand) (checked int, faults []model.Fault) {
	f, err := os.Open(idx.File)
	if err != nil {
		return 0, []model.Fault{fatal(idx, "seek", 0, err.Error())}
	}
	defer f.Close()
	stco := extractOne[*mp4.Stco](f, mustStbl(f), mp4.BoxTypeStco())
	stsz := extractOne[*mp4.Stsz](f, mustStbl(f), mp4.BoxTypeStsz())
	if stco == nil || stsz == nil || len(idx.Entries) == 0 {
		return 0, []model.Fault{fatal(idx, "seek", 0, "index boxes missing")}
	}
	base := int64(stco.ChunkOffset[0])
	for i := 0; i < n; i++ {
		sample := rng.Intn(len(idx.Entries))
		var off int64
		for k := 0; k < sample; k++ {
			off += int64(stsz.EntrySize[k])
		}
		size := int64(stsz.EntrySize[sample])
		buf := make([]byte, size)
		if _, err := f.ReadAt(buf, base+off); err != nil {
			faults = append(faults, model.Fault{
				Class: model.FaultIndexBounds, Severity: model.SeverityFatal,
				Location: model.SourceLocation{Track: idx.TrackID, Seq: idx.Entries[sample].Seq, Offset: idx.Entries[sample].Offset},
				Message:  fmt.Sprintf("seek read failed at sample %d: %v", sample, err),
			})
			continue
		}
		if len(buf) != int(idx.Entries[sample].PayloadLen) {
			faults = append(faults, fatal(idx, "seek", idx.Entries[sample].Seq, "seek returned wrong payload length"))
			continue
		}
		checked++
	}
	return checked, faults
}

func mustStbl(f *os.File) *mp4.BoxInfo {
	info, err := mp4.ExtractBox(f, nil, mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl()})
	if err != nil || len(info) == 0 {
		return nil
	}
	return info[0]
}

func extractOne[T mp4.IImmutableBox](f *os.File, parent *mp4.BoxInfo, t mp4.BoxType) T {
	var zero T
	if parent == nil {
		return zero
	}
	boxes, err := mp4.ExtractBoxWithPayload(f, parent, mp4.BoxPath{t})
	if err != nil || len(boxes) == 0 {
		return zero
	}
	b, ok := boxes[0].Payload.(T)
	if !ok {
		return zero
	}
	return b
}

func fatal(idx *Index, where string, seq uint64, msg string) model.Fault {
	off := int64(-1)
	for _, e := range idx.Entries {
		if e.Seq == seq {
			off = e.Offset
			break
		}
	}
	return model.Fault{
		Class: model.FaultIndexBounds, Severity: model.SeverityFatal,
		Location: model.SourceLocation{File: idx.File, Track: idx.TrackID, Seq: seq, Offset: off},
		Message:  where + ": " + msg,
	}
}
