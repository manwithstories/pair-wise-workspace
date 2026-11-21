package adapter

import (
	"fmt"
	"math"
	"sort"

	"mediacore/internal/media"
)

// stscRun is one stsc table entry: starting at FirstChunk (1-based), each
// chunk holds SamplesPerChunk samples.
type stscRun struct {
	FirstChunk      uint32
	SamplesPerChunk uint32
	DescIndex       uint32
	EntryOffset     int64
}

// sttsRun is one stts table entry: SampleCount samples share SampleDelta.
type sttsRun struct {
	SampleCount uint32
	SampleDelta uint32
}

// cttsRun is one ctts table entry. SampleOffset is signed; version 0 values
// are reinterpreted as unsigned by the parser before reaching this struct.
type cttsRun struct {
	SampleCount  uint32
	SampleOffset int64
}

// sampleTable is the raw metadata needed to resolve samples lazily.
type sampleTable struct {
	timescale uint32
	count     int
	uniform   uint32   // stsz SampleSize when non-zero
	sizes     []uint32 // nil when uniform != 0
	sizeBase  int64    // byte offset of the first stsz entry, -1 when uniform

	chunkOffsets      []uint64
	chunkOffsetBase   int64 // byte offset of the first chunk offset entry
	chunkOffsetWidth  int
	stsc              []stscRun
	chunkSamples      []uint32
	chunkFirstSample  []int
	chunkDescIndex    []uint32
	stts              []sttsRun
	sttsFirstSample   []int64
	sttsFirstTick     []int64
	ctts              []cttsRun
	cttsFirstSample   []int64
	cttsFirstOffset   []int64
	syncSet           map[uint32]bool
	fileSize          int64
	validateEntryBase int64

	cursorIndex  int
	cursorOffset int64
	cursorSize   int64
	cursorChunk  int
}

// buildSampleTable expands chunk mappings and validates table consistency,
// but does not touch essence bytes.
func buildSampleTable(st *sampleTable) error {
	if st.count < 0 {
		return media.NewError(media.ClassMalformed, "negative sample count")
	}
	if st.uniform == 0 && len(st.sizes) != st.count {
		return media.NewError(media.ClassMalformed,
			"stsz declares %d entries but %d samples were expected", len(st.sizes), st.count)
	}
	if st.count == 0 {
		return nil
	}
	if len(st.chunkOffsets) == 0 {
		return media.NewError(media.ClassMalformed, "sample table has no chunk offsets")
	}
	if len(st.stsc) == 0 {
		return media.NewError(media.ClassMalformed, "sample table has no stsc entries")
	}

	st.chunkSamples = make([]uint32, len(st.chunkOffsets))
	st.chunkDescIndex = make([]uint32, len(st.chunkOffsets))
	st.chunkFirstSample = make([]int, len(st.chunkOffsets))
	run := 0
	total := 0
	for c := 0; c < len(st.chunkOffsets); c++ {
		for run+1 < len(st.stsc) && int(st.stsc[run+1].FirstChunk) <= c+1 {
			run++
		}
		if int(st.stsc[run].FirstChunk) > c+1 {
			return media.NewError(media.ClassMalformed,
				"stsc does not cover chunk %d", c+1).WithOffset(st.stsc[run].EntryOffset)
		}
		if st.stsc[run].SamplesPerChunk == 0 || st.stsc[run].SamplesPerChunk > math.MaxInt32 {
			return media.NewError(media.ClassInvalidLength,
				"stsc declares an invalid samples-per-chunk value").WithOffset(st.stsc[run].EntryOffset)
		}
		st.chunkSamples[c] = st.stsc[run].SamplesPerChunk
		st.chunkDescIndex[c] = st.stsc[run].DescIndex
		st.chunkFirstSample[c] = total
		total += int(st.stsc[run].SamplesPerChunk)
		if total > st.count {
			return media.NewError(media.ClassMalformed,
				"chunk layout yields more samples than stsz declares").WithOffset(st.stsc[run].EntryOffset)
		}
	}
	if total != st.count {
		return media.NewError(media.ClassMalformed,
			"chunk layout yields %d samples but stsz declares %d", total, st.count)
	}

	st.sttsFirstSample = make([]int64, len(st.stts))
	st.sttsFirstTick = make([]int64, len(st.stts))
	var sc, tick int64
	for i, e := range st.stts {
		st.sttsFirstSample[i] = sc
		st.sttsFirstTick[i] = tick
		sc += int64(e.SampleCount)
		tick += int64(e.SampleCount) * int64(e.SampleDelta)
	}
	if sc != int64(st.count) {
		return media.NewError(media.ClassMalformed,
			"stts covers %d samples but stsz declares %d", sc, st.count)
	}

	if len(st.ctts) > 0 {
		st.cttsFirstSample = make([]int64, len(st.ctts))
		st.cttsFirstOffset = make([]int64, len(st.ctts))
		var cc int64
		for i, e := range st.ctts {
			st.cttsFirstSample[i] = cc
			st.cttsFirstOffset[i] = e.SampleOffset
			cc += int64(e.SampleCount)
		}
		if cc != int64(st.count) {
			return media.NewError(media.ClassMalformed,
				"ctts covers %d samples but stsz declares %d", cc, st.count)
		}
	}
	return nil
}

func (st *sampleTable) sizeAt(i int) (int64, error) {
	if st.uniform != 0 {
		return int64(st.uniform), nil
	}
	if i < 0 || i >= len(st.sizes) {
		return 0, media.NewError(media.ClassMalformed, "sample %d outside stsz", i)
	}
	return int64(st.sizes[i]), nil
}

