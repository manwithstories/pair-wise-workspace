// Package mp4 reads ISO-BMFF sample tables through go-mp4 and turns them
// into a keyframe index on the archive tick grid. Every table-derived value
// is validated against the file size and cross-checked before use, so a
// malformed table can never produce an out-of-bounds read.
package mp4

import (
	"fmt"
	"io"
	"sort"

	gomp4 "github.com/abema/go-mp4"

	"archivetl/internal/clock"
	"archivetl/internal/diag"
)

// MaxSamples bounds the memory an untrusted table may allocate.
const MaxSamples = 4_000_000

// Sample is one fully validated entry of the sample table.
type Sample struct {
	Number uint32     // 1-based sample number
	Chunk  uint32     // 1-based chunk number
	Offset int64      // absolute byte offset inside the file
	Size   uint32     // payload size in bytes
	DTS    int64      // decode timestamp in timescale units
	Ticks  clock.Tick // DTS mapped onto the archive grid
	IsKey  bool
}

// Index is the validated, tick-anchored sample/keyframe index.
type Index struct {
	Source    string
	Timescale uint32
	Samples   []Sample
	Keyframes []uint32 // 1-based key sample numbers, ascending
	Duration  clock.Tick
}

func indexErr(source, op, msg string, offset int64, cause error) *diag.Error {
	e := diag.Wrap(diag.PhaseIndex, source, op, false, cause)
	return e.At(source, offset)
}

// BuildIndex parses the first video track's sample table. A structural or
// consistency failure returns a structured error and no partial index.
func BuildIndex(r io.ReadSeeker, source string) (*Index, *diag.Error) {
	fileSize, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, indexErr(source, "seek", err.Error(), 0, err)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, indexErr(source, "seek", err.Error(), 0, err)
	}

	paths := []gomp4.BoxPath{
		{gomp4.BoxTypeMoov(), gomp4.BoxTypeTrak(), gomp4.BoxTypeMdia(), gomp4.BoxTypeMdhd()},
		{gomp4.BoxTypeMoov(), gomp4.BoxTypeTrak(), gomp4.BoxTypeMdia(),
			gomp4.BoxTypeMinf(), gomp4.BoxTypeStbl(), gomp4.BoxTypeStts()},
		{gomp4.BoxTypeMoov(), gomp4.BoxTypeTrak(), gomp4.BoxTypeMdia(),
			gomp4.BoxTypeMinf(), gomp4.BoxTypeStbl(), gomp4.BoxTypeStsc()},
		{gomp4.BoxTypeMoov(), gomp4.BoxTypeTrak(), gomp4.BoxTypeMdia(),
			gomp4.BoxTypeMinf(), gomp4.BoxTypeStbl(), gomp4.BoxTypeStsz()},
		{gomp4.BoxTypeMoov(), gomp4.BoxTypeTrak(), gomp4.BoxTypeMdia(),
			gomp4.BoxTypeMinf(), gomp4.BoxTypeStbl(), gomp4.BoxTypeStco()},
		{gomp4.BoxTypeMoov(), gomp4.BoxTypeTrak(), gomp4.BoxTypeMdia(),
			gomp4.BoxTypeMinf(), gomp4.BoxTypeStbl(), gomp4.BoxTypeCo64()},
		{gomp4.BoxTypeMoov(), gomp4.BoxTypeTrak(), gomp4.BoxTypeMdia(),
			gomp4.BoxTypeMinf(), gomp4.BoxTypeStbl(), gomp4.BoxTypeStss()},
	}
	boxes, err := gomp4.ExtractBoxesWithPayload(r, nil, paths)
	if err != nil {
		return nil, indexErr(source, "box.read", "incomplete or malformed MP4 boxes",
			diag.OffsetUnknown, err)
	}

	var (
		mdhd *gomp4.Mdhd
		stts *gomp4.Stts
		stsc *gomp4.Stsc
		stsz *gomp4.Stsz
		stco *gomp4.Stco
		co64 *gomp4.Co64
		stss *gomp4.Stss
	)
	for _, b := range boxes {
		switch p := b.Payload.(type) {
		case *gomp4.Mdhd:
			mdhd = p
		case *gomp4.Stts:
			stts = p
		case *gomp4.Stsc:
			stsc = p
		case *gomp4.Stsz:
			stsz = p
		case *gomp4.Stco:
			stco = p
		case *gomp4.Co64:
			co64 = p
		case *gomp4.Stss:
			stss = p
		}
	}
	if mdhd == nil || stts == nil || stsc == nil || stsz == nil || (stco == nil && co64 == nil) {
		return nil, indexErr(source, "table.required",
			"missing one of mdhd/stts/stsc/stsz/stco|co64", diag.OffsetUnknown, diag.ErrInvalid)
	}
	if mdhd.Timescale == 0 {
		return nil, indexErr(source, "mdhd.timescale", "zero timescale",
			diag.OffsetUnknown, diag.ErrInvalid)
	}

	sizes, derr := sampleSizes(source, stsz)
	if derr != nil {
		return nil, derr
	}
	dts, derr := sampleDTS(source, stts, len(sizes))
	if derr != nil {
		return nil, derr
	}
	chunks, derr := chunkOffsets(source, stco, co64, fileSize)
	if derr != nil {
		return nil, derr
	}
	perChunk, derr := samplesPerChunk(source, stsc, len(chunks), len(sizes))
	if derr != nil {
		return nil, derr
	}
	keySet, derr := keySet(source, stss, len(sizes))
	if derr != nil {
		return nil, derr
	}

	idx := &Index{Source: source, Timescale: mdhd.Timescale}
	sampleNo := uint32(1)
	for chunkIdx, chunkOff := range chunks {
		count := perChunk[chunkIdx]
		cursor := chunkOff
		for k := uint32(0); k < count; k++ {
			if int(sampleNo)-1 >= len(sizes) {
				return nil, indexErr(source, "stsc.expand",
					"stsc yields more samples than stsz declares", diag.OffsetUnknown, diag.ErrOverflow)
			}
			size := sizes[sampleNo-1]
			end := cursor + int64(size)
			if cursor < 0 || end > fileSize {
				return nil, indexErr(source, "bounds.sample",
					fmt.Sprintf("sample %d reaches byte %d beyond file size %d",
						sampleNo, end, fileSize), cursor, diag.ErrOverflow)
			}
			t, terr := clock.MP4ToTick(dts[sampleNo-1], mdhd.Timescale)
			if terr != nil {
				return nil, indexErr(source, "clock.mp4", terr.Error(),
					diag.OffsetUnknown, terr)
			}
			_, isKey := keySet[sampleNo]
			idx.Samples = append(idx.Samples, Sample{
				Number: sampleNo, Chunk: uint32(chunkIdx + 1),
				Offset: cursor, Size: size, DTS: dts[sampleNo-1],
				Ticks: t, IsKey: isKey,
			})
			if isKey {
				idx.Keyframes = append(idx.Keyframes, sampleNo)
			}
			cursor = end
			sampleNo++
		}
	}
	if int(sampleNo-1) != len(sizes) {
		return nil, indexErr(source, "stsc.expand",
			"stsc expansion covers fewer samples than stsz declares",
			diag.OffsetUnknown, diag.ErrInvalid)
	}
	if len(idx.Samples) > 0 {
		last := idx.Samples[len(idx.Samples)-1]
		dur := last.DTS
		if len(stts.Entries) > 0 {
			dur += int64(stts.Entries[len(stts.Entries)-1].SampleDelta)
		}
		idx.Duration, _ = clock.MP4ToTick(dur, mdhd.Timescale)
	}
	sort.Slice(idx.Keyframes, func(i, j int) bool { return idx.Keyframes[i] < idx.Keyframes[j] })
	return idx, nil
}

