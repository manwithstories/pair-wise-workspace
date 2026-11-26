package mp4

import (
	"encoding/json"
	"fmt"

	gomp4 "github.com/abema/go-mp4"

	"mediad/internal/media"
)

func buildStbl(t *media.Track) ([]byte, error) {
	stsd := fullBoxBytes("stsd", 0, 0, appendU32(nil, 0))

	stts, err := buildStts(t.Samples)
	if err != nil {
		return nil, err
	}
	children := [][]byte{stsd, stts}

	if ctts, ok, err := buildCtts(t.Samples); err != nil {
		return nil, err
	} else if ok {
		children = append(children, ctts)
	}
	if len(t.SyncSampleNumbers()) > 0 {
		stss, err := marshalTypedBox(&gomp4.Stss{EntryCount: uint32(len(t.SyncSampleNumbers())), SampleNumber: t.SyncSampleNumbers()})
		if err != nil {
			return nil, err
		}
		children = append(children, stss)
	}

	stscPayload := appendU32(nil, 1)
	stscPayload = appendU32(stscPayload, 1) // first_chunk
	stscPayload = appendU32(stscPayload, 1) // samples_per_chunk
	stscPayload = appendU32(stscPayload, 1) // sample_description_index
	stsc := fullBoxBytes("stsc", 0, 0, stscPayload)
	children = append(children, stsc)

	sizes := make([]uint32, len(t.Samples))
	offsets := make([]uint32, len(t.Samples))
	for i, s := range t.Samples {
		sizes[i] = s.Size
		if s.Offset < 0 || s.Offset > int64(^uint32(0)) {
			return nil, media.NewFault(media.CodeBadSampleOffset, "offset %d cannot fit stco", s.Offset).
				WithTrack(t.ID).WithBox("stco").Fatal()
		}
		offsets[i] = uint32(s.Offset)
	}
	stsz, err := marshalTypedBox(&gomp4.Stsz{SampleSize: 0, SampleCount: uint32(len(sizes)), EntrySize: sizes})
	if err != nil {
		return nil, err
	}
	stco, err := marshalTypedBox(&gomp4.Stco{EntryCount: uint32(len(offsets)), ChunkOffset: offsets})
	if err != nil {
		return nil, err
	}
	children = append(children, stsz, stco)
	return containerBytes("stbl", children...), nil
}

func buildStts(samples []media.Sample) ([]byte, error) {
	var entries []gomp4.SttsEntry
	for _, s := range samples {
		if s.Duration == 0 {
			return nil, media.NewFault(media.CodeTimestampMissing, "sample %d has zero duration", s.Index).Fatal()
		}
		if len(entries) > 0 && entries[len(entries)-1].SampleDelta == s.Duration && entries[len(entries)-1].SampleCount < ^uint32(0) {
			entries[len(entries)-1].SampleCount++
			continue
		}
		entries = append(entries, gomp4.SttsEntry{SampleCount: 1, SampleDelta: s.Duration})
	}
	return marshalTypedBox(&gomp4.Stts{EntryCount: uint32(len(entries)), Entries: entries})
}

func buildCtts(samples []media.Sample) ([]byte, bool, error) {
	any := false
	negative := false
	for _, s := range samples {
		off := s.PTS - s.DTS
		if off != 0 {
			any = true
		}
		if off < 0 {
			negative = true
		}
	}
	if !any {
		return nil, false, nil
	}
	box := &gomp4.Ctts{EntryCount: uint32(len(samples))}
	if negative {
		box.Version = 1
		box.Entries = make([]gomp4.CttsEntry, len(samples))
		for i, s := range samples {
			off := s.PTS - s.DTS
			if off < mathMinInt32 || off > mathMaxInt32 {
				return nil, false, media.NewFault(media.CodeTimestampWrap, "ctts offset %d exceeds int32", off).Fatal()
			}
			box.Entries[i] = gomp4.CttsEntry{SampleCount: 1, SampleOffsetV1: int32(off)}
		}
	} else {
		box.Entries = make([]gomp4.CttsEntry, len(samples))
		for i, s := range samples {
			off := s.PTS - s.DTS
			if off < 0 || off > mathMaxInt32 {
				return nil, false, fmt.Errorf("invalid unsigned ctts offset %d", off)
			}
			box.Entries[i] = gomp4.CttsEntry{SampleCount: 1, SampleOffsetV0: uint32(off)}
		}
	}
	out, err := marshalTypedBox(box)
	return out, true, err
}

func buildAudioAudit(a *media.AudioMetadata) []byte {
	data, _ := json.Marshal(a) // deterministic struct order; no maps
	return boxBytes("audt", data)
}

func buildColorAudit(c *media.ColorMetadata) []byte {
	data, _ := json.Marshal(c)
	return boxBytes("vcol", data)
}
