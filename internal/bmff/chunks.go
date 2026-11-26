package bmff

import (
	"mediad/internal/model"
)

type stscEntry struct {
	FirstChunk        uint32
	SamplesPerChunk   uint32
	SampleDescription uint32
}

func (s *Reader) parseSTSC(stbl *BoxHeader) ([]stscEntry, error) {
	b, err := s.MustChild(stbl, "stsc")
	if err != nil {
		return nil, err
	}
	p, err := s.ReadPayload(b)
	if err != nil {
		return nil, err
	}
	if len(p) < 8 {
		return nil, corruptBox(s.Source, b, "short stsc")
	}
	count := u32(p, 4)
	if int(count)*12+8 > len(p) {
		return nil, corruptBox(s.Source, b, "truncated stsc entries")
	}
	out := make([]stscEntry, 0, count)
	pos := 8
	for i := uint32(0); i < count; i++ {
		out = append(out, stscEntry{
			FirstChunk:        u32(p, pos),
			SamplesPerChunk:   u32(p, pos+4),
			SampleDescription: u32(p, pos+8),
		})
		pos += 12
	}
	if len(out) == 0 {
		return nil, corruptBox(s.Source, b, "stsc has no entries")
	}
	return out, nil
}

func (s *Reader) parseChunkOffsets(stbl *BoxHeader) ([]int64, error) {
	if b, err := s.FirstChild(stbl, "stco"); err != nil {
		return nil, err
	} else if b != nil {
		p, err := s.ReadPayload(b)
		if err != nil {
			return nil, err
		}
		if len(p) < 8 {
			return nil, corruptBox(s.Source, b, "short stco")
		}
		count := u32(p, 4)
		if int(count)*4+8 > len(p) {
			return nil, corruptBox(s.Source, b, "truncated stco entries")
		}
		out := make([]int64, count)
		pos := 8
		for i := range out {
			out[i] = int64(u32(p, pos))
			pos += 4
		}
		return out, nil
	}
	b, err := s.MustChild(stbl, "co64")
	if err != nil {
		return nil, err
	}
	p, err := s.ReadPayload(b)
	if err != nil {
		return nil, err
	}
	if len(p) < 8 {
		return nil, corruptBox(s.Source, b, "short co64")
	}
	count := u32(p, 4)
	if int(count)*8+8 > len(p) {
		return nil, corruptBox(s.Source, b, "truncated co64 entries")
	}
	out := make([]int64, count)
	pos := 8
	for i := range out {
		out[i] = int64(u64(p, pos))
		pos += 8
	}
	return out, nil
}

func (s *Reader) parseSTSS(stbl *BoxHeader) (map[uint32]struct{}, error) {
	b, err := s.FirstChild(stbl, "stss")
	if err != nil || b == nil {
		return nil, err
	}
	p, err := s.ReadPayload(b)
	if err != nil {
		return nil, err
	}
	if len(p) < 8 {
		return nil, corruptBox(s.Source, b, "short stss")
	}
	count := u32(p, 4)
	if int(count)*4+8 > len(p) {
		return nil, corruptBox(s.Source, b, "truncated stss entries")
	}
	out := make(map[uint32]struct{}, count)
	pos := 8
	for i := uint32(0); i < count; i++ {
		out[u32(p, pos)] = struct{}{}
		pos += 4
	}
	return out, nil
}

func (s *Reader) assignSampleOffsets(t *Track, sizes []uint32, chunks []int64, entries []stscEntry) ([]int64, error) {
	if len(sizes) == 0 {
		return nil, nil
	}
	if len(chunks) == 0 {
		return nil, model.New(model.CodeCorruptBox, model.StageTables, "missing chunk offsets for samples").
			WithSource(s.Source).WithTrack(t.ID).WithBox("stco")
	}
	offsets := make([]int64, len(sizes))
	entryIdx := 0
	sampleIdx := 0
	for chunk := 1; chunk <= len(chunks); chunk++ {
		for entryIdx+1 < len(entries) && int(entries[entryIdx+1].FirstChunk) <= chunk {
			entryIdx++
		}
		n := int(entries[entryIdx].SamplesPerChunk)
		off := chunks[chunk-1]
		for j := 0; j < n; j++ {
			if sampleIdx >= len(sizes) {
				return nil, model.New(model.CodeCorruptBox, model.StageTables, "stsc assigns more samples than stsz").
					WithSource(s.Source).WithTrack(t.ID).WithBox("stsc")
			}
			end := off + int64(sizes[sampleIdx])
			if off < 0 || end > s.Size {
				return nil, model.Newf(model.CodeCorruptBox, model.StageTables,
					"sample %d range %d-%d exceeds file size %d", sampleIdx, off, end, s.Size).
					WithSource(s.Source).WithTrack(t.ID).WithBox("stco").WithOffset(off)
			}
			offsets[sampleIdx] = off
			off = end
			sampleIdx++
		}
	}
	if sampleIdx != len(sizes) {
		return nil, model.Newf(model.CodeCorruptBox, model.StageTables,
			"stsc covered %d samples but stsz has %d", sampleIdx, len(sizes)).
			WithSource(s.Source).WithTrack(t.ID).WithBox("stsc")
	}
	return offsets, nil
}
