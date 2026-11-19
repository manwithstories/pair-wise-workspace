// Package index: verification of a finished MP4 index against the source
// record stream. Every sample offset, size and timestamp is re-checked.
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/abema/go-mp4"

	"mediq/internal/model"
)

// VerifyReport summarises an index verification.
type VerifyReport struct {
	Samples         uint64
	Chunks          uint64
	FirstMedia      int64
	LastMedia       int64
	IndexValid      bool
	OutputSHA256    string
	Problems        []string
	SourceFaults    []*model.Fault
	UnindexedSource int
	TimestampMono   bool
}

// ProblemsAsError converts problems to a single error, or nil.
func (r *VerifyReport) ProblemsAsError() error {
	if len(r.Problems) == 0 {
		return nil
	}
	return fmt.Errorf("%s", r.Problems[0])
}

// Table is the in-memory form of an index's sample tables. Memory is one
// slice per sample; verification is an offline inspection tool, not the
// streaming ingest path.
type Table struct {
	Timescale  uint32
	SampleSize []uint32
	Offsets    []uint64
	Times      []int64 // media decode time per sample, 90 kHz ticks
}

// SampleCount returns the number of samples.
func (t *Table) SampleCount() int { return len(t.SampleSize) }

// SeekTime returns the sample index whose media time is the largest <= target.
// jumps beyond the last sample return -1? No: returns last sample.
func (t *Table) SeekTime(target int64) int {
	return sort.Search(len(t.Times), func(i int) bool { return t.Times[i] > target }) - 1
}

// ReadTable parses an MP4 file's index tables.
func ReadTable(path string) (*Table, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readTableFrom(f)
}

func readTableFrom(f *os.File) (*Table, error) {
	moovs, err := mp4.ExtractBoxesWithPayload(f, nil, []mp4.BoxPath{
		{mp4.BoxTypeMoov()},
	})
	if err != nil {
		return nil, fmt.Errorf("read moov: %w", err)
	}
	if len(moovs) != 1 {
		return nil, fmt.Errorf("expected exactly one moov, got %d", len(moovs))
	}
	moov := &moovs[0].Info

	boxes, err := mp4.ExtractBoxesWithPayload(f, moov, []mp4.BoxPath{
		{mp4.BoxTypeMvhd()},
		{mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMdhd()},
		{mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStts()},
		{mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsz()},
		{mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsc()},
		{mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStco()},
		{mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeCo64()},
	})
	if err != nil {
		return nil, fmt.Errorf("read tables: %w", err)
	}
	var (
		timescale uint32
		sttsBox   *mp4.Stts
		stszBox   *mp4.Stsz
		stscBox   *mp4.Stsc
		offsets   []uint64
	)
	for _, b := range boxes {
		switch box := b.Payload.(type) {
		case *mp4.Mdhd:
			timescale = box.Timescale
		case *mp4.Stts:
			sttsBox = box
		case *mp4.Stsz:
			stszBox = box
		case *mp4.Stsc:
			stscBox = box
		case *mp4.Stco:
			offsets = make([]uint64, len(box.ChunkOffset))
			for i, v := range box.ChunkOffset {
				offsets[i] = uint64(v)
			}
		case *mp4.Co64:
			offsets = append([]uint64(nil), box.ChunkOffset...)
		}
	}
	if timescale == 0 || sttsBox == nil || stszBox == nil || stscBox == nil {
		return nil, fmt.Errorf("index is missing mdhd/stts/stsz/stsc")
	}
	t := &Table{Timescale: timescale}
	if stszBox.SampleSize != 0 {
		t.SampleSize = make([]uint32, stszBox.SampleCount)
		for i := range t.SampleSize {
			t.SampleSize[i] = stszBox.SampleSize
		}
	} else {
		t.SampleSize = append([]uint32(nil), stszBox.EntrySize...)
	}
	if uint32(len(t.SampleSize)) != stszBox.SampleCount {
		return nil, fmt.Errorf("stsz entry count %d != sample_count %d", len(t.SampleSize), stszBox.SampleCount)
	}
	t.Times = decodeStts(sttsBox.Entries)

	// Expand chunk offsets into per-sample offsets using stsc.
	t.Offsets = make([]uint64, len(t.SampleSize))
	sample := 0
	for i, e := range stscBox.Entries {
		firstChunk := int(e.FirstChunk)
		var nextChunk int
		if i+1 < len(stscBox.Entries) {
			nextChunk = int(stscBox.Entries[i+1].FirstChunk)
		} else {
			nextChunk = len(offsets) + 1
		}
		if firstChunk < 1 || firstChunk > len(offsets)+1 {
			return nil, fmt.Errorf("stsc first_chunk %d out of range", firstChunk)
		}
		for chunk := firstChunk; chunk < nextChunk; chunk++ {
			off := offsets[chunk-1]
			for s := uint32(0); s < e.SamplesPerChunk; s++ {
				if sample >= len(t.Offsets) {
					return nil, fmt.Errorf("stsc describes more samples than stsz")
				}
				t.Offsets[sample] = off
				off += uint64(t.SampleSize[sample])
				sample++
			}
		}
	}
	if sample != len(t.Offsets) {
		return nil, fmt.Errorf("stsc describes %d samples, stsz has %d", sample, len(t.Offsets))
	}
	if len(t.Times) != len(t.SampleSize) {
		return nil, fmt.Errorf("stts covers %d samples, stsz has %d", len(t.Times), len(t.SampleSize))
	}
	return t, nil
}

