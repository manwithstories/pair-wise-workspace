package parse

import (
	"fmt"
	"math"

	mp4 "github.com/abema/go-mp4"

	"mediarun/internal/model"
)

func writeStbl(t Track, offsets []int64) (*box, error) {
	stsdBody, err := marshalBody(&mp4.Stsd{EntryCount: 1})
	if err != nil {
		return nil, err
	}
	stsd := payloadBox("stsd", stsdBody).withChildren(rawToBox(t.Entry))

	sttsEntries := []mp4.SttsEntry{}
	for _, s := range t.Samples {
		if n := len(sttsEntries); n > 0 && sttsEntries[n-1].SampleDelta == s.Duration {
			sttsEntries[n-1].SampleCount++
			continue
		}
		sttsEntries = append(sttsEntries, mp4.SttsEntry{SampleCount: 1, SampleDelta: s.Duration})
	}
	sttsBody, err := marshalBody(&mp4.Stts{EntryCount: uint32(len(sttsEntries)), Entries: sttsEntries})
	if err != nil {
		return nil, err
	}
	children := []*box{stsd, leaf("stts", sttsBody)}

	allSync := true
	numbers := make([]uint32, 0, len(t.Samples))
	for i, s := range t.Samples {
		if s.Sync {
			numbers = append(numbers, uint32(i+1))
		} else {
			allSync = false
		}
	}
	if !allSync {
		stssBody, err := marshalBody(&mp4.Stss{EntryCount: uint32(len(numbers)), SampleNumber: numbers})
		if err != nil {
			return nil, err
		}
		children = append(children, leaf("stss", stssBody))
	}

	sizes := make([]uint32, len(t.Samples))
	for i, s := range t.Samples {
		sizes[i] = uint32(len(s.Data))
	}
	stszBody, err := marshalBody(&mp4.Stsz{SampleCount: uint32(len(sizes)), EntrySize: sizes})
	if err != nil {
		return nil, err
	}
	children = append(children, leaf("stsz", stszBody))

	stscEntries := []mp4.StscEntry{}
	if len(t.Samples) > 0 {
		stscEntries = append(stscEntries, mp4.StscEntry{FirstChunk: 1, SamplesPerChunk: 1, SampleDescriptionIndex: 1})
	}
	stscBody, err := marshalBody(&mp4.Stsc{EntryCount: uint32(len(stscEntries)), Entries: stscEntries})
	if err != nil {
		return nil, err
	}
	children = append(children, leaf("stsc", stscBody))

	values := make([]uint32, len(offsets))
	for i, v := range offsets {
		if v < 0 || v > math.MaxUint32 {
			return nil, fmt.Errorf("%w: chunk offset %d", model.ErrLimit, v)
		}
		values[i] = uint32(v)
	}
	stcoBody, err := marshalBody(&mp4.Stco{EntryCount: uint32(len(values)), ChunkOffset: values})
	if err != nil {
		return nil, err
	}
	children = append(children, leaf("stco", stcoBody))
	return container("stbl", children...), nil
}