func sampleSizes(source string, stsz *gomp4.Stsz) ([]uint32, *diag.Error) {
	n := int(stsz.SampleCount)
	if n > MaxSamples {
		return nil, indexErr(source, "stsz.count", "sample count exceeds the safety cap",
			diag.OffsetUnknown, diag.ErrOverflow)
	}
	sizes := make([]uint32, n)
	if stsz.SampleSize != 0 {
		for i := range sizes {
			sizes[i] = stsz.SampleSize
		}
		return sizes, nil
	}
	if len(stsz.EntrySize) != n {
		return nil, indexErr(source, "stsz.entries",
			fmt.Sprintf("declared %d samples but %d size entries", n, len(stsz.EntrySize)),
			diag.OffsetUnknown, diag.ErrInvalid)
	}
	copy(sizes, stsz.EntrySize)
	return sizes, nil
}

func sampleDTS(source string, stts *gomp4.Stts, n int) ([]int64, *diag.Error) {
	dts := make([]int64, 0, n)
	var cumulative int64
	for _, e := range stts.Entries {
		if e.SampleCount == 0 {
			return nil, indexErr(source, "stts.zero", "zero run length in stts",
				diag.OffsetUnknown, diag.ErrInvalid)
		}
		for i := uint32(0); i < e.SampleCount; i++ {
			dts = append(dts, cumulative)
			cumulative += int64(e.SampleDelta)
		}
	}
	if len(dts) != n {
		return nil, indexErr(source, "stts.count",
			fmt.Sprintf("stts expands to %d samples, stsz says %d", len(dts), n),
			diag.OffsetUnknown, diag.ErrInvalid)
	}
	return dts, nil
}

