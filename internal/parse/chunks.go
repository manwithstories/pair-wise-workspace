package parse

import (
	"fmt"

	mp4 "github.com/abema/go-mp4"

	"mediarun/internal/model"
)

// sampleOffsets resolves chunk offsets into absolute sample offsets.
func (t *Track) sampleOffsets(stbl []RawBox, sizes []uint32) ([]int64, error) {
	stscRaw := FindBox(stbl, "stsc")
	if stscRaw == nil {
		return nil, model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: track %d missing stsc", model.ErrTruncated, t.ID))
	}
	var stsc mp4.Stsc
	if err := unmarshal(stscRaw, &stsc); err != nil {
		return nil, err
	}
	chunkOffsets, err := t.chunkOffsets(stbl)
	if err != nil {
		return nil, err
	}
	offsets := make([]int64, 0, len(sizes))
	sample := 0
	for i, entry := range stsc.Entries {
		if entry.SamplesPerChunk == 0 {
			return nil, model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: track %d stsc entry %d has zero samples", model.ErrBadLength, t.ID, i))
		}
		first := int(entry.FirstChunk)
		if first < 1 || first > len(chunkOffsets) {
			return nil, model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: track %d stsc references chunk %d of %d",
					model.ErrBadLength, t.ID, first, len(chunkOffsets)))
		}
		last := len(chunkOffsets)
		if i+1 < len(stsc.Entries) {
			last = int(stsc.Entries[i+1].FirstChunk) - 1
			if last < first || last > len(chunkOffsets) {
				return nil, model.Classify(model.ClassCorrupt,
					fmt.Errorf("%w: track %d stsc chunk range %d..%d invalid", model.ErrBadLength, t.ID, first, last))
			}
		}
		for chunk := first; chunk <= last; chunk++ {
			pos := chunkOffsets[chunk-1]
			if pos < 0 {
				return nil, model.Classify(model.ClassCorrupt,
					fmt.Errorf("%w: track %d chunk %d has bogus offset", model.ErrBadLength, t.ID, chunk))
			}
			for s := uint32(0); s < entry.SamplesPerChunk; s++ {
				if sample >= len(sizes) {
					return offsets, nil
				}
				offsets = append(offsets, pos)
				pos += int64(sizes[sample])
				sample++
			}
		}
	}
	if sample != len(sizes) {
		return nil, model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: track %d stsc placed %d of %d samples", model.ErrBadLength, t.ID, sample, len(sizes)))
	}
	return offsets, nil
}

func (t *Track) chunkOffsets(stbl []RawBox) ([]int64, error) {
	if raw := FindBox(stbl, "stco"); raw != nil {
		var stco mp4.Stco
		if err := unmarshal(raw, &stco); err != nil {
			return nil, err
		}
		if uint32(len(stco.ChunkOffset)) != stco.EntryCount {
			return nil, model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: track %d stco declares %d chunks, found %d",
					model.ErrBadLength, t.ID, stco.EntryCount, len(stco.ChunkOffset)))
		}
		out := make([]int64, len(stco.ChunkOffset))
		for i, v := range stco.ChunkOffset {
			out[i] = int64(v)
		}
		return out, nil
	}
	if raw := FindBox(stbl, "co64"); raw != nil {
		var co64 mp4.Co64
		if err := unmarshal(raw, &co64); err != nil {
			return nil, err
		}
		if uint32(len(co64.ChunkOffset)) != co64.EntryCount {
			return nil, model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: track %d co64 declares %d chunks, found %d",
					model.ErrBadLength, t.ID, co64.EntryCount, len(co64.ChunkOffset)))
		}
		out := make([]int64, len(co64.ChunkOffset))
		for i, v := range co64.ChunkOffset {
			if v > uint64(1<<62) {
				return nil, model.Classify(model.ClassCorrupt,
					fmt.Errorf("%w: track %d co64 offset overflow", model.ErrBadLength, t.ID))
			}
			out[i] = int64(v)
		}
		return out, nil
	}
	return nil, model.Classify(model.ClassCorrupt,
		fmt.Errorf("%w: track %d missing stco/co64", model.ErrTruncated, t.ID))
}
