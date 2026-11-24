package parse

import (
	"fmt"

	mp4 "github.com/abema/go-mp4"

	"mediarun/internal/model"
)

// parseSampleTables reads stts/stsz/stss/stsc/stco and materialises sample
// offsets, sizes, durations and sync flags.
func (t *Track) parseSampleTables(stbl []RawBox, file []byte, mdatStart int64) error {
	sttsRaw := FindBox(stbl, "stts")
	stszRaw := FindBox(stbl, "stsz")
	if sttsRaw == nil || stszRaw == nil {
		return model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: track %d missing stts/stsz", model.ErrTruncated, t.ID))
	}
	var stts mp4.Stts
	if err := unmarshal(sttsRaw, &stts); err != nil {
		return err
	}
	var stsz mp4.Stsz
	if err := unmarshal(stszRaw, &stsz); err != nil {
		return err
	}
	count := int(stsz.SampleCount)
	if count <= 0 {
		return nil
	}
	if count > MaxSamples {
		return model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: track %d has %d samples", model.ErrLimit, t.ID, count))
	}
	durations, err := expandDurations(stts, count)
	if err != nil {
		return err
	}
	sizes := make([]uint32, count)
	if stsz.SampleSize != 0 {
		for i := range sizes {
			sizes[i] = stsz.SampleSize
		}
	} else {
		if len(stsz.EntrySize) != count {
			return model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: track %d stsz has %d sizes for %d samples",
					model.ErrBadLength, t.ID, len(stsz.EntrySize), count))
		}
		copy(sizes, stsz.EntrySize)
	}
	offsets, err := t.sampleOffsets(stbl, sizes)
	if err != nil {
		return err
	}

	syncSet := make(map[uint32]bool)
	if stssRaw := FindBox(stbl, "stss"); stssRaw != nil {
		var stss mp4.Stss
		if err := unmarshal(stssRaw, &stss); err != nil {
			return err
		}
		for _, n := range stss.SampleNumber {
			if n == 0 || n > uint32(count) {
				return model.Classify(model.ClassCorrupt,
					fmt.Errorf("%w: track %d stss references sample %d of %d",
						model.ErrBadLength, t.ID, n, count))
			}
			syncSet[n] = true
			t.Keyframes = append(t.Keyframes, n)
		}
	} else {
		// No stss means every sample is a sync sample.
		for n := 1; n <= count; n++ {
			syncSet[uint32(n)] = true
			t.Keyframes = append(t.Keyframes, uint32(n))
		}
	}

	var dts uint64
	t.Samples = make([]Sample, count)
	for i := 0; i < count; i++ {
		idx := i + 1
		off := offsets[i]
		s := Sample{
			Index:    idx,
			Offset:   off,
			Size:     sizes[i],
			Duration: durations[i],
			DTS:      dts,
			Sync:     syncSet[uint32(idx)],
		}
		if sizes[i] > MaxSampleBytes {
			return model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: track %d sample %d size %d", model.ErrLimit, t.ID, idx, sizes[i]))
		}
		end := off + int64(sizes[i])
		if off < mdatStart || end > int64(len(file)) {
			return model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: track %d sample %d range [%d,%d) outside mdat",
					model.ErrTruncated, t.ID, idx, off, end))
		}
		s.Data = file[off:end]
		t.Samples[i] = s
		dts += uint64(durations[i])
	}
	return nil
}

// expandDurations flattens the run-length stts table to one entry per
// sample, rejecting tables that do not cover every sample exactly.
func expandDurations(stts mp4.Stts, count int) ([]uint32, error) {
	out := make([]uint32, 0, count)
	for _, e := range stts.Entries {
		if len(out)+int(e.SampleCount) > count {
			return nil, model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: stts covers more than %d samples", model.ErrBadLength, count))
		}
		for i := uint32(0); i < e.SampleCount; i++ {
			out = append(out, e.SampleDelta)
		}
	}
	if len(out) != count {
		return nil, model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: stts covers %d of %d samples", model.ErrBadLength, len(out), count))
	}
	return out, nil
}