func chunkOffsets(source string, stco *gomp4.Stco, co64 *gomp4.Co64,
	fileSize int64) ([]int64, *diag.Error) {
	var raw []int64
	switch {
	case co64 != nil:
		if len(co64.ChunkOffset) != int(co64.EntryCount) {
			return nil, indexErr(source, "co64.entries", "co64 entry count mismatch",
				diag.OffsetUnknown, diag.ErrInvalid)
		}
		for _, o := range co64.ChunkOffset {
			raw = append(raw, int64(o))
		}
	default:
		if len(stco.ChunkOffset) != int(stco.EntryCount) {
			return nil, indexErr(source, "stco.entries", "stco entry count mismatch",
				diag.OffsetUnknown, diag.ErrInvalid)
		}
		for _, o := range stco.ChunkOffset {
			raw = append(raw, int64(o))
		}
	}
	if len(raw) == 0 {
		return nil, indexErr(source, "stco.empty", "no chunks indexed",
			diag.OffsetUnknown, diag.ErrInvalid)
	}
	for i, o := range raw {
		if o < 8 || o >= fileSize {
			return nil, indexErr(source, "bounds.chunk",
				fmt.Sprintf("chunk %d offset %d outside [8,%d)", i+1, o, fileSize),
				o, diag.ErrOverflow)
		}
	}
	return raw, nil
}

func samplesPerChunk(source string, stsc *gomp4.Stsc, chunkCount, sampleCount int) ([]uint32, *diag.Error) {
	if len(stsc.Entries) == 0 || stsc.Entries[0].FirstChunk != 1 {
		return nil, indexErr(source, "stsc.first", "stsc must start at chunk 1",
			diag.OffsetUnknown, diag.ErrInvalid)
	}
	out := make([]uint32, chunkCount)
	entryIdx := 0
	for c := 1; c <= chunkCount; c++ {
		for entryIdx+1 < len(stsc.Entries) && stsc.Entries[entryIdx+1].FirstChunk <= uint32(c) {
			entryIdx++
		}
		e := stsc.Entries[entryIdx]
		if e.SamplesPerChunk == 0 {
			return nil, indexErr(source, "stsc.spc", "zero samples-per-chunk",
				diag.OffsetUnknown, diag.ErrInvalid)
		}
		out[c-1] = e.SamplesPerChunk
	}
	total := uint64(0)
	for _, n := range out {
		total += uint64(n)
	}
	if int(total) != sampleCount {
		return nil, indexErr(source, "stsc.total",
			fmt.Sprintf("stsc covers %d samples, stsz says %d", total, sampleCount),
			diag.OffsetUnknown, diag.ErrInvalid)
	}
	return out, nil
}

func keySet(source string, stss *gomp4.Stss, sampleCount int) (map[uint32]struct{}, *diag.Error) {
	set := map[uint32]struct{}{}
	if stss == nil {
		// No stss: every sample is independently decodable.
		for i := 1; i <= sampleCount; i++ {
			set[uint32(i)] = struct{}{}
		}
		return set, nil
	}
	if len(stss.SampleNumber) != int(stss.EntryCount) {
		return nil, indexErr(source, "stss.entries", "stss entry count mismatch",
			diag.OffsetUnknown, diag.ErrInvalid)
	}
	prev := uint32(0)
	for _, n := range stss.SampleNumber {
		if n < 1 || int(n) > sampleCount || n <= prev {
			return nil, indexErr(source, "stss.range",
				fmt.Sprintf("keyframe number %d invalid for %d samples", n, sampleCount),
				diag.OffsetUnknown, diag.ErrInvalid)
		}
		prev = n
		set[n] = struct{}{}
	}
	return set, nil
}

// LookupKeyAt returns the latest keyframe whose DTS is no later than t.
// It uses binary search on the validated keyframe list.
func (idx *Index) LookupKeyAt(t clock.Tick) (Sample, bool) {
	if len(idx.Keyframes) == 0 {
		return Sample{}, false
	}
	pos := sort.Search(len(idx.Keyframes), func(i int) bool {
		return idx.Samples[idx.Keyframes[i]-1].Ticks > t
	})
	// pos is the first keyframe strictly after t; the answer is the one
	// before it. Exact hits land one position further right and are included.
	if pos == 0 {
		return Sample{}, false
	}
	return idx.Samples[idx.Keyframes[pos-1]-1], true
}

// LinearScanLookup is the deliberately independent O(n) reference
// implementation used to verify the binary-search index.
func (idx *Index) LinearScanLookup(t clock.Tick) (Sample, bool) {
	var best Sample
	found := false
	for _, s := range idx.Samples {
		if s.IsKey && s.Ticks <= t {
			best, found = s, true
		}
	}
	return best, found
}

// Verify cross-checks index lookups against the linear scan at every sample
// boundary and at one tick past the end, returning the first divergence.
func (idx *Index) Verify() error {
	probes := make([]clock.Tick, 0, len(idx.Samples)+1)
	for _, s := range idx.Samples {
		probes = append(probes, s.Ticks)
	}
	probes = append(probes, idx.Duration+1)
	for _, t := range probes {
		got, gok := idx.LookupKeyAt(t)
		want, wok := idx.LinearScanLookup(t)
		if gok != wok || (gok && got.Number != want.Number) {
			return fmt.Errorf("keyframe index mismatch at tick %d: index=%v(%v) scan=%v(%v)",
				t, got.Number, gok, want.Number, wok)
		}
	}
	return nil
}