func decodeStts(entries []mp4.SttsEntry) []int64 {
	var times []int64
	var cur int64
	for _, e := range entries {
		for i := uint32(0); i < e.SampleCount; i++ {
			times = append(times, cur)
			cur += int64(e.SampleDelta)
		}
	}
	return times
}

// VerifyOptions controls index verification.
type VerifyOptions struct {
	// SourcePath, when set, cross-checks each sample hash against the source
	// frame record. Samples whose source frame was quarantined are absent from
	// the index, so verification walks both streams together by sequence.
	SourcePath string
}

// VerifyFile verifies structural validity of path and, if requested, that the
// sample bytes still match the source frame hashes.
func VerifyFile(path string, opts VerifyOptions) (*VerifyReport, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rep := &VerifyReport{}
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return nil, err
	}
	rep.OutputSHA256 = hex.EncodeToString(sum.Sum(nil))

	mdatStart, mdatEnd, err := findMdat(f)
	if err != nil {
		rep.Problems = append(rep.Problems, err.Error())
		return rep, nil
	}

	// Walk the sample tables as streams: memory stays constant even for a
	// million-frame index.
	ts, err := openTableSource(f)
	if err != nil {
		rep.Problems = append(rep.Problems, "index tables unreadable: "+err.Error())
		return rep, nil
	}
	rep.TimestampMono = true
	var (
		prevEnd   uint64
		prevTime  int64
		firstOff  uint64
		lastEnd   uint64
		haveFirst bool
		sample    uint64
	)
	for {
		off, size, mt, ok, err := ts.next()
		if err != nil {
			rep.Problems = append(rep.Problems, "index tables unreadable: "+err.Error())
			return rep, nil
		}
		if !ok {
			break
		}
		if sample == 0 {
			firstOff = off
			rep.FirstMedia = mt
			haveFirst = true
		} else {
			if off != prevEnd {
				rep.Problems = append(rep.Problems,
					fmt.Sprintf("sample %d offset %d does not follow sample %d end %d", sample, off, sample-1, prevEnd))
				if len(rep.Problems) > 16 {
					return rep, nil
				}
			}
			if mt <= prevTime {
				rep.TimestampMono = false
				rep.Problems = append(rep.Problems,
					fmt.Sprintf("timestamp not increasing at sample %d: %d <= %d", sample, mt, prevTime))
			}
		}
		prevEnd, prevTime = off+uint64(size), mt
		lastEnd = prevEnd
		sample++
		if len(rep.Problems) > 16 {
			return rep, nil
		}
	}
	rep.Samples = sample
	rep.Chunks = ts.Chunks()
	if haveFirst {
		if firstOff != uint64(mdatStart) {
			rep.Problems = append(rep.Problems,
				fmt.Sprintf("first sample offset %d != mdat payload start %d", firstOff, mdatStart))
		}
		if lastEnd != uint64(mdatEnd) {
			rep.Problems = append(rep.Problems,
				fmt.Sprintf("last sample ends at %d, mdat payload ends at %d", lastEnd, mdatEnd))
		}
		rep.LastMedia = prevTime
	} else if mdatStart != mdatEnd {
		rep.Problems = append(rep.Problems,
			fmt.Sprintf("empty index but mdat holds %d bytes", mdatEnd-mdatStart))
	}

	if opts.SourcePath != "" {
		if err := verifyAgainstSourceStream(f, rep, opts.SourcePath); err != nil {
			return rep, err
		}
	}
	rep.IndexValid = len(rep.Problems) == 0
	return rep, nil
}

func findMdat(f *os.File) (int64, int64, error) {
	bis, err := mp4.ExtractBox(f, nil, mp4.BoxPath{mp4.BoxTypeMdat()})
	if err != nil {
		return 0, 0, fmt.Errorf("read mdat: %w", err)
	}
	if len(bis) != 1 {
		return 0, 0, fmt.Errorf("expected exactly one mdat, got %d", len(bis))
	}
	bi := bis[0]
	return int64(bi.Offset + bi.HeaderSize), int64(bi.Offset + bi.Size), nil
}