// sampleOffset resolves the absolute byte offset of sample i. Sequential
// access is O(1) amortized; random access costs O(samples in chunk).
func (st *sampleTable) sampleOffset(i int) (int64, error) {
	if i < 0 || i >= st.count {
		return 0, media.NewError(media.ClassMalformed, "sample %d outside table", i)
	}
	if i == st.cursorIndex && i > 0 {
		return st.cursorOffset, nil
	}
	if i == st.cursorIndex+1 && st.cursorChunk >= 0 && st.cursorChunk < len(st.chunkSamples) &&
		st.chunkFirstSample[st.cursorChunk] <= i &&
		i < st.chunkFirstSample[st.cursorChunk]+int(st.chunkSamples[st.cursorChunk]) {
		off := st.cursorOffset + st.cursorSize
		st.cursorIndex, st.cursorOffset = i, off
		return off, nil
	}

	chunk := sort.Search(len(st.chunkFirstSample), func(k int) bool {
		return st.chunkFirstSample[k] > i
	}) - 1
	if chunk < 0 {
		return 0, media.NewError(media.ClassMalformed, "sample %d has no chunk", i)
	}
	sum := int64(0)
	for k := st.chunkFirstSample[chunk]; k < i; k++ {
		s, err := st.sizeAt(k)
		if err != nil {
			return 0, err
		}
		sum += s
	}
	off := int64(st.chunkOffsets[chunk]) + sum
	st.cursorChunk = chunk
	st.cursorIndex = i
	st.cursorOffset = off
	sz, err := st.sizeAt(i)
	if err != nil {
		return 0, err
	}
	st.cursorSize = sz
	return off, nil
}

// dtsTicks returns the exact DTS of sample i in timescale ticks.
func (st *sampleTable) dtsTicks(i int) (int64, uint32) {
	k := sort.Search(len(st.sttsFirstSample), func(j int) bool {
		return st.sttsFirstSample[j] > int64(i)
	}) - 1
	if k < 0 {
		return 0, 0
	}
	ticks := st.sttsFirstTick[k] + int64(i)*int64(st.stts[k].SampleDelta) -
		st.sttsFirstSample[k]*int64(st.stts[k].SampleDelta)
	return ticks, st.stts[k].SampleDelta
}

// cttsOffset returns the presentation offset in ticks, or 0.
func (st *sampleTable) cttsOffset(i int) int64 {
	if len(st.ctts) == 0 {
		return 0
	}
	k := sort.Search(len(st.cttsFirstSample), func(j int) bool {
		return st.cttsFirstSample[j] > int64(i)
	}) - 1
	if k < 0 {
		return 0
	}
	return st.cttsFirstOffset[k]
}

func (st *sampleTable) isSync(i int) bool {
	if st.syncSet == nil {
		return true
	}
	return st.syncSet[uint32(i+1)]
}

// At materializes one sample on demand.
func (st *sampleTable) At(i int) (media.Sample, error) {
	off, err := st.sampleOffset(i)
	if err != nil {
		return media.Sample{}, err
	}
	size, err := st.sizeAt(i)
	if err != nil {
		return media.Sample{}, err
	}
	dts, delta := st.dtsTicks(i)
	pts := dts + st.cttsOffset(i)
	return media.Sample{
		Seq:      uint32(i),
		DTS:      media.RatFromTicks(dts, st.timescale),
		PTS:      media.RatFromTicks(pts, st.timescale),
		Duration: media.RatFromTicks(int64(delta), st.timescale),
		Offset:   off,
		Size:     size,
		Sync:     st.isSync(i),
	}, nil
}

func (st *sampleTable) Count() int { return st.count }

// Validate walks every sample descriptor and reports the first illegal size
// or offset with the exact table byte offset that declared it. It reads no
// essence bytes, so it is safe for arbitrarily large inputs.
func (st *sampleTable) Validate() error {
	for i := 0; i < st.count; i++ {
		size, err := st.sizeAt(i)
		if err != nil {
			return err
		}
		if size <= 0 {
			return media.NewError(media.ClassInvalidLength,
				"sample %d declares non-positive length %d", i, size).
				WithOffset(st.sizeEntryOffset(i)).WithTolerance(0)
		}
		off, err := st.sampleOffset(i)
		if err != nil {
			return err
		}
		if off < 0 || off > st.fileSize || size > st.fileSize-off {
			return media.NewError(media.ClassInvalidLength,
				"sample %d range [%d,%d) exceeds input size %d", i, off, off+size, st.fileSize).
				WithOffset(st.sizeEntryOffset(i)).WithTolerance(0)
		}
	}
	return nil
}

func (st *sampleTable) sizeEntryOffset(i int) int64 {
	if st.sizeBase < 0 {
		return st.validateEntryBase
	}
	return st.sizeBase + int64(i)*4
}

// firstPayloadOffset returns the smallest sample offset, used for the input
// fingerprint.
func (st *sampleTable) firstPayloadOffset() int64 {
	best := int64(-1)
	for _, c := range st.chunkOffsets {
		if best < 0 || int64(c) < best {
			best = int64(c)
		}
	}
	return best
}

func (st *sampleTable) String() string {
	return fmt.Sprintf("samples=%d chunks=%d timescale=%d", st.count, len(st.chunkOffsets), st.timescale)
}
