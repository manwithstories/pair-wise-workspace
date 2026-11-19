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
